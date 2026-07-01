// Package pipeline runs WaxTap's source-agnostic audio processing on a staged
// local file: it cuts time ranges, normalizes loudness, and transcodes, fusing
// whatever is requested into a single ffmpeg encode.
//
// The facade acquires the input (a YouTube download staged to a temp file, or a
// local file) and a transcode.Runner, then calls [Run]. The pipeline never knows
// where the audio came from, so the YouTube and local-file paths share it.
//
// The stages are probe, optional loudness analysis, one fused processing pass,
// and an optional output loudness measurement. Analysis includes any requested
// cut and downmix so loudnorm measures the audio that will be encoded.
package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
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
	// Remove lists [Start, End) spans to cut. Spans are clamped to the probed
	// duration and merged before processing. An empty slice means no cut.
	Remove    []cut.Range
	CutMode   cut.Mode      // rendering strategy for effective cuts
	Crossfade time.Duration // overlap applied at each splice
	// RejectEmptyRemoval rejects a non-empty Remove when every span lies outside
	// the media. The check runs before output is written.
	RejectEmptyRemoval bool

	// Codec is the transcode target. transcode.CodecCopy means keep the source
	// codec (no re-encode unless a cut, loudness apply, or downmix forces one).
	Codec   transcode.Codec
	Bitrate int // target bits per second for lossy codecs

	// Downmix reduces sources with more channels to this count. Supported values
	// are 1 and 2. A downmix requires encoding; CodecCopy uses the source codec
	// family when possible.
	Downmix int

	// Remux requests a stream copy through ffmpeg even when Codec is CodecCopy,
	// for an explicit copy/remux into the output container (which strips non-audio
	// streams). The zero Spec, with Remux false, is a no-op that leaves the input
	// untouched. It is ignored when a re-encode or cut already runs.
	Remux bool

	// Loudness controls measurement/normalization. Nil means no loudness work.
	Loudness *Loudness

	// Threads limits ffmpeg's worker threads. Zero lets ffmpeg choose.
	Threads int
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
	Transcoded       bool            // a re-encode ran (not a stream copy/remux)
	OutputCodec      transcode.Codec // codec written to OutputPath
	LoudnessMeasured bool            // input loudness was measured
	LoudnessApplied  bool            // normalization was applied

	InputLoudness  *normalize.Loudness // measured post-cut input loudness
	OutputLoudness *normalize.Loudness // measured output loudness, set only on Apply

	// OutputProbe is an ffprobe of the written OutputPath, populated whenever an
	// output file was produced (transcode, remux, downmix, or copy-mode cut). It is
	// nil for a measure-only or no-op spec (OutputPath == "") and nil when the probe
	// failed. Callers read it for authoritative output rate/channels/bitrate/
	// duration/size.
	OutputProbe *transcode.ProbeResult
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
		// Reject caller-supplied spans that do not intersect the media before
		// opening the output.
		if !effectiveCut && spec.RejectEmptyRemoval {
			return Result{}, fmt.Errorf("%w: cut ranges %s do not intersect the media (duration %s)",
				waxerr.ErrIncompatibleSpec, formatRanges(spec.Remove), total.Round(time.Second))
		}
	}
	if effectiveCut && spec.Crossfade > 0 {
		if err := cut.ValidateCrossfade(keeps, spec.Crossfade); err != nil {
			return Result{}, err
		}
	}

	var res Result
	res.OutputCodec = transcode.CodecCopy
	res.SourceDuration = total
	srcSampleRate, srcChannels, srcBitrate := 0, 0, 0
	if audio, ok := probe.AudioStream(); ok {
		res.SourceCodec = audio.CodecName
		srcSampleRate = audio.SampleRate
		srcChannels = audio.Channels
		srcBitrate = audio.BitRate
	}

	// Reduce the channel count only when the source exceeds the requested target.
	fold := 0
	if spec.Downmix > 0 && srcChannels > spec.Downmix {
		fold = spec.Downmix
	}

	// Resolve container compatibility before choosing an encoder for downmixing.
	// Automatic processing may select the container's default codec, while an
	// explicitly requested stream copy must fail.
	//
	// stageExt gives ffmpeg a muxer hint when the final path has no usable
	// extension.
	stageExt := ""
	if spec.Codec == transcode.CodecCopy && (effectiveCut || remux || fold > 0) {
		ext := containerExt(output)
		// A remux can infer a container from the source codec. Copy cuts still need
		// an explicit container because they create intermediate concat segments.
		if (remux || effectiveCut) && fold == 0 && (ext == "" || ext == "copy") {
			if effectiveCut {
				return Result{}, fmt.Errorf("%w: cannot stream-copy %s without a container extension; choose one that fits the source (%s)", waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec), containerSuggestion(res.SourceCodec))
			}
			stageExt = copyContainerExt(res.SourceCodec)
		}
		if !containerAccepts(ext, res.SourceCodec) {
			if remux || spec.CutMode == cut.ModeCopy {
				return Result{}, fmt.Errorf("%w: cannot stream-copy %s into a .%s container; transcode instead", waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec), ext)
			}
			c, ok := containerCodec(ext)
			if !ok {
				return Result{}, fmt.Errorf("%w: cannot infer an encoder for the .%s container; pass --format", waxerr.ErrIncompatibleSpec, ext)
			}
			spec.Codec = c
			transcoding = true
			remux = false
		}
		// Raw FLAC is a special copy-cut case. ffmpeg can stream-copy the audio, but
		// the FLAC muxer keeps the source STREAMINFO total_samples, so players see
		// the old duration after a trim. A downmix or crossfade already re-encodes
		// and writes fresh metadata. For a pure smart cut, upgrade to a lossless FLAC
		// encode; for explicit copy/remux, ask the caller to re-encode or use .mka
		// when a true stream copy is required.
		if effectiveCut && spec.Codec == transcode.CodecCopy && fold == 0 && spec.Crossfade == 0 &&
			ext == "flac" && strings.EqualFold(res.SourceCodec, "flac") {
			if remux || spec.CutMode == cut.ModeCopy {
				return Result{}, fmt.Errorf("%w: a stream-copy cut into raw FLAC leaves a stale duration header; re-encode (smart/accurate mode or --format flac) or use a Matroska (.mka) container for a true copy", waxerr.ErrIncompatibleSpec)
			}
			spec.Codec = transcode.CodecFLAC
			transcoding = true
			remux = false
		}
	}

	// A downmix into a compatible container uses the source codec family when no
	// transcode target was requested.
	if fold > 0 && spec.Codec == transcode.CodecCopy {
		c, ok := sourceEncodeCodec(res.SourceCodec)
		if !ok {
			return Result{}, fmt.Errorf("%w: cannot downmix %s without a transcode target (pass --format)", waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec))
		}
		spec.Codec = c
		if spec.Bitrate == 0 && !c.IsLossless() {
			spec.Bitrate = srcBitrate // 0 falls back to the preset default
		}
		transcoding = true
		remux = false
	}

	// Measure after applying the requested cut and downmix. Reusing the same
	// filters keeps the measured and encoded audio equivalent.
	var measured normalize.Loudness
	if measure {
		send(StageAnalyzing)
		if effectiveCut {
			graph := cut.Graph(keeps, spec.Crossfade, total, "pre")
			sink := "pre"
			if fold > 0 {
				graph += ";[pre]" + foldFilter(fold) + "[folded]"
				sink = "folded"
			}
			measured, err = normalize.MeasureComplex(ctx, r, input, graph, sink, spec.Threads)
		} else {
			var pre []string
			if fold > 0 {
				pre = []string{foldFilter(fold)}
			}
			measured, err = normalize.Measure(ctx, r, input, pre, spec.Threads)
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

	// Preserve the source sample rate because loudnorm otherwise outputs 192 kHz.
	enc := transcode.Spec{Codec: spec.Codec, Bitrate: spec.Bitrate, StageExt: stageExt, Threads: spec.Threads}
	switch {
	case fold > 0 && apply:
		// Fold before loudnorm so its true-peak limiter bounds fold clipping.
		enc.Filters = []string{foldFilter(fold), normalize.ApplyFilter(spec.Loudness.Target, measured, srcSampleRate)}
	case fold > 0:
		// No loudnorm: fold via -ac with ffmpeg's normalized downmix coefficients.
		enc.Channels = fold
	case apply:
		enc.Filters = []string{normalize.ApplyFilter(spec.Loudness.Target, measured, srcSampleRate)}
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
		if out, merr := normalize.Measure(ctx, r, output, nil, spec.Threads); merr == nil {
			res.OutputLoudness = &out
		}
	}

	// Probe the written output so callers can report authoritative output numbers
	// (sample rate, channels, bitrate, duration, size). Best-effort: the write
	// already succeeded, so a probe failure must not fail the job. Reaching here
	// means a real file was produced; the measure-only/no-op early return above
	// left OutputPath empty and never probes.
	if op, perr := r.Probe(ctx, output); perr == nil {
		res.OutputProbe = &op
	}
	return res, nil
}

// foldFilter returns an ffmpeg filter that uses libswresample's normalized
// downmix matrix. It is used when downmixing must precede another filter.
func foldFilter(channels int) string {
	if channels == 1 {
		return "aformat=channel_layouts=mono"
	}
	return "aformat=channel_layouts=stereo"
}

// sourceEncodeCodec maps a probed source codec name to the transcode.Codec that
// re-encodes in the same family, so a downmix with no transcode target keeps the
// source codec. It reports false for codecs WaxTap cannot encode.
func sourceEncodeCodec(name string) (transcode.Codec, bool) {
	switch strings.ToLower(name) {
	case "opus":
		return transcode.CodecOpus, true
	case "aac":
		return transcode.CodecAAC, true
	case "vorbis":
		return transcode.CodecVorbis, true
	case "mp3":
		return transcode.CodecMP3, true
	case "flac":
		return transcode.CodecFLAC, true
	case "alac":
		return transcode.CodecALAC, true
	}
	if strings.HasPrefix(strings.ToLower(name), "pcm") {
		return transcode.CodecWAV, true
	}
	return transcode.CodecCopy, false
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
func formatRanges(rs []cut.Range) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.Start.Round(time.Second).String() + "-" + r.End.Round(time.Second).String()
	}
	return strings.Join(parts, ", ")
}

