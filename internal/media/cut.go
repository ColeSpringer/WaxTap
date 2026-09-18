package media

import (
	"context"
	"fmt"
	"math"
	"strings"
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
	// RequireCopyCutMode and RequireCopyFormat each fail the cut rather than
	// re-encoding when WaxFlow declines the lossless cut-remux: an explicit copy
	// request, which silently re-encoding would break.
	//
	// They are two fields rather than one bool so the refusal can name the flag
	// the caller actually passed. They are independent and may both be set.
	RequireCopyCutMode bool // --cut-mode copy
	RequireCopyFormat  bool // --format copy
	Encode             Spec
	// SourceSamples is the frame count a decode of the source delivers, when
	// the caller measured it (Runner.MeasureLength) because the headers only
	// claim a length; 0 trusts the headers. The composed timeline holds each
	// span to it, so a span reaching the end asks for what the file has.
	SourceSamples int64
}

// requireCopy reports whether either explicit copy request is in force.
func (s CutSpec) requireCopy() bool { return s.RequireCopyCutMode || s.RequireCopyFormat }

// copyFlags names the copy requests this spec carries, for advice that tells
// the caller to drop a flag they actually wrote.
func (s CutSpec) copyFlags() string {
	switch {
	case s.RequireCopyCutMode && s.RequireCopyFormat:
		return "--format copy / --cut-mode copy"
	case s.RequireCopyCutMode:
		return "--cut-mode copy"
	default:
		return "--format copy"
	}
}

// CutResult reports a completed cut.
type CutResult struct {
	Output  string
	Removed time.Duration
	Mode    Mode
	Applied bool
	// Levels is WaxFlow's level measurement of the cut re-encode; see
	// Result.Levels. It is always zero for a lossless cut-remux.
	Levels Levels
	// InputWarnings is the input damage the read found, complete as of the
	// end of the write; see Result.InputWarnings. A cut reads only the spans
	// it keeps, so damage inside a removed span stays unreported here.
	InputWarnings []string
}

// Render applies spec's cut to input and writes the result to output. Output is
// staged and atomically renamed on success. When CopyCut is set and no downmix,
// gain, or crossfade is requested, Render tries a lossless cut-remux first and
// re-encodes only if WaxFlow declines the source codec.
//
// input and output must name different files, for the reason Transcode gives.
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
	var levels Levels
	var found []string
	if tryRemux {
		done, rfound, rerr := r.cutRemux(ctx, src, hint, outExt, spec.Keeps, spec.Total, staged)
		if rerr != nil {
			return CutResult{}, classifyEngineError(rerr, input, output)
		}
		if done {
			mode, found = ModeCopy, rfound
		} else {
			// WaxFlow declined a lossless cut-remux of the source codec (e.g. FLAC),
			// or of the cut's shape (HE-AAC packet-cuts only from the stream start).
			if spec.requireCopy() {
				return CutResult{}, fmt.Errorf("%w: cannot losslessly copy-cut this source (%s support a packet-level cut; HE-AAC only when the cut keeps the stream start); drop %s to re-encode, which stays lossless for a lossless source", waxerr.ErrIncompatibleSpec, strings.Join(waxflow.CutFormats(), "/"), spec.copyFlags())
			}
			// Fall through to a re-encode, which stays lossless for a lossless
			// source. A copy spec whose source has no same-family encoder (WMA,
			// Musepack) has no fallback to fall to; failing here names the escape, where
			// the engine would only say "no output format requested".
			if spec.Encode.Codec == CodecCopy {
				return CutResult{}, fmt.Errorf("%w: this source codec cannot be packet-cut and has no same-family encoder; pass an explicit format (e.g. flac) to render the cut", waxerr.ErrIncompatibleSpec)
			}
			if levels, found, err = r.cutReencode(ctx, src, hint, outExt, spec, staged); err != nil {
				return CutResult{}, classifyEngineError(err, input, output)
			}
		}
	} else {
		if levels, found, err = r.cutReencode(ctx, src, hint, outExt, spec, staged); err != nil {
			return CutResult{}, classifyEngineError(err, input, output)
		}
	}

	if err := staged.Commit(); err != nil {
		return CutResult{}, err
	}
	return CutResult{
		Output:        output,
		Removed:       spec.Total - cutrange.OutputDuration(spec.Keeps, spec.Crossfade),
		Mode:          mode,
		Applied:       true,
		Levels:        levels,
		InputWarnings: found,
	}, nil
}

