// Package pipeline runs WaxTap's source-agnostic audio processing on a staged
// local file: it cuts time ranges, normalizes loudness, and transcodes, fusing
// whatever is requested into a single ffmpeg encode.
//
// The facade acquires the input (a YouTube download staged to a temp file, or a
// local file) and a transcode.Runner, then calls [Run]. The pipeline never knows
// where the audio came from, so the YouTube and local-file paths share it.
//
// The stages are: probe, then an optional loudness analysis pass over the
// post-cut audio, then one fused apply (cut + loudnorm + transcode), then an
// optional post-measure of the output. Measurement runs before the encode so the
// loudnorm values describe exactly the audio that will be written, and the cut
// graph is reused so the measured and encoded audio match even across a
// multi-segment cut.
package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/colespringer/waxtap/cut"
	"github.com/colespringer/waxtap/normalize"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/waxerr"
)

// Stage identifies a processing stage for progress events. The facade maps these
// onto its public waxtap.Stage values.
type Stage uint8

const (
	StageProbing     Stage = iota // inspecting source media
	StageAnalyzing                // measuring loudness
	StageCutting                  // removing time ranges
	StageNormalizing              // applying loudness normalization
	StageTranscoding              // encoding or remuxing audio
)

func (s Stage) String() string {
	switch s {
	case StageProbing:
		return "probing"
	case StageAnalyzing:
		return "analyzing"
	case StageCutting:
		return "cutting"
	case StageNormalizing:
		return "normalizing"
	case StageTranscoding:
		return "transcoding"
	default:
		return "unknown"
	}
}

// Loudness configures the loudness stage. The zero value (Apply false) measures
// only; Apply normalizes to Target, fused into the encode.
type Loudness struct {
	Apply  bool    // normalize when true; measure only when false
	Target float64 // target integrated loudness in LUFS for Apply
}

// Spec describes the processing to perform. The zero value is a pass-through:
// nothing to cut, copy the source codec, no loudness work, which Run reports as
// no output produced.
type Spec struct {
	// Remove lists [Start, End) spans to cut. They are clamped and merged against
	// the probed duration, so out-of-range or overlapping spans are harmless. An
	// empty slice means no cut.
	Remove    []cut.Range
	CutMode   cut.Mode      // rendering strategy for effective cuts
	Crossfade time.Duration // overlap applied at each splice

	// Codec is the transcode target. transcode.CodecCopy means keep the source
	// codec (no re-encode unless a cut or loudness apply forces one).
	Codec   transcode.Codec
	Bitrate int // target bits per second for lossy codecs

	// Remux requests a stream copy through ffmpeg even when Codec is CodecCopy,
	// for an explicit copy/remux into the output container (which strips non-audio
	// streams). The zero Spec, with Remux false, is a no-op that leaves the input
	// untouched. It is ignored when a re-encode or cut already runs.
	Remux bool

	// Loudness controls measurement/normalization. Nil means no loudness work.
	Loudness *Loudness
}

// Result reports what the pipeline did.
type Result struct {
	// OutputPath is where the processed audio was written, or "" when no output
	// pass ran (a measure-only or no-op spec). With "" the caller delivers the
	// input unchanged.
	OutputPath string

	// SourceCodec is the probed input audio codec (for example "opus", "aac"),
	// so a caller can report the source format without re-probing.
	SourceCodec string
	// SourceDuration is the probed input duration, exposed so a caller can reason
	// about the cut (for example whether SponsorBlock contributed) without
	// re-probing. It is 0 when the input duration is unknown.
	SourceDuration time.Duration

	Cut              bool            // an effective cut was rendered
	Removed          time.Duration   // audio removed by the cut
	Transcoded       bool            // a re-encode to a new codec ran
	OutputCodec      transcode.Codec // codec written to OutputPath
	LoudnessMeasured bool            // input loudness was measured
	LoudnessApplied  bool            // normalization was applied

	InputLoudness  *normalize.Loudness // measured post-cut input loudness
	OutputLoudness *normalize.Loudness // measured output loudness, set only on Apply
}

