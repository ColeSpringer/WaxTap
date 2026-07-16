package media

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// Mode selects how a cut is rendered. The facade maps waxtap.CutMode onto these.
type Mode uint8

const (
	// ModeSmart copies (lossless cut-remux) when the cut keeps the source codec,
	// and re-encodes when a transcode, downmix, gain, or crossfade is involved.
	ModeSmart Mode = iota
	// ModeCopy forces a lossless cut-remux. It cannot transcode, downmix, or
	// crossfade, and it fails when WaxFlow cannot cut-remux the source codec.
	ModeCopy
	// ModeAccurate forces a re-encode.
	ModeAccurate
)

func (m Mode) String() string {
	switch m {
	case ModeCopy:
		return "copy"
	case ModeAccurate:
		return "accurate"
	default:
		return "smart"
	}
}

// CutSpec describes a resolved cut. Keeps are the spans to retain, in order, on
// the source timeline; Total is the source duration. CopyCut asks for a lossless
// cut-remux (kept codec, byte-identical packets), which WaxTap tries first and
// falls back from to Encode when WaxFlow declines the source codec. Encode names
// the re-encode used for the re-encode path (or the CopyCut fallback).
type CutSpec struct {
	Keeps     []cutrange.Range
	Total     time.Duration
	Crossfade time.Duration
	CopyCut   bool
	// RequireCopy fails the cut rather than re-encoding when WaxFlow declines the
	// lossless cut-remux. It expresses an explicit copy request (--format copy or
	// --cut-mode copy): silently re-encoding would break the caller's promise.
	RequireCopy bool
	Encode      Spec
}

// CutResult reports a completed cut.
type CutResult struct {
	Output  string
	Removed time.Duration
	Mode    Mode
	Applied bool
}

// Render applies spec's cut to input and writes the result to output. Output is
// staged and atomically renamed on success. When CopyCut is set and no downmix,
// gain, or crossfade is requested, Render tries a lossless cut-remux first and
// re-encodes only if WaxFlow declines the source codec.
func (r *Runner) Render(ctx context.Context, input, output string, spec CutSpec) (CutResult, error) {
	if len(spec.Keeps) == 0 {
		return CutResult{}, fmt.Errorf("%w: cut would remove the entire track", waxerr.ErrIncompatibleSpec)
	}

	src, closeSrc, err := openSource(input)
	if err != nil {
		return CutResult{}, err
	}
	defer closeSrc()

	if err := r.acquire(ctx); err != nil {
		return CutResult{}, err
	}
	defer r.release()

	hint := hintFor(input)
	outExt := hintFor(output)

	staged, err := tempfile.New(output)
	if err != nil {
		return CutResult{}, err
	}
	defer staged.Discard()

	// A lossless cut-remux applies when the caller wants the source codec kept and
	// nothing forces a decode (no downmix, gain, or crossfade).
	tryRemux := spec.CopyCut && spec.Crossfade == 0 && spec.Encode.Channels == 0 && spec.Encode.GainDB == 0
	mode := Mode(ModeAccurate)
	if tryRemux {
		done, rerr := r.cutRemux(ctx, src, hint, outExt, spec.Keeps, spec.Total, staged)
		if rerr != nil {
			return CutResult{}, rerr
		}
		if done {
			mode = ModeCopy
		} else {
			// WaxFlow declined a lossless cut-remux of the source codec (e.g. FLAC).
			if spec.RequireCopy {
				return CutResult{}, fmt.Errorf("%w: cannot losslessly copy-cut this source codec (only Opus and AAC support a packet-level cut); drop --format copy / --cut-mode copy to re-encode, which stays lossless for a lossless source", waxerr.ErrIncompatibleSpec)
			}
			// Fall through to a re-encode, which stays lossless for a lossless source.
			if err := r.cutReencode(ctx, src, hint, outExt, spec, staged); err != nil {
				return CutResult{}, err
			}
		}
	} else {
		if err := r.cutReencode(ctx, src, hint, outExt, spec, staged); err != nil {
			return CutResult{}, err
		}
	}

	if err := staged.Commit(); err != nil {
		return CutResult{}, err
	}
	return CutResult{
		Output:  output,
		Removed: spec.Total - cutrange.OutputDuration(spec.Keeps, spec.Crossfade),
		Mode:    mode,
		Applied: true,
	}, nil
}

