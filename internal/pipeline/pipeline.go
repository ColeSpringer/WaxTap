// Package pipeline runs WaxTap's source-agnostic audio processing on a staged
// local file: it cuts time ranges, normalizes loudness, and transcodes, fusing
// whatever is requested into a single WaxFlow pass.
//
// The facade acquires the input (a YouTube download staged to a temp file, or a
// local file) and a media.Runner, then calls [Run]. The pipeline never knows
// where the audio came from, so the YouTube and local-file paths share it.
//
// The stages are probe, optional loudness analysis, one fused processing pass,
// and an optional output loudness measurement. Analysis includes any requested
// cut so the gain matches the audio that will be encoded.
//
// Normalizing with a true-peak limiter is the one case that writes more than
// once: the limiter gives back part of whatever gain it is handed, so the pass is
// measured and the gain corrected until the output lands on the target. Each pass
// rewrites the output atomically, so the destination always holds a complete file.
package pipeline

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/waxerr"
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
	// PeakLimit applies the full gain and lets WaxFlow's true-peak limiter catch
	// the overshoot. False, the zero value, caps the gain instead so the true peak
	// stays under the ceiling, which is transparent but can miss the target.
	PeakLimit bool
}

// Spec describes the processing to perform. The zero value is a pass-through:
// nothing to cut, copy the source codec, no loudness work, which Run reports as
// no output produced.
type Spec struct {
	// Remove lists [Start, End) spans to cut. Spans are clamped to the probed
	// duration and merged before processing. An empty slice means no cut.
	Remove    []cutrange.Range
	CutMode   media.Mode    // rendering strategy for effective cuts
	Crossfade time.Duration // overlap applied at each splice
	// RejectEmptyRemoval rejects a non-empty Remove when every span lies outside
	// the media. The check runs before output is written.
	RejectEmptyRemoval bool

	// Codec is the transcode target. media.CodecCopy means keep the source codec
	// (no re-encode unless a cut, loudness apply, or downmix forces one).
	Codec    media.Codec
	Bitrate  int // target bits per second for lossy codecs
	BitDepth int // forced integer output depth; 0 follows the decoded stream

	// Downmix reduces sources with more channels to this count. Supported values
	// are 1 and 2. A downmix requires encoding; CodecCopy uses the source codec
	// family when possible.
	Downmix int

	// Remux requests a container copy even when Codec is CodecCopy, for an
	// explicit copy/remux into the output container. The zero Spec, with Remux
	// false, is a no-op that leaves the input untouched. It is ignored when a
	// re-encode or cut already runs.
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
	// SourceChannels is the probed input channel count, 0 when unknown. Callers
	// compare it against OutputProbe to detect a fold the encoder applied on its
	// own, which no field of the request would otherwise reveal.
	SourceChannels int

	Cut     bool          // an effective cut was rendered
	Removed time.Duration // audio removed by the cut
	// Keeps are the kept source spans the effective cut composed, in order, and
	// Crossfade the join overlap used; nil and 0 when no effective cut ran.
	// Callers remap source-timeline metadata (chapter marks) through them.
	Keeps            []cutrange.Range
	Crossfade        time.Duration
	Transcoded       bool        // a re-encode ran (not a container copy)
	OutputCodec      media.Codec // codec written to OutputPath
	LoudnessMeasured bool        // input loudness was measured
	LoudnessApplied  bool        // normalization was applied

	InputLoudness *loudness.Loudness // measured post-cut input loudness
	// OutputLoudness is the measured loudness of the file left at OutputPath, set
	// only on Apply. It is nil when the measurement failed, so a caller reporting it
	// never has to wonder whether it matches the delivered file.
	OutputLoudness *loudness.Loudness
	// LoudnessPasses counts the output writes normalization took: 1 for PeakCap and
	// for a PeakLimit pass that landed inside tolerance, more when the limiter-backed
	// gain needed correcting. It is 0 when no normalization ran. Only completed
	// writes count, so it always matches the encodes behind the delivered file.
	LoudnessPasses int

	// OutputProbe is a probe of the written OutputPath, populated whenever an
	// output file was produced. It is nil for a measure-only or no-op spec and
	// nil when the probe failed. Callers read it for authoritative output
	// rate/channels/duration/size.
	OutputProbe *media.ProbeResult
}

