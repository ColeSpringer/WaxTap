// Package media is WaxTap's in-process audio engine: it probes, transcodes,
// remuxes, and cuts local audio files by calling the pure-Go WaxFlow library
// rather than shelling out to ffmpeg.
//
// Callers pass file paths; the package opens them as WaxFlow sources, bounds the
// number of concurrent operations, and stages output atomically next to the
// destination. A [Codec] selects the target format; [Spec] adds an optional
// bitrate, a downmix, and a normalization gain, all fused into one WaxFlow pass.
// [CodecCopy] is the no-re-encode mode, served by a container-level remux.
package media

import (
	"context"
	"log/slog"
	"os"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
)

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	// MaxProcs bounds concurrent WaxFlow operations (0 = unlimited). Each
	// operation runs one goroutine, so this maps operation count onto cores and
	// caps peak memory from pooled decode/DSP/encode buffers.
	MaxProcs int
	// Logger receives debug logs. Nil discards them.
	Logger *slog.Logger
}

// Runner drives WaxFlow's engine for local audio files. It bounds concurrency,
// and it is safe for concurrent use.
type Runner struct {
	engine *waxflow.Engine
	sem    chan struct{}
	log    *slog.Logger
}

// NewRunner builds a Runner. WaxFlow's engine construction cannot fail, so there
// is no error to return.
func NewRunner(cfg RunnerConfig) *Runner {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var sem chan struct{}
	if cfg.MaxProcs > 0 {
		sem = make(chan struct{}, cfg.MaxProcs)
	}
	return &Runner{
		engine: waxflow.New(waxflow.WithLogger(log)),
		sem:    sem,
		log:    log,
	}
}

// Engine returns the underlying WaxFlow engine, for callers (loudness) that
// measure through it directly.
func (r *Runner) Engine() *waxflow.Engine { return r.engine }

// OutputFormats lists the audio formats the in-process engine can produce. The
// doctor command reports it as WaxTap's capability set.
func OutputFormats() []string { return waxflow.OutputFormats() }

func (r *Runner) acquire(ctx context.Context) error {
	if r.sem == nil {
		return nil
	}
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) release() {
	if r.sem != nil {
		<-r.sem
	}
}

// openSource opens path as a WaxFlow source. The returned closer closes the
// underlying file and must be called once the operation finishes.
func openSource(path string) (container.Source, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	src, err := container.FileSource(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return src, f.Close, nil
}
