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
package pipeline

import (
	"context"
	"fmt"
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
	Codec   media.Codec
	Bitrate int // target bits per second for lossy codecs

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

	Cut              bool          // an effective cut was rendered
	Removed          time.Duration // audio removed by the cut
	Transcoded       bool          // a re-encode ran (not a container copy)
	OutputCodec      media.Codec   // codec written to OutputPath
	LoudnessMeasured bool          // input loudness was measured
	LoudnessApplied  bool          // normalization was applied

	InputLoudness  *loudness.Loudness // measured post-cut input loudness
	OutputLoudness *loudness.Loudness // measured output loudness, set only on Apply

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
	srcChannels := 0
	if audio, ok := probe.AudioStream(); ok {
		res.SourceCodec = audio.CodecName
		srcChannels = audio.Channels
	}

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
		c, ok := sourceEncodeCodec(res.SourceCodec)
		if !ok {
			return Result{}, fmt.Errorf("%w: cannot downmix %s without a transcode target (pass --format)", waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec))
		}
		spec.Codec = c
		transcoding = true
		remux = false
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

	enc := media.Spec{Codec: spec.Codec, Bitrate: spec.Bitrate, Channels: fold}
	if apply {
		enc.GainDB = loudness.GainFor(spec.Loudness.Target, measured)
	}

	if apply {
		send(StageNormalizing)
	}
	if effectiveCut {
		send(StageCutting)
		fallback := enc
		if copyCut {
			// The re-encode fallback (when cut-remux declines the source codec)
			// keeps the source family, staying lossless for a lossless source.
			if c, ok := sourceEncodeCodec(res.SourceCodec); ok {
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
			return Result{}, err
		}
		res.Cut = cres.Applied
		res.Removed = cres.Removed
		// A copy cut that fell back to a re-encode (cut-remux declined the source
		// codec) reports the encode it actually produced.
		if copyCut && cres.Mode == media.ModeAccurate {
			transcoding = true
			spec.Codec = fallback.Codec
		}
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
	// Best-effort: the apply already succeeded, so a measurement failure must not
	// fail the job.
	if apply {
		send(StageAnalyzing)
		// The output is already at the target layout, so measure it as-is (0).
		if out, merr := loudness.Measure(ctx, r, output, 0); merr == nil {
			res.OutputLoudness = &out
		}
	}

	// Probe the written output so callers can report authoritative output numbers.
	// Best-effort: the write already succeeded, so a probe failure must not fail
	// the job.
	if op, perr := r.Probe(ctx, output); perr == nil {
		res.OutputProbe = &op
	}
	return res, nil
}

// sourceEncodeCodec maps a probed source codec name to the media.Codec that
// re-encodes in the same family, so a downmix or a declined cut-remux keeps the
// source codec. It reports false for codecs WaxTap cannot encode.
func sourceEncodeCodec(name string) (media.Codec, bool) {
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