// containerAccepts reports whether the container named by ext can stream-copy the
// given ffprobe codec. The compatibility table lives in transcode so copy-time
// and encode-time checks use the same rules. Unknown extensions are left for
// ffmpeg to validate.
func containerAccepts(ext, codec string) bool {
	return transcode.ContainerAccepts(ext, codec)
}

// containerSuggestion lists conventional container extensions for a probed source
// codec. It falls back to a broad list when the codec is unknown.
func containerSuggestion(codec string) string {
	if exts := transcode.ContainersFor(codec); len(exts) > 0 {
		return strings.Join(exts, "/")
	}
	return ".webm/.m4a/.ogg/.mka"
}

// copyContainerExt returns a container extension suitable for a stream copy of
// codec. Only the staged file uses this extension.
func copyContainerExt(codec string) string {
	switch strings.ToLower(codec) {
	case "opus":
		return "webm"
	case "aac", "alac":
		return "m4a"
	case "vorbis":
		return "ogg"
	case "mp3":
		return "mp3"
	case "flac":
		return "flac"
	default:
		return "mka" // Matroska accepts the remaining codecs WaxTap handles
	}
}

// containerCodec returns the default encoder for a container extension. It
// reports false for an unknown extension.
func containerCodec(ext string) (transcode.Codec, bool) {
	switch ext {
	case "flac":
		return transcode.CodecFLAC, true
	case "wav":
		return transcode.CodecWAV, true
	case "mp3":
		return transcode.CodecMP3, true
	case "m4a", "mp4", "m4b", "aac":
		return transcode.CodecAAC, true
	case "ogg", "oga":
		return transcode.CodecVorbis, true
	case "opus":
		return transcode.CodecOpus, true
	case "webm", "mka", "mkv":
		return transcode.CodecOpus, true
	}
	return transcode.CodecCopy, false
}
