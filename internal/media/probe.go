package media

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// ProbeResult is a probe of a local audio file: container metadata plus the
// audio track's codec details.
type ProbeResult struct {
	Format  ProbeFormat   // container metadata
	Streams []ProbeStream // audio tracks
}

// ProbeFormat describes the container.
type ProbeFormat struct {
	Container string        // identified container name ("webm", "mka", "flac", ...)
	Duration  time.Duration // container duration, or 0 when unknown
	Size      int64         // bytes, or 0 when unknown
	// BitRate is always 0: WaxFlow does not report a container bit rate, so
	// callers fall back to a size/duration estimate (mapping.applyProbe).
	BitRate int
}

// ProbeStream describes one audio track.
type ProbeStream struct {
	CodecType  string        // always "audio"
	CodecName  string        // codecName-mapped: "opus", "aac", "flac", "pcm", ...
	SampleRate int           // Hz
	Channels   int           // channel count
	BitRate    int           // always 0 (WaxFlow reports none)
	Duration   time.Duration // track duration, or 0 when unknown
}

// AudioStream returns the first audio track and true, or a zero stream and false
// when the result carries none. Probe rejects no-audio inputs, so a result
// obtained from Probe always has one.
func (p ProbeResult) AudioStream() (ProbeStream, bool) {
	for _, s := range p.Streams {
		if s.CodecType == "audio" {
			return s, true
		}
	}
	return ProbeStream{}, false
}

// hintFor returns WaxFlow's container hint for a path: its extension without the
// leading dot, or "" to let WaxFlow sniff the container.
func hintFor(path string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
}

// Probe inspects a local input and returns the parsed result. Undecodable media
// and media with no audio track are reported as waxerr.ErrUnsupportedInput.
func (r *Runner) Probe(ctx context.Context, input string) (ProbeResult, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return ProbeResult{}, err
	}
	defer closeSrc()
	return r.probeSource(ctx, src, hintFor(input))
}

func (r *Runner) probeSource(ctx context.Context, src container.Source, hint string) (ProbeResult, error) {
	if err := r.acquire(ctx); err != nil {
		return ProbeResult{}, err
	}
	defer r.release()

	info, err := r.engine.Probe(src, hint, nil)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ProbeResult{}, ctxErr
		}
		return ProbeResult{}, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	return mapProbe(info, src.Size()), nil
}

// mapProbe converts a WaxFlow probe into a ProbeResult. It never errors; the
// caller (probeSource) has already ensured the input decoded.
//
// The default track is reported first (so AudioStream describes it) and its
// duration is Format.Duration: cuts and measurements run against the default
// track, so a file whose default audio is not its longest track must not report
// a longer duration than the track a cut will actually address.
func mapProbe(info *format.Info, size int64) ProbeResult {
	pr := ProbeResult{Format: ProbeFormat{Container: info.Container, Size: size}}
	stream := func(t container.Track) ProbeStream {
		return ProbeStream{
			CodecType:  "audio",
			CodecName:  codecName(t.Codec),
			SampleRate: t.Fmt.Rate,
			Channels:   t.Fmt.Channels,
			Duration:   trackDuration(t.Samples, t.Fmt.Rate),
		}
	}
	if len(info.Tracks) == 0 {
		return pr
	}
	def := info.Default()
	pr.Streams = append(pr.Streams, stream(def))
	pr.Format.Duration = trackDuration(def.Samples, def.Fmt.Rate)
	for _, t := range info.Tracks {
		if t.ID == def.ID {
			continue
		}
		pr.Streams = append(pr.Streams, stream(t))
	}
	return pr
}

// trackDuration converts a sample count and rate to a duration. Samples is -1
// when unknown (raw ADTS), yielding 0.
func trackDuration(samples int64, rate int) time.Duration {
	if samples <= 0 || rate <= 0 {
		return 0
	}
	return time.Duration(float64(samples) / float64(rate) * float64(time.Second))
}

// probeAudio probes and requires a decodable audio track, applying the same
// classification the old ffprobe boundary used.
func (r *Runner) probeAudio(ctx context.Context, input string) (ProbeResult, ProbeStream, error) {
	pr, err := r.Probe(ctx, input)
	if err != nil {
		return ProbeResult{}, ProbeStream{}, err
	}
	a, ok := pr.AudioStream()
	if !ok {
		return ProbeResult{}, ProbeStream{}, fmt.Errorf("%w: no audio stream", waxerr.ErrUnsupportedInput)
	}
	if a.Channels == 0 && a.SampleRate == 0 {
		return ProbeResult{}, ProbeStream{}, fmt.Errorf("%w: no decodable audio", waxerr.ErrUnsupportedInput)
	}
	return pr, a, nil
}