// cutRemux performs the lossless packet-level cut-remux. It reports done=false
// (and no error) when WaxFlow declines the source codec, so the caller re-encodes
// instead, and the damage the packet walk found when it ran.
func (r *Runner) cutRemux(ctx context.Context, src container.Source, hint, outExt string, keeps []cutrange.Range, total time.Duration, dst *tempfile.File) (done bool, found []string, err error) {
	grid, err := r.engine.PacketGrid(src, hint)
	if err != nil {
		return false, nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	demux, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return false, nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	outFormat, ok := codecToFormat(track.Codec)
	if !ok {
		return false, nil, nil // unknown codec: let the re-encode path handle it
	}
	spans := toSpans(keeps, total, track.Fmt.Rate)
	opts := waxflow.TranscodeOptions{Format: outFormat, Container: containerFor(outFormat, outExt)}

	plan, err := r.engine.PlanCut(track, opts, spans, grid)
	if err != nil {
		return false, nil, err
	}
	if plan == nil {
		return false, nil, nil // declined (e.g. FLAC): fall back to a re-encode
	}
	cutTrack, _, err := waxflow.CutTrack(track, spans, grid)
	if err != nil {
		return false, nil, err
	}
	cutDemux, err := waxflow.Cut(demux, track, spans, grid)
	if err != nil {
		return false, nil, err
	}
	// NB: pass CutTrack's track, not plan.Track.
	tres, err := r.engine.RemuxDemuxer(ctx, cutDemux, cutTrack, dst, opts)
	if err != nil {
		return false, nil, err
	}
	return true, sourceWarnings(tres.InputWarnings), nil
}

// cutReencode renders the cut by decoding: it slices the kept spans, concatenates
// them (with an optional crossfade), and re-encodes with spec.Encode. It reports
// the encode's level measurement and the damage the read found alongside; see
// Result.Levels and Result.InputWarnings.
func (r *Runner) cutReencode(ctx context.Context, src container.Source, hint, outExt string, spec CutSpec, dst *tempfile.File) (Levels, []string, error) {
	med, err := r.openComposed(src, hint, spec.Keeps, spec.Total, spec.Crossfade, spec.SourceSamples)
	if err != nil {
		return Levels{}, nil, err
	}
	defer med.Close()

	opts := encodeOptions(spec.Encode)
	format, _ := codecFormat(spec.Encode.Codec)
	opts.Container = containerFor(format, outExt)
	tres, err := r.engine.TranscodeMedia(ctx, med, dst, opts)
	if err != nil {
		return Levels{}, nil, err
	}
	return levelsOf(tres), sourceWarnings(tres.InputWarnings), nil
}

// openComposed builds the WaxFlow Media for the kept spans: a single Slice for
// one span, or a Concat of per-span slices (with an optional crossfade) for
// several. The caller closes the returned Media.
//
// It probes the source's rate to convert the time-domain keeps to sample
// spans. sourceSamples is 0 or the count a caller's Runner.MeasureLength
// delivered; see CutSpec.SourceSamples.
func (r *Runner) openComposed(src container.Source, hint string, keeps []cutrange.Range, total time.Duration, crossfade time.Duration, sourceSamples int64) (format.Media, error) {
	_, info, err := format.OpenDemuxer(src, hint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	// bound is what a bounded span is clamped under: the media a span opens
	// refuses a bound past its own declared total (Slice checks it up front
	// through SpanTrack), and the track handed to a timeline refuses one
	// past the measured count, so the smaller of the two it is.
	bound := track.Samples
	if sourceSamples > 0 {
		// The caller measured the source (MeasureLength), which is the
		// length the timeline holds each span to; the declaration is only
		// what the spans are still clamped under.
		if bound <= 0 || sourceSamples < bound {
			bound = sourceSamples
		}
		track.Samples, track.SamplesAdvisory = sourceSamples, false
	}
	// An open-ended final span inherits the track's own claim about its
	// length, and a Concat refuses one that is advisory (a WMA nobody
	// measured) or absent altogether (raw ADTS, MP3 in an AIFF-C, both
	// SamplesAdvisory false with no claim at all), so the open form is used
	// only where the count is trusted or measured.
	openEnded := !track.SamplesAdvisory && track.Samples >= 0
	rate := track.Fmt.Rate

	if len(keeps) == 1 {
		from, to := sampleBounds(keeps[0], total, rate, bound, openEnded)
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
		from, to := sampleBounds(k, total, rate, bound, openEnded)
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
// re-encoding. The caller closes it. sourceSamples is 0 or the count a
// caller's Runner.MeasureLength delivered; see CutSpec.SourceSamples.
func (r *Runner) OpenComposed(input string, keeps []cutrange.Range, total, crossfade time.Duration, sourceSamples int64) (format.Media, func() error, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return nil, nil, err
	}
	med, err := r.openComposed(src, hintFor(input), keeps, total, crossfade, sourceSamples)
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

// sampleBounds returns the [from, to) sample bounds of a kept range. A range
// reaching total takes ToEnd when openEnded, as toSpans does: an open-ended
// slice runs to whatever the source holds and declares no limit a short
// source can fall foul of, where a bounded one is held to its declaration
// (Slice refuses a source that ends inside a declared span). An interior
// bound, and a final one on a track whose claim cannot be trusted open, is
// clamped to bound, the count the opened media and the timeline both
// refuse to be asked past.
func sampleBounds(k cutrange.Range, total time.Duration, rate int, bound int64, openEnded bool) (from, to int64) {
	from = samplesOf(k.Start, rate)
	if k.End >= total && openEnded {
		to = waxflow.ToEnd
	} else {
		to = samplesOf(min(k.End, total), rate)
		if bound > 0 && to > bound {
			to = bound
		}
	}
	if bound > 0 && from > bound {
		from = bound
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