// Run processes input per spec, writing any output to output. It returns a
// Result describing the work; when no output pass is needed (measure-only or a
// no-op), Result.OutputPath is "" and output is not written.
//
// emit receives stage transitions and may be nil.
func Run(ctx context.Context, r *transcode.Runner, input, output string, spec Spec, emit func(Stage)) (Result, error) {
	send := func(s Stage) {
		if emit != nil {
			emit(s)
		}
	}

	send(StageProbing)
	probe, err := r.Probe(ctx, input)
	if err != nil {
		return Result{}, err
	}
	total := probe.Format.Duration

	apply := spec.Loudness != nil && spec.Loudness.Apply
	measure := spec.Loudness != nil
	transcoding := spec.Codec != transcode.CodecCopy
	// An explicit stream-copy remux (Codec is Copy but Remux was requested). A
	// re-encode supersedes it, so it only matters in the pure-copy case.
	remux := spec.Remux && !transcoding

	// Loudness apply rewrites samples, so it needs a real encode. Copy and a
	// missing transcode target are both invalid.
	if apply && !transcoding {
		return Result{}, fmt.Errorf("%w: loudness apply requires a transcode target, not copy", waxerr.ErrIncompatibleSpec)
	}

	// Resolve the cut against the real duration. A cut is only "effective" when it
	// removes something; an empty SponsorBlock result or fully-clamped ranges fall
	// through to a plain transcode (or no-op) so a requested transcode still runs.
	var keeps []cut.Range
	effectiveCut := false
	if len(spec.Remove) > 0 {
		if total <= 0 {
			return Result{}, fmt.Errorf("%w: cannot cut input with unknown duration", waxerr.ErrUnsupportedInput)
		}
		keeps = cut.Keeps(spec.Remove, total)
		if len(keeps) == 0 {
			return Result{}, fmt.Errorf("%w: cut would remove the entire track", waxerr.ErrIncompatibleSpec)
		}
		effectiveCut = cut.OutputDuration(keeps, 0) < total
	}
	if effectiveCut && spec.Crossfade > 0 {
		if err := cut.ValidateCrossfade(keeps, spec.Crossfade); err != nil {
			return Result{}, err
		}
	}

	var res Result
	res.OutputCodec = transcode.CodecCopy
	res.SourceDuration = total
	if audio, ok := probe.AudioStream(); ok {
		res.SourceCodec = audio.CodecName
	}

	// Loudness analysis over the post-cut audio. Reusing the cut graph keeps the
	// measured audio identical to what the fused encode will produce.
	var measured normalize.Loudness
	if measure {
		send(StageAnalyzing)
		if effectiveCut {
			graph := cut.Graph(keeps, spec.Crossfade, total, "pre")
			measured, err = normalize.MeasureComplex(ctx, r, input, graph, "pre")
		} else {
			measured, err = normalize.Measure(ctx, r, input, nil)
		}
		if err != nil {
			return Result{}, err
		}
		res.LoudnessMeasured = true
		m := measured
		res.InputLoudness = &m
	}

	// Nothing to write: a measure-only or fully no-op spec. The caller delivers
	// the input unchanged.
	if !effectiveCut && !transcoding && !apply && !remux {
		return res, nil
	}

	enc := transcode.Spec{Codec: spec.Codec, Bitrate: spec.Bitrate}
	if apply {
		enc.Filters = []string{normalize.ApplyFilter(spec.Loudness.Target, measured)}
	}

	if apply {
		send(StageNormalizing)
	}
	if effectiveCut {
		send(StageCutting)
		cres, err := cut.Render(ctx, r, input, output, cut.Spec{
			Remove:    spec.Remove,
			Mode:      spec.CutMode,
			Crossfade: spec.Crossfade,
			Encode:    enc,
		})
		if err != nil {
			return Result{}, err
		}
		res.Cut = cres.Applied
		res.Removed = cres.Removed
	} else {
		send(StageTranscoding)
		if _, err := r.Transcode(ctx, input, output, enc); err != nil {
			return Result{}, err
		}
	}

	res.OutputPath = output
	res.Transcoded = transcoding
	res.LoudnessApplied = apply
	res.OutputCodec = spec.Codec

	// Post-measure the written output so callers can report the achieved loudness.
	// It is best-effort: the apply already succeeded, so a measurement failure
	// must not fail the job.
	if apply {
		send(StageAnalyzing)
		if out, merr := normalize.Measure(ctx, r, output, nil); merr == nil {
			res.OutputLoudness = &out
		}
	}
	return res, nil
}
