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
	"slices"
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
	StageTranscoding              // encoding audio
	StageRemuxing                 // copying packets into another container
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
	case StageRemuxing:
		return "remuxing"
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

// Keep is what the caller lets the keep rule keep, once the staged source
// can be seen. The facade derives it from TranscodeSpec: Force gives
// KeepNone, FromContainer gives KeepContainer, any other named format
// KeepCodec, so the pipeline never holds two answers at once.
type Keep uint8

const (
	// KeepNone: Codec is an encoder to run, whatever the source is in.
	KeepNone Keep = iota
	// KeepCodec: Codec is a codec to deliver, so a source already in it
	// (media.SourceMatches) is kept: a copy when nothing else needs the
	// encoder, Codec's encoder when Bitrate, BitDepth, a fold, a loudness
	// apply, an accurate cut, or a crossfade does.
	KeepCodec
	// KeepContainer: Codec is the output container's usual encoder, chosen
	// because the caller named an extension and could not see the source (a
	// URL, whose codec only the download settles). The container keeps
	// whatever it carries, on KeepCodec's terms, and encodes to Codec
	// otherwise (media.OutputCodecFor). Ignored when Codec is CodecCopy,
	// which takes the copy rule on its own.
	KeepContainer
)

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

	// Keep says what Run's keep rule may keep once the staged source is
	// probed; the zero value keeps nothing, so Codec is an encoder to run.
	Keep Keep

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
	// SourceWarnings are the input's damage notes, carried so a caller can
	// report that the delivered audio is the readable part of a damaged file
	// rather than all of it: the probe's (media.ProbeResult.Warnings), then
	// what the write's read found past the headers (media.Result
	// .InputWarnings; a demuxer that walks its payload lazily finds damage
	// where the read reaches it), each once, then the short-decode note when
	// the output came up short of what the source declared.
	//
	// Only the input contributes. The output probe below is of a file this
	// pipeline just wrote, where damage would be an encoder defect rather than
	// something to warn the user about their input.
	SourceWarnings []string
	// SourceNotes are the engine's remarks on an input that is not damaged
	// (media.ProbeResult.Notes): what it did with a well-formed file. The
	// probe's list is the whole of it: the engine raises every note when it
	// opens the file, so unlike damage none turns up later in the read.
	SourceNotes []string
	// SourceEmpty says the input's audio track decodes to no frames at all: a
	// container that parses and declares a codec but delivers nothing, whether
	// it stores nothing or stores only samples its gapless trims discard. It is
	// distinct from an unknown length, which the probe also reports as a zero
	// duration, and it is what lets a caller say "no audio frames" instead of
	// guessing at "unknown duration".
	SourceEmpty bool

	Cut     bool          // an effective cut was rendered
	Removed time.Duration // audio removed by the cut
	// Keeps are the spans the output really holds, in order, and Crossfade the
	// join overlap used; nil and 0 when no effective cut ran. A packet copy
	// snaps each interior join inward to the packet grid, so these are its
	// landed spans; a re-encode keeps exactly what was asked. Callers remap
	// source-timeline metadata (chapter marks) through them.
	Keeps     []cutrange.Range
	Crossfade time.Duration
	// CutMode is how the cut was rendered: media.ModeCopy for a packet copy,
	// ModeCopyExact for one with spliced joins, ModeAccurate for a decode.
	// Meaningful only when Cut is set.
	CutMode media.Mode
	// CutSnaps counts the interior joins a packet copy moved inward to the
	// packet grid and CutSnapMax is the largest single move; both zero for a
	// decode and for a copy whose edges already sat on the grid.
	CutSnaps   int
	CutSnapMax time.Duration
	// CutDeclined says why a cut that would have copied packets decoded
	// instead (media.CopyDecline); NotDeclined when the copy ran, or when
	// the spec named an encode and no copy was in question. The fallback's
	// encoder is OutputCodec.
	CutDeclined      media.CopyDecline
	Transcoded       bool        // a re-encode ran (not a container copy)
	OutputCodec      media.Codec // codec written to OutputPath
	LoudnessMeasured bool        // input loudness was measured
	// LoudnessApplied says a normalization gain actually reached the encode, not
	// merely that one was requested: an input with no measurable integrated
	// loudness yields a zero gain from both peak policies, and the file that
	// leaves is a plain transcode.
	LoudnessApplied bool

	InputLoudness *loudness.Loudness // measured post-cut input loudness
	// OutputLoudness is the measured loudness of the file left at OutputPath, set
	// whenever normalization was requested and the output could be measured -
	// including a non-finite measurement, which is a result the caller can
	// explain rather than a failure. It is nil when the measurement itself
	// failed, so a caller reporting it never has to wonder whether it matches
	// the delivered file.
	OutputLoudness *loudness.Loudness
	// GainDB is the gain this run applied, in dB: the encode's scalar (the
	// last pass's, under the PeakLimit search), or under GainInHeader the
	// amount the Opus header's output gain was moved by, quantized to its Q7.8
	// step. It is a change, not a total: a source whose head already stated a
	// gain keeps it, and the head the file leaves with states the sum. 0 when
	// no normalization ran or none could be derived.
	GainDB float64
	// GainInHeader says the gain rode in the OpusHead output gain and the
	// packets were copied untouched, rather than being applied to the samples
	// by a re-encode. Every compliant decoder applies it, WaxFlow's included,
	// so OutputLoudness reads the normalized loudness off the file.
	GainInHeader bool
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

	// EncodeBitRate is the encoder's own rate for an encode that took a
	// Bitrate: WaxFlow's plan (TranscodePlan.BitRate), which is the rate it
	// really runs at when the request is not one it supports exactly. 0
	// otherwise. WaxTap encodes Opus CBR (OpusVBR unset), so the Opus ceiling
	// is what is reported there; Vorbis never carries a rate.
	EncodeBitRate int

	// Levels is WaxFlow's level measurement of the delivered file: clipped
	// samples, output true peak, and whether a quantizer ran. It comes from the
	// last completed write, so when the PeakLimit gain search re-encodes it
	// always describes the file left at OutputPath. Zero for every copy path
	// and when no output pass ran; warning policy belongs to the caller.
	Levels media.Levels

	// TrimDropped is the source's gapless trim the copy could not carry; see
	// media.Result.TrimDropped. Zero for every other path.
	TrimDropped media.Trim
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
	// Whether the input holds no audio at all. Exactly 0 frames, not <= 0: -1 is
	// WaxFlow's "the container does not state a length", which is the opposite
	// claim, and both reach total == 0. Read here rather than off res because
	// the cut guard below runs before res exists.
	sourceEmpty := false
	if audio, ok := probe.AudioStream(); ok {
		sourceEmpty = audio.Samples == 0
	}

	apply := spec.Loudness != nil && spec.Loudness.Apply
	measure := spec.Loudness != nil
	transcoding := spec.Codec != media.CodecCopy
	// An explicit container copy (Codec is Copy but Remux was requested). A
	// re-encode supersedes it, so it only matters in the pure-copy case.
	remux := spec.Remux && !transcoding

	// sourceSamples is the count a read of the source delivers, measured only
	// when the headers merely claim a length.
	sourceSamples := int64(0)
	if len(spec.Remove) > 0 && probe.LengthClaimed {
		// The header's length is a claim (a payload the demuxer walks lazily,
		// an advisory total, or none), and a cut resolved against it can
		// declare a span the file does not hold: a truncated MP3 still states
		// its Xing count, and the engine refuses a span the source ends
		// inside. So the source is measured first and the ranges resolve
		// against what a read delivers, the way a truncated FLAC's clamped
		// probe already behaves. A walk settles every input that has one, a
		// Xing MP3 included; only a WMA costs a decode. A download reaches
		// here for a WebM Opus row, whose Matroska open is lazy and states
		// the Info Duration; an m4a states an exact count and does not.
		send(StageAnalyzing)
		length, lerr := r.MeasureLength(ctx, input)
		if lerr != nil {
			return Result{}, lerr
		}
		total, sourceSamples = length.Duration, length.Samples
		sourceEmpty = length.Samples == 0
		probe.Warnings = mergeSourceWarnings(probe.Warnings, length.Warnings)
		// The walk's own remarks arrive here too: a count off by one clean frame
		// is not damage, and the note is what says the measurement moved.
		probe.Notes = mergeSourceWarnings(probe.Notes, length.Notes)
	}

	// Resolve the cut against the real duration. A cut is only "effective" when it
	// removes something; an empty SponsorBlock result or fully-clamped ranges fall
	// through to a plain transcode (or no-op) so a requested transcode still runs.
	var keeps []cutrange.Range
	effectiveCut := false
	if len(spec.Remove) > 0 {
		if total <= 0 {
			// Two different facts arrive here as the same zero duration, and
			// telling a user their empty file has an "unknown duration" sends
			// them looking for a header problem that is not there.
			if sourceEmpty {
				return Result{}, fmt.Errorf("%w: cannot cut input: it contains no audio frames", waxerr.ErrUnsupportedInput)
			}
			return Result{}, fmt.Errorf("%w: cannot cut input with unknown duration", waxerr.ErrUnsupportedInput)
		}
		keeps = cutrange.Keeps(spec.Remove, total)
		if len(keeps) == 0 {
			return Result{}, fmt.Errorf("%w: cut would remove the entire track", waxerr.ErrIncompatibleSpec)
		}
		effectiveCut = cutrange.OutputDuration(keeps, 0) < total
		// Reject caller-supplied spans that do not intersect the media before
		// opening the output. When the probe worked around damage, the duration
		// here is the clamped one, and the user's ranges were likely written
		// against the length the header still claims, so the rejection carries
		// the probe's own note about why the file reads short.
		if !effectiveCut && spec.RejectEmptyRemoval {
			damage := ""
			if len(probe.Warnings) > 0 {
				damage = "; " + probe.Warnings[0]
			}
			return Result{}, fmt.Errorf("%w: cut ranges %s do not intersect the media (duration %s%s)",
				waxerr.ErrIncompatibleSpec, formatRanges(spec.Remove), total.Round(time.Second), damage)
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
	// Both lists are cloned: the warnings grow below and the notes go out
	// to the caller, and the probe's backing arrays are its own. A probe's
	// notes are the ones the engine raises at open; a measurement above has
	// already merged in the walk's, and only damage is found past there.
	res.SourceWarnings = slices.Clone(probe.Warnings)
	res.SourceNotes = slices.Clone(probe.Notes)
	res.SourceEmpty = sourceEmpty

	// Reduce the channel count only when the source exceeds the requested target.
	fold := 0
	if spec.Downmix > 0 && srcChannels > spec.Downmix {
		fold = spec.Downmix
	}

	// A decode the request named: accurate by name, a crossfade because it
	// blends across each join. Neither may be delivered as a packet copy, so
	// the keep rule and the promotion below both take the encoder, and the
	// copy rule's "choose a container" refusal is not theirs to raise.
	namedDecode := effectiveCut && (spec.CutMode == media.ModeAccurate || spec.Crossfade > 0)

	// keptNamed is the codec a KeepCodec match turned into a copy. A copy cut
	// that declines falls back to it rather than to the source's own family,
	// which the two differ over for one pair: an aac request keeps an HE-AAC
	// source, and a decode owes the caller the AAC-LC they named. CodecCopy
	// means the caller named nothing, where the source family is the answer.
	keptNamed := media.CodecCopy

	// The staged source's real codec is known now, so the keep rule runs
	// here: a source an encode would deliver unchanged is kept, as a copy
	// when nothing else needs the encoder and as the encoder when a knob it
	// honours, a fold, a gain, or a cut promised a decode does. Which
	// sources qualify is the caller's choice. A codec the caller named
	// (KeepCodec) keeps a source already in it, so an Opus delivery into
	// .opus is a copy, which the CLI's same-format shortcut already gives a
	// local file and nothing could give a URL before the download. A
	// container the caller named (KeepContainer) keeps whatever it carries
	// and takes its own encoder otherwise, the container rule
	// (media.OutputCodecFor). KeepNone is the caller asking for the encoder
	// regardless.
	if transcoding && spec.Keep != KeepNone {
		c, kept := spec.Codec, media.SourceMatches(res.SourceCodec, spec.Codec, containerExt(output))
		if spec.Keep == KeepContainer {
			// An extension naming no container WaxTap writes is left to the
			// caller's format, which then picks the muxer, exactly as the copy
			// rule below leaves such a path alone (media.needsForcedMuxer). The
			// bit is a fallback, so it never turns a spec that would have been
			// written into a refusal; CheckOutputContainer refuses the names
			// that must be refused, before the pipeline is reached.
			if cc, k, cerr := media.OutputCodecFor(containerExt(output), res.SourceCodec); cerr == nil {
				c, kept = cc, k
			} else {
				kept = false
			}
		}
		switch {
		case !kept:
			spec.Codec = c
		case c == media.CodecWAV || c == media.CodecAIFF:
			// The target carries the source, but the source is PCM, whose
			// sample layout belongs to its container: the packets cannot
			// move unchanged (media.Runner.remux declines every PCM copy,
			// remuxDeclined). The family row is an encode, and a PCM encode
			// is bit exact, so nothing is lost by taking it.
			spec.Codec = c
		case (spec.Bitrate > 0 && media.TakesBitRate(c)) || (spec.BitDepth > 0 && c.IsLossless()) || apply || fold > 0:
			// Only a knob the family encoder honours forces the encode: a
			// bit depth on Opus or a bit rate on FLAC is ignored by that
			// encoder and would buy a generation for nothing, which is what
			// the CLI's same-format shortcut already decides for a local file
			// (specChangesAudio).
			//
			// A fold takes the encoder here rather than through the downmix
			// promotion below, so it runs the codec the caller named (aac on
			// an HE-AAC source folds to AAC-LC) instead of the source's family.
			spec.Codec = c
		case namedDecode:
			// A copy cut would honour neither, and Render never learns the mode.
			spec.Codec = c
		default:
			if spec.Keep == KeepCodec {
				keptNamed = c
			}
			spec.Codec = media.CodecCopy
			// A copy has no encoder to hand a knob to; an ignored one is
			// zeroed as the CLI's shortcut zeroes it, so it neither reaches
			// the remux nor a re-encode a later promotion makes of this copy.
			spec.Bitrate, spec.BitDepth = 0, 0
			transcoding = false
			// What gets a plain copy past the no-op return below. Not set for
			// a cut: copyOnly in the copy rule and RequireCopyFormat in the
			// cut render both read this flag as "--format copy", which would
			// refuse a crossfade the caller never asked to forbid. A cut
			// writes regardless, as a packet copy where the codec allows and
			// a same-family re-encode where it does not.
			remux = !effectiveCut
		}
	}

	// Resolve container compatibility before choosing an encoder. Automatic
	// processing may select the container's default codec; an explicitly
	// requested container copy must fail on an incompatible extension.
	if spec.Codec == media.CodecCopy && (effectiveCut || remux || fold > 0) {
		ext := containerExt(output)
		copyOnly := remux || spec.CutMode == media.ModeCopy || spec.CutMode == media.ModeCopyExact
		// A fold, an accurate cut, and a crossfade all promote to an encoder
		// below, whose own muxer writes the file, so none of them needs an
		// extension to name a container. Only a copy does: its packets go
		// into whatever the name says.
		noContainer := effectiveCut && fold == 0 && !namedDecode && (ext == "" || ext == "copy")
		// A source WaxFlow only decodes has no packets any container can carry
		// unchanged, so every request to keep them fails for the one reason,
		// ahead of the container checks that would otherwise suggest containers
		// nothing could put it in.
		if display, decodeOnly := media.DecodeOnlyCodec(res.SourceCodec); decodeOnly && (copyOnly || noContainer) {
			return Result{}, fmt.Errorf("%w: cannot copy %s: WaxFlow decodes %s but does not write it, so no container can carry the packets unchanged; pass --format to re-encode",
				waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec), display)
		}
		// A copy cut writes into the container named by the output extension.
		if noContainer {
			return Result{}, fmt.Errorf("%w: cannot copy %s without a container extension; choose one that fits the source (%s), or pass --format to re-encode",
				waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec), containerSuggestion(res.SourceCodec))
		}
		// The container rule, one statement of it in media.OutputCodecFor: a
		// container that can carry the source codec keeps it (the copy above
		// stands), and one that cannot takes its own usual encoder. An
		// extension naming no container at all does not constrain the write,
		// so it keeps the copy and the muxer is forced from the format.
		if !containerAccepts(ext, res.SourceCodec) {
			c, _, cerr := media.OutputCodecFor(ext, res.SourceCodec)
			if cerr != nil {
				// A name WaxTap only reads is refused whatever the source:
				// there is no "transcode instead" into it, and no encoder to
				// infer for it.
				return Result{}, fmt.Errorf("%w; choose an output extension WaxTap writes, or pass --format to re-encode", cerr)
			}
			if copyOnly {
				return Result{}, fmt.Errorf("%w: cannot copy %s into a .%s container; transcode instead", waxerr.ErrIncompatibleSpec, sourceCodecLabel(res.SourceCodec), ext)
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

	// A decode the request named must not become a packet copy. Accurate
	// mode and a crossfade reach here with the codec still Copy by one
	// route: the facade waives its "pass --format" refusal when a downmix is
	// asked for, and a fold that turns out to have nothing to fold leaves
	// the copy standing. Promote the way a fold does, to the source's own
	// family; an explicit copy format asked for two things at once and is
	// refused, as Render refuses a crossfade beside --format copy.
	if namedDecode && spec.Codec == media.CodecCopy {
		what := "an accurate cut"
		if spec.Crossfade > 0 {
			what = "a crossfade"
		}
		if remux {
			return Result{}, fmt.Errorf("%w: %s needs a decode, which --format copy forbids; drop one", waxerr.ErrIncompatibleSpec, what)
		}
		c, ok := sourceEncodeCodec(res.SourceCodec, containerExt(output))
		if !ok {
			return Result{}, fmt.Errorf("%w: cannot decode %s of %s without a transcode target (pass --format)", waxerr.ErrIncompatibleSpec, what, sourceCodecLabel(res.SourceCodec))
		}
		spec.Codec = c
		transcoding = true
	}

	// An explicit copy cut cannot ride along with an encode. The facade rejects the
	// --format form before any download, but this also catches the route it cannot
	// see: the downmix branch above sets spec.Codec without consulting CutMode, so
	// --cut-mode copy --downmix --channels mono with no --format would otherwise
	// reach the same silent downgrade. Placed after container resolution so the
	// container-mismatch path (which already honors ModeCopy) reports its own
	// clearer error first.
	if effectiveCut && (spec.CutMode == media.ModeCopy || spec.CutMode == media.ModeCopyExact) && spec.Codec != media.CodecCopy {
		return Result{}, fmt.Errorf("%w: a copy cut cannot re-encode, but this spec encodes to %s; drop the copy mode or the encode",
			waxerr.ErrIncompatibleSpec, spec.Codec)
	}

	// Loudness apply rewrites samples, so it needs a real encode. Checked after
	// container resolution and the downmix promotion, both of which turn a copy
	// into one: --downmix on a copy spec really does encode, and refusing it
	// earlier rejected a request the pipeline was about to satisfy.
	if apply && !transcoding {
		return Result{}, fmt.Errorf("%w: loudness apply requires a transcode target, not copy", waxerr.ErrIncompatibleSpec)
	}

	// A copy cut is one neither the keep rule nor the guard above turned into
	// an encode, so it is never accurate and never crossfaded. It stays
	// lossless: WaxTap cut-remuxes it (kept codec, byte-identical packets)
	// and re-encodes only if the copy declines.
	copyCut := effectiveCut && spec.Codec == media.CodecCopy

	// A gain the Opus header can carry: cap mode's one scalar, on an Opus
	// source staying Opus, with no cut, no fold, and no bitrate asked for.
	// limit cannot take it (the limiter reshapes samples), and a bitrate asks
	// for an encode. The container is whichever the output names; the facade
	// validated it for Opus, and every one of them carries the head.
	//
	// A source wider than stereo is excluded for the same reason an explicit
	// fold is: the Opus encoder folds it, so the measurement below is taken at
	// the fold, and writing that gain into the head of a copy that still
	// carries every channel would state a gain for audio the file does not
	// deliver. Such a source takes the encode path, which really does fold.
	// It has no fixture here: WaxFlow's Opus encoder folds every wide source,
	// so a surround Opus file can only come from another tool.
	headerGain := apply && !spec.Loudness.PeakLimit && !effectiveCut && fold == 0 && srcChannels <= 2 &&
		res.SourceCodec == "opus" && spec.Codec == media.CodecOpus && spec.Bitrate == 0

	// Ask WaxFlow what the encode really delivers, once, for the two questions
	// that need it: the width a measurement has to fold to, and the rate the
	// encoder runs at when one was requested. A plan error is an encode that
	// would fail (a rate under the encoder's floor lands here), so it is
	// returned before anything is written.
	measureFold := fold
	needFold := measure && transcoding && fold == 0 && srcChannels > 2
	if transcoding && ((spec.Bitrate > 0 && media.TakesBitRate(spec.Codec)) || needFold) {
		// Channels: fold, so the plan describes the encode that will really
		// run. The AAC family's rate floor is per channel, so a plan taken at
		// the source width would report a rate the folded encode never uses.
		n, br, perr := r.PlanEncode(ctx, input, output, media.Spec{Codec: spec.Codec, Bitrate: spec.Bitrate, BitDepth: spec.BitDepth, Channels: fold})
		if perr != nil {
			return Result{}, perr
		}
		if needFold && n > 0 && n < srcChannels {
			measureFold = n
		}
		// Only where a rate was actually asked for and taken. A PCM or
		// lossless plan still projects a BitRate (rate times channels times
		// depth, for PCM), which describes the output rather than answering
		// the request, and reporting it as an adjustment would claim the WAV
		// encoder "cannot run at 128000 b/s" beside a note saying the flag
		// was ignored.
		if spec.Bitrate > 0 && media.TakesBitRate(spec.Codec) {
			res.EncodeBitRate = br
		}
	}

	// Measure after resolving the cut. The composed cut audio is measured, so the
	// gain matches the encoded bytes.
	//
	// It measures the requested keeps, while a packet copy delivers the landed
	// ones. The two can only differ by under one packet at each interior join,
	// and the measurement has to precede the render that decides where they
	// land, so closing the gap would mean cutting twice for a difference of
	// milliseconds in an integrated figure over the whole programme.
	var measured loudness.Loudness
	if measure {
		// The measurement folds to the width the encode delivers. An explicit
		// Downmix is one fold; a lossy row folding a wide source on its own is
		// the other, and the gain has to be computed on the audio the encoder
		// meters either way, since a fold moves the integrated loudness and the
		// true peak both. The plan is WaxFlow's own, so the two cannot disagree.
		send(StageAnalyzing)
		if effectiveCut {
			measured, err = loudness.MeasureCut(ctx, r, input, keeps, total, spec.Crossfade, measureFold, sourceSamples)
		} else {
			measured, err = loudness.Measure(ctx, r, input, measureFold)
		}
		if err != nil {
			return Result{}, err
		}
		res.LoudnessMeasured = true
		// The measurement read the whole input, so its damage list is the
		// complete one, with or without a write to follow.
		res.SourceWarnings = mergeSourceWarnings(res.SourceWarnings, measured.Warnings)
		m := measured
		res.InputLoudness = &m
	}

	if headerGain {
		return writeHeaderGain(ctx, r, input, output, spec.Loudness.Target, total, measured, res, send)
	}

	// Nothing to write: a measure-only or fully no-op spec. The caller delivers
	// the input unchanged. The meter's own read length is checked on the way
	// out: with no output file to compare, a measurement that covered less of
	// the track than the probe declared is the only sign the file does not
	// decode to its declared length (mid-file corruption probes clean).
	if !effectiveCut && !transcoding && !apply && !remux {
		if measure {
			if note := media.ShortMeasureNote(measured.Duration, total); note != "" {
				res.SourceWarnings = append(res.SourceWarnings, note)
			}
		}
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
				// The re-encode fallback (when the packet copy declines, for
				// whatever reason Render reports on Declined) keeps the source
				// family, staying lossless for a lossless source. A codec the
				// caller named wins over the family: that is what they asked
				// to be delivered, and the copy was only the cheaper way to
				// deliver it.
				if keptNamed != media.CodecCopy {
					fallback.Codec = keptNamed
				} else if c, ok := sourceEncodeCodec(res.SourceCodec, containerExt(output)); ok {
					fallback.Codec = c
				}
			}
			cres, err := r.Render(ctx, input, output, media.CutSpec{
				Keeps:              keeps,
				Total:              total,
				Crossfade:          spec.Crossfade,
				CopyCut:            copyCut,
				RequireCopyCutMode: spec.CutMode == media.ModeCopy || spec.CutMode == media.ModeCopyExact,
				RequireCopyFormat:  remux,
				SpliceTrims:        spec.CutMode == media.ModeCopyExact,
				Encode:             fallback,
				SourceSamples:      sourceSamples,
			})
			if err != nil {
				return err
			}
			res.Cut = cres.Applied
			res.Removed = cres.Removed
			res.Keeps = cres.Keeps
			res.CutMode = cres.Mode
			res.CutSnaps, res.CutSnapMax = cres.Snaps, cres.SnapMax
			res.CutDeclined = cres.Declined
			res.Levels = cres.Levels
			res.SourceWarnings = mergeSourceWarnings(res.SourceWarnings, cres.InputWarnings)
			// A copy cut that fell back to a re-encode (the packet copy declined;
			// res.CutDeclined says why) reports the encode it actually produced.
			if copyCut && cres.Mode == media.ModeAccurate {
				transcoding = true
				spec.Codec = fallback.Codec
			}
			return nil
		}
		// Branch on the encoder's own codec, not spec.Codec: spec.Codec is
		// reassigned above by container resolution and the downmix fold, so a
		// --format copy promoted to a real encoder would otherwise still say
		// "remuxing".
		if enc.Codec == media.CodecCopy {
			send(StageRemuxing)
		} else {
			send(StageTranscoding)
		}
		tres, err := r.Transcode(ctx, input, output, enc)
		if err == nil {
			res.Levels = tres.Levels
			res.TrimDropped = tres.TrimDropped
			res.SourceWarnings = mergeSourceWarnings(res.SourceWarnings, tres.InputWarnings)
		}
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
	// Requesting normalization is not applying it: on an unmeasurable input both
	// gain functions return 0 by design, so the encode above was a plain
	// transcode. Gainable is their own guard, so this flag cannot disagree with
	// what the gain actually did.
	res.LoudnessApplied = apply && loudness.Gainable(measured.IntegratedLUFS)
	res.OutputCodec = spec.Codec

	// The output is already at the target layout, so it is measured as-is (0).
	measureOutput := func() (loudness.Loudness, error) { return loudness.Measure(ctx, r, output, 0) }

	switch {
	case apply && spec.Loudness.PeakLimit:
		// The limiter gives back part of whatever gain it is handed, by an amount
		// that depends on the material, so one pass cannot hit the target. Measure
		// the encode and correct.
		var cerr error
		res.OutputLoudness, res.LoudnessPasses, res.GainDB, cerr = converge(ctx, spec.Loudness.Target, enc, measureOutput, write, send)
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
		res.GainDB = enc.GainDB
	}

	// Probe the written output so callers can report authoritative output numbers.
	// Best-effort: the write already succeeded, so a probe failure must not fail
	// the job.
	if op, perr := r.Probe(ctx, output); perr == nil {
		res.OutputProbe = &op
		// A decode path (re-encode or rendered cut) that delivered materially
		// less audio than the source declared means the decode ended early on a
		// file the probe passed clean - the one shape of input damage with no
		// probe warning to carry. A pure remux is exempt: it moves packets
		// without decoding, so the output declares whatever the input declared.
		if res.Transcoded || res.Cut {
			want := total
			if effectiveCut {
				// The landed keeps, not the request: a packet copy gives up
				// under one packet at each interior join, so a many-join cut
				// measured against the request would read as input damage.
				want = cutrange.OutputDuration(res.Keeps, spec.Crossfade)
			}
			if note := media.ShortDecodeNote(op.Format.Duration, want); note != "" {
				res.SourceWarnings = append(res.SourceWarnings, note)
			}
		}
	}
	return res, nil
}

// writeHeaderGain delivers a cap-mode Opus normalization as a packet copy
// whose head carries the gain. The copy carries the source's own header gain
// across, and the measurement above already heard that gain, so the head is
// moved by the change rather than set to a total. An input with no measurable
// loudness derives no gain and is delivered as a plain copy.
func writeHeaderGain(ctx context.Context, r *media.Runner, input, output string, target float64, total time.Duration, measured loudness.Loudness, res Result, send func(Stage)) (Result, error) {
	res.LoudnessApplied = loudness.Gainable(measured.IntegratedLUFS)
	q := 0
	if res.LoudnessApplied {
		send(StageNormalizing)
		q = media.OpusGainQ78(loudness.GainFor(target, measured))
	}
	// The packets are copied, so the output probe cannot reveal a decode that
	// ended early; the measurement above is the only read of the audio, and its
	// own length is what says the file does not decode to what it declares.
	// The encode path learns the same thing from its output probe.
	if note := media.ShortMeasureNote(measured.Duration, total); note != "" {
		res.SourceWarnings = append(res.SourceWarnings, note)
	}
	send(StageRemuxing)
	var tres media.Result
	var err error
	if res.LoudnessApplied {
		tres, _, err = r.RemuxWithOpusGain(ctx, input, output, q)
	} else {
		tres, err = r.Transcode(ctx, input, output, media.Spec{Codec: media.CodecCopy})
	}
	if err != nil {
		return Result{}, err
	}
	res.SourceWarnings = mergeSourceWarnings(res.SourceWarnings, tres.InputWarnings)
	res.OutputPath = output
	res.OutputCodec = media.CodecOpus
	// Only a head that actually moved: a source already on target quantizes to
	// a zero step, and the file that leaves is a plain copy whose own
	// ReplayGain and R128 tags still describe it exactly. Claiming the header
	// carried a gain would drop them for nothing.
	res.GainInHeader = q != 0
	res.GainDB = media.OpusGainDB(q)
	res.LoudnessPasses = 1
	send(StageAnalyzing)
	// Measured, not derived from the input plus the gain, although a header
	// gain is a pure scalar and the arithmetic would be exact: this is the one
	// read that confirms the head survived the mux and the decoder applies it,
	// and OutputLoudness is documented as the file's own measurement. It costs
	// one decode of a file this path did not re-encode, where the encode path
	// it replaces pays an encode and this measurement both. An album derives
	// instead, because there the same check would decode every track twice.
	//
	// Best-effort on both, like every other post-write measurement here: the
	// file is already delivered, so neither failure fails the job.
	if out, merr := loudness.Measure(ctx, r, output, 0); merr == nil {
		res.OutputLoudness = &out
	}
	if op, perr := r.Probe(ctx, output); perr == nil {
		res.OutputProbe = &op
	}
	return res, nil
}

// mergeSourceWarnings appends the damage a write's read found to the probe's
// list, each line once: the write closure can run more than once (the
// PeakLimit gain search), and every pass reads the same file and finds the
// same damage.
func mergeSourceWarnings(have, found []string) []string {
	for _, w := range found {
		if !slices.Contains(have, w) {
			have = append(have, w)
		}
	}
	return have
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
) (*loudness.Loudness, int, float64, error) {
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
				return nil, writes, gain, ctx.Err()
			}
			break
		}
		m := out
		cur = &m
		if !out.Finite() {
			// Silence: no miss to correct, and no gain would change it. Recorded
			// before the break so the caller still gets the measurement it can
			// explain; leaving OutputLoudness nil here made limit-mode the one
			// path where an unmeasurable output reported nothing at all, where
			// cap's single post-measure has always recorded it.
			break
		}

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
			return cur, writes, gain, nil
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
				return nil, writes, gain, err
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
				return nil, writes, gain, err
			}
			// The rewrite failed, so the previous pass is still at output and cur still
			// describes it. Nothing to correct.
		} else {
			writes++
			cur, gain = best, bestGain
		}
	}
	return cur, writes, gain, nil
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
	return media.SourceFamilyCodec(name, outExt)
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