// cutRemux performs the lossless packet-level cut-remux. It reports done=false
// (and no error) when WaxFlow declines the source codec, so the caller re-encodes
// instead.
func (r *Runner) cutRemux(ctx context.Context, src container.Source, hint, outExt string, keeps []cutrange.Range, total time.Duration, dst *tempfile.File) (done bool, err error) {
	grid, err := r.engine.PacketGrid(src, hint)
	if err != nil {
		return false, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return false, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	outFormat, ok := codecToFormat(track.Codec)
	if !ok {
		return false, nil // unknown codec: let the re-encode path handle it
	}
	spans := toSpans(keeps, total, track.Fmt.Rate)
	opts := waxflow.TranscodeOptions{Format: outFormat, Container: containerFor(outFormat, outExt)}

	plan, err := r.engine.PlanCut(track, opts, spans, grid)
	if err != nil {
		return false, err
	}
	if plan == nil {
		return false, nil // declined (e.g. FLAC): fall back to a re-encode
	}
	cutTrack, _, err := waxflow.CutTrack(track, spans, grid)
	if err != nil {
		return false, err
	}
	cutDemux, err := waxflow.Cut(demux, track, spans, grid)
	if err != nil {
		return false, err
	}
	// NB: pass CutTrack's track, not plan.Track.
	if _, err := r.engine.RemuxDemuxer(ctx, cutDemux, cutTrack, dst, opts); err != nil {
		return false, err
	}
	return true, nil
}

// cutReencode renders the cut by decoding: it slices the kept spans, concatenates
// them (with an optional crossfade), and re-encodes with spec.Encode.
func (r *Runner) cutReencode(ctx context.Context, src container.Source, hint, outExt string, spec CutSpec, dst *tempfile.File) error {
	med, err := r.openComposed(src, hint, spec.Keeps, spec.Total, spec.Crossfade)
	if err != nil {
		return err
	}
	defer med.Close()

	opts := encodeOptions(spec.Encode)
	format, _ := codecFormat(spec.Encode.Codec)
	opts.Container = containerFor(format, outExt)
	_, err = r.engine.TranscodeMedia(ctx, med, dst, opts)
	return err
}

// openComposed builds the WaxFlow Media for the kept spans: a single Slice for
// one span, or a Concat of per-span slices (with an optional crossfade) for
// several. The caller closes the returned Media.
//
// It probes the source's rate to convert the time-domain keeps to sample spans.
func (r *Runner) openComposed(src container.Source, hint string, keeps []cutrange.Range, total time.Duration, crossfade time.Duration) (format.Media, error) {
	_, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	rate := track.Fmt.Rate

	if len(keeps) == 1 {
		from, to := sampleBounds(keeps[0], total, rate, track.Samples)
		med, err := format.Open(src, hint, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
		}
		sl, err := waxflow.Slice(med, from, to)
		if err != nil {
			med.Close()
			return nil, err
		}
		return sl, nil
	}

	members := make([]waxflow.ConcatSource, len(keeps))
	for i, k := range keeps {
		from, to := sampleBounds(k, total, rate, track.Samples)
		st, err := waxflow.SpanTrack(track, from, to)
		if err != nil {
			return nil, err
		}
		members[i] = waxflow.ConcatSource{Track: st, Open: func() (format.Media, error) {
			m, err := format.Open(src, hint, nil)
			if err != nil {
				return nil, err
			}
			return waxflow.Slice(m, from, to)
		}}
	}
	xfade := int64(0)
	if crossfade > 0 {
		xfade = samplesOf(crossfade, rate)
	}
	return waxflow.Concat(members, waxflow.ConcatOptions{Crossfade: xfade})
}

// OpenComposed opens the cut-composed Media for measurement (loudness), without
// re-encoding. The caller closes it.
func (r *Runner) OpenComposed(input string, keeps []cutrange.Range, total, crossfade time.Duration) (format.Media, func() error, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return nil, nil, err
	}
	med, err := r.openComposed(src, hintFor(input), keeps, total, crossfade)
	if err != nil {
		_ = closeSrc()
		return nil, nil, err
	}
	closer := func() error {
		cerr := med.Close()
		if serr := closeSrc(); cerr == nil {
			cerr = serr
		}
		return cerr
	}
	return med, closer, nil
}

// toSpans converts kept time ranges to WaxFlow sample spans. The final span uses
// ToEnd when it reaches the source end so the trailer resolves the exact length.
func toSpans(keeps []cutrange.Range, total time.Duration, rate int) []waxflow.Span {
	spans := make([]waxflow.Span, len(keeps))
	for i, k := range keeps {
		from := samplesOf(k.Start, rate)
		to := samplesOf(k.End, rate)
		if k.End >= total {
			to = waxflow.ToEnd
		}
		spans[i] = waxflow.Span{From: from, To: to}
	}
	return spans
}

// sampleBounds returns the [from, to) sample bounds of a kept range. Unlike
// toSpans it never uses ToEnd, because Slice/SpanTrack take explicit bounds. Both
// bounds are clamped to trackSamples (when known) so a range derived from a
// longer sibling track's duration cannot ask Slice/SpanTrack for samples the
// default track does not have.
func sampleBounds(k cutrange.Range, total time.Duration, rate int, trackSamples int64) (from, to int64) {
	from = samplesOf(k.Start, rate)
	end := k.End
	if end > total {
		end = total
	}
	to = samplesOf(end, rate)
	if trackSamples > 0 {
		if to > trackSamples {
			to = trackSamples
		}
		if from > trackSamples {
			from = trackSamples
		}
	}
	return from, to
}

// samplesOf converts a duration to a sample count at rate.
func samplesOf(d time.Duration, rate int) int64 {
	return int64(math.Round(d.Seconds() * float64(rate)))
}

// ValidateCrossfade checks whether the retained spans can supply the requested
// overlap. A crossfade consumes d from both sides of each join, so an interior
// span must be at least 2*d. Rejecting short spans up front avoids an encode that
// emits no audio.
func ValidateCrossfade(keeps []cutrange.Range, d time.Duration) error {
	if d <= 0 || len(keeps) < 2 {
		return nil
	}
	for i, k := range keeps {
		required := d
		if i != 0 && i != len(keeps)-1 {
			required = 2 * d
		}
		if k.Duration() < required {
			return fmt.Errorf("%w: crossfade %v is too long for the %v span kept at %v (needs %v)",
				waxerr.ErrIncompatibleSpec, d, k.Duration(), k.Start, required)
		}
	}
	return nil
}
