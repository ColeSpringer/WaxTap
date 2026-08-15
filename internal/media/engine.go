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
		engine: waxflow.New(waxflow.WithLogger(slog.New(demoteImplicitDownmix{log.Handler()}))),
		sem:    sem,
		log:    log,
	}
}

// implicitDownmixMsg is WaxFlow's log record for a channel fold the caller did
// not ask for.
//
// keep in sync with waxflow.go logImplicitDownmix
const implicitDownmixMsg = "downmixed to fit the output format"

// demoteImplicitDownmix lowers that one WaxFlow record from WARN to DEBUG, so it
// does not duplicate the implicit-downmix warning WaxTap raises from its own
// probes, and still shows up under --verbose.
//
// Narrow on purpose: demoting every WaxFlow WARN would silence the upstream
// warnings WaxTap has *not* learned to detect, which is the same defect one
// release later. The coupling to the message string fails benignly too: if
// upstream rewords the record, the raw line comes back as a cosmetic duplicate
// rather than a lost signal, because the typed warning does not depend on it.
type demoteImplicitDownmix struct{ slog.Handler }

func (h demoteImplicitDownmix) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && r.Message == implicitDownmixMsg {
		// Handle is called without re-checking Enabled, so the level test that the
		// demotion implies has to happen here or the record prints anyway.
		if !h.Handler.Enabled(ctx, slog.LevelDebug) {
			return nil
		}
		r.Level = slog.LevelDebug
	}
	return h.Handler.Handle(ctx, r)
}

func (h demoteImplicitDownmix) WithAttrs(attrs []slog.Attr) slog.Handler {
	return demoteImplicitDownmix{h.Handler.WithAttrs(attrs)}
}

func (h demoteImplicitDownmix) WithGroup(name string) slog.Handler {
	return demoteImplicitDownmix{h.Handler.WithGroup(name)}
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