// Run processes input per spec, writing any output to output. It returns a
// Result describing the work; when no output pass is needed (measure-only or a
// no-op), Result.OutputPath is "" and output is not written.
//
// emit receives stage transitions and may be nil.
func Run(ctx context.Context, r *media.Runner, input, output string, spec Spec, emit func(Stage)) (Result, error) {
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
	transcoding := spec.Codec != media.CodecCopy
	// An explicit container copy (Codec is Copy but Remux was requested). A
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
	var keeps []cutrange.Range
	effectiveCut := false
	if len(spec.Remove) > 0 {
		if total <= 0 {
			return Result{}, fmt.Errorf("%w: cannot cut input with unknown duration", waxerr.ErrUnsupportedInput)
		}
		keeps = cutrange.Keeps(spec.Remove, total)
		if len(keeps) == 0 {
			return Result{}, fmt.Errorf("%w: cut would remove the entire track", waxerr.ErrIncompatibleSpec)
		}
		effectiveCut = cutrange.OutputDuration(keeps, 0) < total
		// Reject caller-supplied spans that do not intersect the media before
		// opening the output.
		if !effectiveCut && spec.RejectEmptyRemoval {
			return Result{}, fmt.Errorf("%w: cut ranges %s do not intersect the media (duration %s)",
				waxerr.ErrIncompatibleSpec, formatRanges(spec.Remove), total.Round(time.Second))
		}
	}
	if effectiveCut && spec.Crossfade > 0 {
		if err := media.ValidateCrossfade(keeps, spec.Crossfade); err != nil {
			return Result{}, err
		}
	}

	var res Result
	res.OutputCodec = media.CodecCopy
	res.SourceDuration = total
	if effectiveCut {
		res.Keeps = keeps
		res.Crossfade = spec.Crossfade
	}
	srcChannels := 0
	if audio, ok := probe.AudioStream(); ok {
		res.SourceCodec = audio.CodecName
		srcChannels = audio.Channels
	}
	res.SourceChannels = srcChannels

	// Reduce the channel count only when the source exceeds the requested target.
	fold := 0
	if spec.Downmix > 0 && srcChannels > spec.Downmix {
		fold = spec.Downmix
	}

	// Resolve container compatibility before choosing an encoder. Automatic
	// processing may select the container's default codec; an explicitly
	// requested container copy must fail on an incompatible extension.
	if spec.Codec == media.CodecCopy && (effectiveCut || remux || fold > 0) {
		ext := containerExt(output)
		// A copy cut writes into the container named by the output extension.
		if effectiveCut && fold == 0 && (ext == "" || ext == "copy") {
			return Result{}, fmt.Errorf("%w: cannot copy %s without a container extension; choose one that fits the source (%s)",
				waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec), containerSuggestion(res.SourceCodec))
		}
		if !containerAccepts(ext, res.SourceCodec) {
			if remux || spec.CutMode == media.ModeCopy {
				return Result{}, fmt.Errorf("%w: cannot copy %s into a .%s container; transcode instead", waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec), ext)
			}
			c, ok := containerCodec(ext)
			if !ok {
				return Result{}, fmt.Errorf("%w: cannot infer an encoder for the .%s container; pass --format", waxerr.ErrIncompatibleSpec, ext)
			}
			spec.Codec = c
			transcoding = true
			remux = false
		}
	}

	// A downmix into a compatible container uses the source codec family when no
	// transcode target was requested.
	if fold > 0 && spec.Codec == media.CodecCopy {
		c, ok := sourceEncodeCodec(res.SourceCodec, containerExt(output))
		if !ok {
			return Result{}, fmt.Errorf("%w: cannot downmix %s without a transcode target (pass --format)", waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec))
		}
		spec.Codec = c
		transcoding = true
		remux = false
	}

	// An explicit copy cut cannot ride along with an encode. The facade rejects the
	// --format form before any download, but this also catches the route it cannot
	// see: the downmix branch above sets spec.Codec without consulting CutMode, so
	// --cut-mode copy --downmix --channels mono with no --format would otherwise
	// reach the same silent downgrade. Placed after container resolution so the
	// container-mismatch path (which already honors ModeCopy) reports its own
	// clearer error first.
	if effectiveCut && spec.CutMode == media.ModeCopy && spec.Codec != media.CodecCopy {
		return Result{}, fmt.Errorf("%w: a copy cut cannot re-encode, but this spec encodes to %s; drop the copy mode or the encode",
			waxerr.ErrIncompatibleSpec, spec.Codec)
	}

	// A copy cut that survived container resolution stays lossless: WaxTap
	// cut-remuxes it (kept codec, byte-identical packets) and re-encodes only if
	// WaxFlow declines the source codec.
	copyCut := effectiveCut && spec.Codec == media.CodecCopy

	// Measure after resolving the cut. The composed cut audio is measured, so the
	// gain matches the encoded bytes.
	var measured loudness.Loudness
	if measure {
		send(StageAnalyzing)
		// Fold the measurement to the downmix target so the gain is computed on the
		// audio the encode will meter (fold is 0 when no downmix applies).
		if effectiveCut {
			measured, err = loudness.MeasureCut(ctx, r, input, keeps, total, spec.Crossfade, fold)
		} else {
			measured, err = loudness.Measure(ctx, r, input, fold)
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

	enc := media.Spec{Codec: spec.Codec, Bitrate: spec.Bitrate, BitDepth: spec.BitDepth, Channels: fold}
	if apply {
		// The two peak policies differ only here: RawGain aims straight at the target
		// and hands the peaks to WaxFlow's limiter, GainFor holds the peak under the
		// ceiling and may fall short.
		if spec.Loudness.PeakLimit {
			enc.GainDB = loudness.RawGain(spec.Loudness.Target, measured.IntegratedLUFS)
		} else {
			enc.GainDB = loudness.GainFor(spec.Loudness.Target, measured)
		}
	}

	// write runs one complete output pass: the fused cut+encode, or a plain
	// transcode. It is a closure so the loudness search below can run it more than
	// once. Every path stages through internal/tempfile and commits, so re-running
	// it atomically replaces the output rather than appending to it, and enc.GainDB
	// is absolute and always applied to input, so repeated passes never compound.
	write := func(enc media.Spec) error {
		if effectiveCut {
			send(StageCutting)
			fallback := enc
			if copyCut {
				// The re-encode fallback (when cut-remux declines the source codec)
				// keeps the source family, staying lossless for a lossless source.
				if c, ok := sourceEncodeCodec(res.SourceCodec, containerExt(output)); ok {
					fallback.Codec = c
				}
			}
			cres, err := r.Render(ctx, input, output, media.CutSpec{
				Keeps:       keeps,
				Total:       total,
				Crossfade:   spec.Crossfade,
				CopyCut:     copyCut,
				RequireCopy: spec.CutMode == media.ModeCopy || remux,
				Encode:      fallback,
			})
			if err != nil {
				return err
			}
			res.Cut = cres.Applied
			res.Removed = cres.Removed
			// A copy cut that fell back to a re-encode (cut-remux declined the source
			// codec) reports the encode it actually produced.
			if copyCut && cres.Mode == media.ModeAccurate {
				transcoding = true
				spec.Codec = fallback.Codec
			}
			return nil
		}
		send(StageTranscoding)
		_, err := r.Transcode(ctx, input, output, enc)
		return err
	}

	if apply {
		send(StageNormalizing)
	}
	if err := write(enc); err != nil {
		return Result{}, err
	}

	res.OutputPath = output
	res.Transcoded = transcoding
	res.LoudnessApplied = apply
	res.OutputCodec = spec.Codec

	// The output is already at the target layout, so it is measured as-is (0).
	measureOutput := func() (loudness.Loudness, error) { return loudness.Measure(ctx, r, output, 0) }

	switch {
	case apply && spec.Loudness.PeakLimit:
		// The limiter gives back part of whatever gain it is handed, by an amount
		// that depends on the material, so one pass cannot hit the target. Measure
		// the encode and correct.
		var cerr error
		res.OutputLoudness, res.LoudnessPasses, cerr = converge(ctx, spec.Loudness.Target, enc, measureOutput, write, send)
		if cerr != nil {
			// Only cancellation reaches here; everything else the search can hit is
			// non-fatal by design. It must not be swallowed: the file at output is a
			// complete earlier pass, but it carries the wrong gain, and returning
			// success would report that as the requested loudness. The caller keeps
			// exit 130 and can name the file it found.
			return Result{}, cerr
		}
	case apply:
		// PeakCap keeps exactly one pass. Its head clamp is the whole policy, and
		// iterating would defeat it; the caller reports the resulting miss instead.
		//
		// Post-measure so callers can report the achieved loudness. Best-effort: the
		// apply already succeeded, so a measurement failure must not fail the job.
		send(StageAnalyzing)
		if out, merr := measureOutput(); merr == nil {
			res.OutputLoudness = &out
		}
		res.LoudnessPasses = 1
	}

	// Probe the written output so callers can report authoritative output numbers.
	// Best-effort: the write already succeeded, so a probe failure must not fail
	// the job.
	if op, perr := r.Probe(ctx, output); perr == nil {
		res.OutputProbe = &op
	}
	return res, nil
}

// Tuning for the PeakLimit gain search.
const (
	// maxLoudnessWrites caps how many times the search writes output while
	// converging on a PeakLimit target.
	//
	// One further write is allowed past it, and only to put back the best pass the
	// search already measured. That write cannot come out of the budget: the search
	// spends its last write speculatively, without knowing whether the result will
	// improve, so reserving a slot for the restore would cost a search pass on every
	// run to serve the one where the last pass got worse. The worst case is therefore
	// maxLoudnessWrites+1 encodes, and only on a run that would otherwise have
	// delivered a file the search itself had already rejected.
	maxLoudnessWrites = 4

	// loudnessGainSlope is the LUFS gained per dB of applied gain, seeding the first
	// correction before there are two points to take a secant from. It is under 1.0
	// because the limiter gives back part of every dB; assuming unit slope
	// under-corrects and costs an extra pass every time.
	loudnessGainSlope = 0.93

	// minLoudnessSlope and maxLoudnessSlope bound a secant estimate. A slope above
	// 1.0 is not physical for a limiter that only ever gives gain back, and a very
	// small one (two passes that barely moved) would blow the next step up.
	minLoudnessSlope = 0.5
	maxLoudnessSlope = 1.0
)

// converge re-encodes until the measured output loudness is within
// [loudness.ConvergeToleranceDB] of target, and reports the delivered
// measurement together with the number of output writes that produced it.
//
// write performs one output pass with the gain in the spec it is handed; measure
// reads back the file write left behind. They are injected so the search can be
// exercised without an encoder: it is the one place in WaxTap whose failure mode
// is silently delivering audio at a loudness nobody asked for.
//
// The returned measurement always describes the file left at output, or is nil
// when that could not be measured; a caller reporting it must not have to wonder
// whether the file matches. enc arrives holding the first pass's gain, which has
// already been written.
//
// Failures are non-fatal, matching the best-effort contract the single-pass
// post-measure has always carried: the apply already succeeded, and every write is
// atomic, so a failed correction leaves a complete earlier pass at output and
// simply stops the search.
//
// Cancellation is the one exception and is returned as an error. The file at
// output is complete either way, but it holds an uncorrected gain, so reporting
// success would present a loudness the caller never asked for as the delivered
// result. Returning it also keeps a Ctrl-C at exit 130.
func converge(
	ctx context.Context,
	target float64,
	enc media.Spec,
	measure func() (loudness.Loudness, error),
	write func(media.Spec) error,
	send func(Stage),
) (*loudness.Loudness, int, error) {
	gain := enc.GainDB // gain that produced the file currently at output
	var cur *loudness.Loudness
	bestGain, bestMiss := gain, math.Inf(1)
	var best *loudness.Loudness
	// Previous (gain, LUFS) point, for the secant slope. NaN until a second pass.
	prevGain, prevLUFS := math.NaN(), math.NaN()
	writes := 1

	for {
		send(StageAnalyzing)
		out, merr := measure()
		if merr != nil {
			if ctx.Err() != nil {
				return nil, writes, ctx.Err()
			}
			break
		}
		if !out.Finite() {
			break // silence: no miss to correct, and no gain would change it
		}
		m := out
		cur = &m

		// Symmetric on the absolute miss. The step can overshoot (a 0.93 slope
		// assumed against a true 1.0 gives miss*0.075 of overshoot), and the -70 LUFS
		// absolute gate can let a pass over-deliver on dynamic material. A loop that
		// only tested miss > tol would accept an unbounded overshoot and ship a
		// silent "delivered -12.0 for a -14 target", the same defect being fixed.
		miss := target - out.IntegratedLUFS
		improved := math.Abs(miss) < bestMiss
		if improved {
			b := out
			bestGain, bestMiss, best = gain, math.Abs(miss), &b
		}
		if bestMiss <= loudness.ConvergeToleranceDB {
			return cur, writes, nil
		}
		// A pass that did not improve on an earlier one ends the search; so does a
		// spent write budget.
		if !improved || writes >= maxLoudnessWrites {
			break
		}

		// Do not assume unit slope. Seed from loudnessGainSlope, then use the secant
		// of the last two (gain, LUFS) points once they exist.
		slope := loudnessGainSlope
		if !math.IsNaN(prevGain) && gain != prevGain {
			slope = clampFloat((out.IntegratedLUFS-prevLUFS)/(gain-prevGain), minLoudnessSlope, maxLoudnessSlope)
		}
		// Clamped like the gains loudness computes: the search walks away from the
		// value RawGain returned, and a step off the end of WaxFlow's accepted range
		// would end the search on a write error instead of on the miss.
		next := loudness.ClampGain(gain + miss/slope)
		if next == gain {
			break // pinned at the limit: another pass would encode the same file
		}

		prevGain, prevLUFS = gain, out.IntegratedLUFS
		enc.GainDB = next
		if err := write(enc); err != nil {
			if ctx.Err() != nil {
				return nil, writes, err
			}
			// The failed pass left the previous file at output, so gain and cur still
			// describe what is there.
			break
		}
		writes++
		gain, cur = next, nil // cur is stale: the file changed
	}

	// Restore the best pass when the search left something worse at output, or left
	// a file it could not measure. This runs at most once and is not bound by
	// maxLoudnessWrites; see the constant for why.
	if best != nil && gain != bestGain {
		enc.GainDB = bestGain
		if err := write(enc); err != nil {
			if ctx.Err() != nil {
				return nil, writes, err
			}
			// The rewrite failed, so the previous pass is still at output and cur still
			// describes it. Nothing to correct.
		} else {
			writes++
			cur = best
		}
	}
	return cur, writes, nil
}

// clampFloat bounds v to [lo, hi].
func clampFloat(v, lo, hi float64) float64 {
	return min(max(v, lo), hi)
}

// sourceEncodeCodec maps a probed source codec name to the media.Codec that
// re-encodes in the same family, so a downmix or a declined cut-remux keeps the
// source codec. It reports false for codecs WaxTap cannot encode.
//
// PCM has two container-defining encoders, so outExt picks AIFF over the WAV
// default. Without it these fallbacks write RIFF bytes into an AIFF file. outExt
// comes from containerExt, already lowercased.
func sourceEncodeCodec(name, outExt string) (media.Codec, bool) {
	switch strings.ToLower(name) {
	case "opus":
		return media.CodecOpus, true
	case "aac":
		return media.CodecAAC, true
	case "vorbis":
		return media.CodecVorbis, true
	case "mp3":
		return media.CodecMP3, true
	case "flac":
		return media.CodecFLAC, true
	case "alac":
		return media.CodecALAC, true
	}
	if strings.HasPrefix(strings.ToLower(name), "pcm") {
		if media.IsAIFFExt(outExt) {
			return media.CodecAIFF, true
		}
		return media.CodecWAV, true
	}
	return media.CodecCopy, false
}

// sourceCodecLabel formats a probed codec name for error messages.
func sourceCodecLabel(name string) string {
	if name == "" {
		return "the source stream"
	}
	return name + " audio"
}

// containerExt returns the lowercased output extension without a dot, or "" when
// the path has none.
func containerExt(output string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
}

// formatRanges renders removal ranges as "start-end" pairs for an error message.
func formatRanges(rs []cutrange.Range) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Start.Round(time.Second).String() + "-" + r.End.Round(time.Second).String()
	}
	return strings.Join(parts, ", ")
}

// containerAccepts reports whether the container named by ext can hold the given
// codec unchanged. Unknown extensions are left permissive.
func containerAccepts(ext, codec string) bool {
	return media.ContainerAccepts(ext, codec)
}

// containerSuggestion lists conventional container extensions for a probed source
// codec. It falls back to a broad list when the codec is unknown.
func containerSuggestion(codec string) string {
	if exts := media.ContainersFor(codec); len(exts) > 0 {
		return strings.Join(exts, "/")
	}
	return ".webm/.m4a/.ogg/.mka"
}

// containerCodec returns the default encoder for a container extension. It
// reports false for an unknown extension.
func containerCodec(ext string) (media.Codec, bool) {
	if media.IsAIFFExt(ext) {
		return media.CodecAIFF, true
	}
	switch ext {
	case "flac":
		return media.CodecFLAC, true
	case "wav":
		return media.CodecWAV, true
	case "mp3":
		return media.CodecMP3, true
	case "m4a", "mp4", "m4b", "aac":
		return media.CodecAAC, true
	case "ogg", "oga":
		return media.CodecVorbis, true
	case "opus":
		return media.CodecOpus, true
	case "webm", "mka", "mkv":
		return media.CodecOpus, true
	}
	return media.CodecCopy, false
}
