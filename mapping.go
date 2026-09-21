package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3/download"
	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
	"github.com/colespringer/waxtap/v3/youtube"
)

// transcodeCodec maps a public TranscodeFormat to a media.Codec.
func transcodeCodec(f TranscodeFormat) media.Codec {
	switch f {
	case FormatFLAC:
		return media.CodecFLAC
	case FormatALAC:
		return media.CodecALAC
	case FormatWAV:
		return media.CodecWAV
	case FormatAIFF:
		return media.CodecAIFF
	case FormatMP3:
		return media.CodecMP3
	case FormatAAC:
		return media.CodecAAC
	case FormatOpus:
		return media.CodecOpus
	case FormatVorbis:
		return media.CodecVorbis
	case FormatHEAAC:
		return media.CodecHEAAC
	case FormatWavPack:
		return media.CodecWavPack
	case FormatAPE:
		return media.CodecAPE
	default:
		return media.CodecCopy
	}
}

// transcodeTarget maps a TranscodeSpec to a format.Target so source selection can
// minimize cross-codec loss. A nil or copy spec yields the zero Target (best
// audio). Lossless targets gain nothing from a matched source. Lossy targets name
// a source codec family only when YouTube has a native equivalent (AAC, Opus,
// Vorbis); MP3 has none, so it ranks on best audio.
func transcodeTarget(t *TranscodeSpec) format.Target {
	if t == nil {
		return format.Target{}
	}
	c := transcodeCodec(t.Format)
	if c == media.CodecCopy {
		return format.Target{}
	}
	if c.IsLossless() {
		return format.Target{Lossless: true}
	}
	switch t.Format {
	case FormatAAC, FormatHEAAC:
		// HE-AAC shares the AAC family: YouTube's mp4a itags are the nearest
		// native source for either target.
		return format.Target{Codec: "aac"}
	case FormatOpus:
		return format.Target{Codec: "opus"}
	case FormatVorbis:
		return format.Target{Codec: "vorbis"}
	default:
		return format.Target{}
	}
}

// cutRanges maps public TimeRanges to cutrange.Ranges.
func cutRanges(rs []TimeRange) []cutrange.Range {
	if len(rs) == 0 {
		return nil
	}
	out := make([]cutrange.Range, len(rs))
	for i, r := range rs {
		out[i] = cutrange.Range{Start: r.Start, End: r.End}
	}
	return out
}

// Inclusive bounds for an applied integrated-loudness target.
const (
	loudnessTargetMin = -70.0
	loudnessTargetMax = -5.0
)

// maxBitrate rejects likely unit mistakes while remaining above practical lossy
// audio bitrates.
const maxBitrate = 3_000_000 // bits/sec

// minPlausibleBitrate rejects a kbps value or a 1-10 quality scale mistakenly
// passed as bits/sec (e.g. 128 or 5 instead of 128000), all of which fall well
// below 1000. It still permits an intentional sub-8-kbps voice encode.
const minPlausibleBitrate = 1000 // bits/sec

// ValidateProcessSpec checks a ProcessSpec without acquiring or processing media.
// Invalid specs return an error that wraps [ErrIncompatibleSpec].
// [Client.Download], [Client.Stream], and [Client.Process] call it automatically;
// callers may use it to fail before starting batch work.
func ValidateProcessSpec(s ProcessSpec) error { return validateProcessSpec(s) }

// validateProcessSpec rejects unsupported ProcessSpec combinations before
// acquisition or audio processing begins.
func validateProcessSpec(s ProcessSpec) error {
	if s.Downmix && s.Channels != LayoutMono && s.Channels != LayoutStereo {
		return fmt.Errorf("%w: downmix requires Channels mono or stereo, got %s", waxerr.ErrIncompatibleSpec, s.Channels)
	}
	// ValidateCrossfade treats non-positive durations as disabled, so reject
	// negative values before reaching it.
	if s.Cut != nil && s.Cut.Crossfade < 0 {
		return fmt.Errorf("%w: crossfade must be non-negative, got %v", waxerr.ErrIncompatibleSpec, s.Cut.Crossfade)
	}
	if err := validateWriter(s.Output); err != nil {
		return err
	}
	if err := validateEnums(s); err != nil {
		return err
	}
	if s.Channels != LayoutAny && !s.Downmix {
		return fmt.Errorf("%w: Channels names a downmix target, so it needs Downmix; leave it LayoutAny for no fold", waxerr.ErrIncompatibleSpec)
	}
	if err := validateRanges(s.Cut); err != nil {
		return err
	}
	if err := validateOutputContainer(s); err != nil {
		return err
	}
	if err := validateCutEncodeNeed(s); err != nil {
		return err
	}
	if err := validateLoudness(s); err != nil {
		return err
	}
	if err := validateBitrate(s.Transcode); err != nil {
		return err
	}
	if err := validateBitDepth(s.Transcode); err != nil {
		return err
	}
	if err := validateForce(s.Transcode); err != nil {
		return err
	}
	return validateCoverArt(s)
}

// validateWriter rejects a ToWriter sink with no writer behind it: the copy
// would panic at the moment the audio was ready, after every byte of work.
func validateWriter(o Output) error {
	if o.kind == outputWriter && o.writer == nil {
		return fmt.Errorf("%w: ToWriter was given a nil writer", waxerr.ErrIncompatibleSpec)
	}
	return nil
}

// validateEnums refuses a value none of the constants spell, the way
// validateCoverArt does: allow-lists, not range checks, so a constant added
// later has to be listed here rather than slipping in under a bound.
func validateEnums(s ProcessSpec) error {
	if s.Transcode != nil {
		switch s.Transcode.Format {
		case FormatCopy, FormatFLAC, FormatALAC, FormatWAV, FormatMP3, FormatAAC, FormatOpus, FormatVorbis, FormatAIFF, FormatHEAAC, FormatWavPack, FormatAPE:
		default:
			return fmt.Errorf("%w: transcode format %d is not supported", waxerr.ErrIncompatibleSpec, s.Transcode.Format)
		}
	}
	if s.Cut != nil {
		switch s.Cut.Mode {
		case CutSmart, CutCopy, CutAccurate, CutCopyExact:
		default:
			return fmt.Errorf("%w: cut mode %d is not supported", waxerr.ErrIncompatibleSpec, s.Cut.Mode)
		}
		switch s.Cut.OnError {
		case ProceedUncut, FailDownload:
		default:
			return fmt.Errorf("%w: SponsorBlock error policy %d is not supported", waxerr.ErrIncompatibleSpec, s.Cut.OnError)
		}
	}
	if s.Loudness != nil {
		switch s.Loudness.Mode {
		case LoudnessMeasureOnly, LoudnessApply:
		default:
			return fmt.Errorf("%w: loudness mode %d is not supported", waxerr.ErrIncompatibleSpec, s.Loudness.Mode)
		}
		switch s.Loudness.PeakMode {
		case PeakCap, PeakLimit:
		default:
			return fmt.Errorf("%w: peak mode %d is not supported", waxerr.ErrIncompatibleSpec, s.Loudness.PeakMode)
		}
	}
	switch s.Channels {
	case LayoutAny, LayoutMono, LayoutStereo, LayoutSurround:
	default:
		return fmt.Errorf("%w: channel layout %d is not supported", waxerr.ErrIncompatibleSpec, s.Channels)
	}
	return nil
}

// validateRanges rejects a cut range the timeline cannot mean: a negative
// start, and an end at or before its start. Both leave nothing to keep, and
// the pipeline's clamp would silently drop them.
func validateRanges(c *CutSpec) error {
	if c == nil {
		return nil
	}
	for _, r := range c.Ranges {
		if r.Start < 0 {
			return fmt.Errorf("%w: cut range start %v must be >= 0", waxerr.ErrIncompatibleSpec, r.Start)
		}
		if r.End <= r.Start {
			return fmt.Errorf("%w: cut range %v-%v: end must be after start", waxerr.ErrIncompatibleSpec, r.Start, r.End)
		}
	}
	return nil
}

// validateCoverArt rejects an unknown CoverArt value, and a cover-art mode set
// without EmbedThumbnail. The second is a spec that asks to shape a picture that
// will never be fetched, so it is reported rather than silently ignored.
func validateCoverArt(s ProcessSpec) error {
	switch s.CoverArt {
	case CoverArtFrame, CoverArtSquare:
	default:
		return fmt.Errorf("%w: cover art mode %d is not supported (want CoverArtFrame or CoverArtSquare)",
			waxerr.ErrIncompatibleSpec, s.CoverArt)
	}
	if s.CoverArt != CoverArtFrame && !s.EmbedThumbnail {
		return fmt.Errorf("%w: CoverArt needs EmbedThumbnail; there is no cover picture to shape without it",
			waxerr.ErrIncompatibleSpec)
	}
	return nil
}

// validateOutputContainer rejects a file transcode when the output extension
// names a container that cannot hold the target codec. Extensionless, codec-
// named, and copy outputs are unconstrained. Writer sinks are not checked here
// because they stage with a derived extension.
func validateOutputContainer(s ProcessSpec) error {
	if s.Transcode == nil || s.Output.kind != outputFile {
		return nil
	}
	return media.CheckOutputContainer(transcodeCodec(s.Transcode.Format), s.Output.path)
}

// validateCutEncodeNeed rejects copy-mode cuts that cannot be described by the
// output path alone. Accurate cuts and crossfades require encoding, so copy mode
// needs an explicit target format. A plain copy cut can keep the source samples,
// but a file output still needs a container extension.
//
// The copy-plus-transcode contradiction is checked first, because the rest of the
// function only looks at specs with no transcode target.
//
// Beyond that, downmix is skipped here because the pipeline needs the probed
// channel count. When the source has more channels than the target, the pipeline
// chooses an encode after probing and the cut is valid without --format. When no
// fold is needed, the pipeline still applies its copy-mode checks before writing.
func validateCutEncodeNeed(s ProcessSpec) error {
	cut := cutRequested(s.Cut)
	target := transcodeCodec(specFormat(s.Transcode))
	// An explicit copy cut and a transcode target contradict each other: --format
	// re-encodes, which is what copy mode forbids. Reject rather than silently
	// dropping the copy request. FormatCopy is media.CodecCopy, so the coherent
	// cut-plus-remux case is not caught. This sits ahead of the s.Downmix term, so
	// --cut-mode copy --downmix --format flac fails here too, which is correct.
	if cut && (s.Cut.Mode == CutCopy || s.Cut.Mode == CutCopyExact) && target != media.CodecCopy {
		return fmt.Errorf("%w: --cut-mode %s cannot be combined with --format %s, which names an encode; drop one",
			waxerr.ErrIncompatibleSpec, cutModeName(s.Cut.Mode), target)
	}
	// Only Matroska states a trim per packet, which is what an exact interior
	// splice is written as, so the output has to be one.
	if cut && s.Cut.Mode == CutCopyExact {
		if s.Output.kind != outputFile || !exactSpliceExt(s.Output.path) {
			return fmt.Errorf("%w: copy-exact needs a .mka, .mkv, or .webm output, the containers that state a trim per packet", waxerr.ErrIncompatibleSpec)
		}
	}
	if !cut || s.Downmix || target != media.CodecCopy {
		return nil
	}
	switch {
	case s.Cut.Mode == CutAccurate:
		return fmt.Errorf("%w: accurate cut re-encodes; pass --format <format> (e.g. flac)", waxerr.ErrIncompatibleSpec)
	case s.Cut.Crossfade > 0:
		return fmt.Errorf("%w: crossfade re-encodes; pass --format <format> (e.g. flac)", waxerr.ErrIncompatibleSpec)
	case s.Output.kind == outputFile && copyCutNeedsExtension(s.Output.path):
		return fmt.Errorf("%w: cutting without re-encoding keeps the source codec, which needs a container extension on the output that can hold it (e.g. .opus/.m4a/.webm/.ogg/.mka), or pass --format to re-encode", waxerr.ErrIncompatibleSpec)
	}
	return nil
}

// cutModeName is the flag spelling of a copy mode, for a refusal that names
// what the caller actually passed.
func cutModeName(m CutMode) string {
	if m == CutCopyExact {
		return "copy-exact"
	}
	return "copy"
}

// exactSpliceExt reports whether path ends in a container that can state a
// trim per packet, which is what an exact interior splice is written as.
func exactSpliceExt(path string) bool {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "mka", "mkv", "webm":
		return true
	}
	return false
}

// copyCutNeedsExtension reports whether a stream-copy cut to path lacks a usable
// container extension. It mirrors the pipeline's runtime guard (ext "" or "copy").
func copyCutNeedsExtension(path string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	return ext == "" || ext == "copy"
}

// validateLoudness checks targets used for loudness application. Measure-only
// specs do not use a target.
func validateLoudness(s ProcessSpec) error {
	if err := validateLoudnessTarget(s.Loudness); err != nil {
		return err
	}
	l := s.Loudness
	if l == nil || l.Mode != LoudnessApply {
		return nil
	}
	// Applying a gain rewrites samples, which needs an encoder. A Downmix is
	// one: the pipeline promotes a copy spec to the source's own family to
	// fold it, and the gain rides that encode.
	if !s.Downmix && (s.Transcode == nil || s.Transcode.Format == FormatCopy) {
		return fmt.Errorf("%w: loudness apply requires an encode: a Transcode target other than copy, or a Downmix", waxerr.ErrIncompatibleSpec)
	}
	return nil
}

// validateLoudnessTarget checks the target alone, which is all an album has to
// offer: ProcessAlbum takes a bare target and chooses the encode itself.
func validateLoudnessTarget(l *LoudnessSpec) error {
	if l == nil || l.Mode != LoudnessApply {
		return nil
	}
	if math.IsNaN(l.Target) || math.IsInf(l.Target, 0) {
		return fmt.Errorf("%w: loudness target must be a finite LUFS value, got %v", waxerr.ErrIncompatibleSpec, l.Target)
	}
	if l.Target < loudnessTargetMin || l.Target > loudnessTargetMax {
		return fmt.Errorf("%w: loudness target %g LUFS is out of range [%g, %g]", waxerr.ErrIncompatibleSpec, l.Target, loudnessTargetMin, loudnessTargetMax)
	}
	return nil
}

// validateBitrate rejects a negative or implausibly high transcode bitrate. Zero
// selects the preset default.
func validateBitrate(t *TranscodeSpec) error {
	if t == nil {
		return nil
	}
	if t.Bitrate < 0 {
		return fmt.Errorf("%w: transcode bitrate must be >= 0, got %d", waxerr.ErrIncompatibleSpec, t.Bitrate)
	}
	// The bounds apply only where bitrate is used. It is ignored for lossless
	// and copy targets, so an out-of-range value there is harmless, not an error.
	if t.Bitrate > 0 && t.Bitrate < minPlausibleBitrate && !transcodeCodec(t.Format).IsLossless() {
		return fmt.Errorf("%w: transcode bitrate %d bps is implausibly low (min %d); bitrate is in bits per second, e.g. 128000 for 128 kbps", waxerr.ErrIncompatibleSpec, t.Bitrate, minPlausibleBitrate)
	}
	if t.Bitrate > maxBitrate && !transcodeCodec(t.Format).IsLossless() {
		return fmt.Errorf("%w: transcode bitrate %d bps is implausibly high (max %d)", waxerr.ErrIncompatibleSpec, t.Bitrate, maxBitrate)
	}
	return nil
}

// validateBitDepth rejects a requested output depth outside {0, 16, 24}.
//
// The honoring formats individually allow more (WAV and AIFF take 2..32, FLAC
// 4..32, ALAC 16/20/24/32), so this is policy, not a codec limit: 16 and 24 are
// the depths worth naming, and neither 32-bit integer nor the odd depths serve
// the reason the knob exists, which is forcing integer output from a float
// decode. The error says "want 16 or 24" so a rejected 32 does not read as a bug.
func validateBitDepth(t *TranscodeSpec) error {
	if t == nil {
		return nil
	}
	switch t.BitDepth {
	case 0, 16, 24:
		return nil
	default:
		return fmt.Errorf("%w: transcode bit depth %d is not supported (want 16 or 24, or 0 to follow the source)",
			waxerr.ErrIncompatibleSpec, t.BitDepth)
	}
}

// validateForce rejects Force beside FormatCopy. Force asks for Format's
// encoder and FormatCopy names none, so the pair says two things at once, the
// way a BitDepth outside the accepted set is refused on a preset that would
// have ignored it.
func validateForce(t *TranscodeSpec) error {
	if t == nil || !t.Force || t.Format != FormatCopy {
		return nil
	}
	return fmt.Errorf("%w: Force runs an encoder and FormatCopy runs none; drop one", waxerr.ErrIncompatibleSpec)
}

// downmixChannels returns the requested output channel count, or 0 when downmix
// is disabled. validateProcessSpec rejects layouts without a fixed count.
func downmixChannels(layout ChannelLayout, downmix bool) int {
	if !downmix {
		return 0
	}
	return layout.ChannelCount()
}

// cutMode maps a public CutMode to a media.Mode.
func cutMode(m CutMode) media.Mode {
	switch m {
	case CutCopy:
		return media.ModeCopy
	case CutCopyExact:
		return media.ModeCopyExact
	case CutAccurate:
		return media.ModeAccurate
	default:
		return media.ModeSmart
	}
}

// publicCutMode is cutMode's inverse, for reporting the mode a cut really ran
// in. ModeSmart never reaches a result: the pipeline resolves it to the mode
// that ran, so a smart cut reports the copy or the decode it chose.
func publicCutMode(m media.Mode) CutMode {
	switch m {
	case media.ModeCopy:
		return CutCopy
	case media.ModeCopyExact:
		return CutCopyExact
	default:
		return CutAccurate
	}
}

// pipelineSpec builds the internal pipeline spec from a ProcessSpec and the
// resolved removal ranges (explicit ranges plus any from SponsorBlock).
func pipelineSpec(s ProcessSpec, ranges []cutrange.Range) pipeline.Spec {
	ps := pipeline.Spec{Remove: ranges, Downmix: downmixChannels(s.Channels, s.Downmix)}
	if s.Cut != nil {
		ps.CutMode = cutMode(s.Cut.Mode)
		ps.Crossfade = s.Cut.Crossfade
		// Explicit ranges that do not intersect the media are rejected. Empty
		// SponsorBlock results are allowed and reported as a warning.
		ps.RejectEmptyRemoval = len(s.Cut.Ranges) > 0
	}
	if s.Transcode != nil {
		ps.Codec = transcodeCodec(s.Transcode.Format)
		ps.Bitrate = s.Transcode.Bitrate
		ps.BitDepth = s.Transcode.BitDepth
		// An explicit FormatCopy is a stream-copy remux (distinct from a nil
		// Transcode, which keeps the source bytes untouched).
		ps.Remux = s.Transcode.Format == FormatCopy
		// One answer for the pipeline's keep rule: the container's keep when
		// the caller named an extension it could not see the source for, the
		// codec's keep for every other named format, and none when the
		// caller asked for the encoder regardless.
		switch {
		case s.Transcode.Force:
			ps.Keep = pipeline.KeepNone
		case s.Transcode.FromContainer:
			ps.Keep = pipeline.KeepContainer
		default:
			ps.Keep = pipeline.KeepCodec
		}
	}
	if s.Loudness != nil {
		ps.Loudness = &pipeline.Loudness{
			Apply:     s.Loudness.Mode == LoudnessApply,
			Target:    s.Loudness.Target,
			PeakLimit: s.Loudness.PeakMode == PeakLimit,
		}
	}
	return ps
}

// sponsorBlockContributed reports whether SponsorBlock removed additional audio
// after clamping and merging. Segments that fall outside the media duration, or
// that are already covered by explicit ranges, do not count as applied work.
func sponsorBlockContributed(explicit, sbRanges []cutrange.Range, pres pipeline.Result) bool {
	if !pres.Cut || len(sbRanges) == 0 || pres.SourceDuration <= 0 {
		return false
	}
	total := pres.SourceDuration
	combined := append(append([]cutrange.Range{}, explicit...), sbRanges...)
	explicitKept := cutrange.OutputDuration(cutrange.Keeps(explicit, total), 0)
	combinedKept := cutrange.OutputDuration(cutrange.Keeps(combined, total), 0)
	return combinedKept < explicitKept
}

// cutRequested reports whether the spec asks for any cut (explicit ranges or a
// SponsorBlock fetch). A nil SponsorBlock slice disables the fetch.
func cutRequested(c *CutSpec) bool {
	return c != nil && (len(c.Ranges) > 0 || c.SponsorBlock != nil)
}

// warnEmptyCut reports a SponsorBlock-only request whose segments fell outside the
// media so nothing was removed. sbHadSegments says whether SponsorBlock returned
// any segments: when it returned none, collectRanges already emitted
// WarnSponsorBlockEmpty, so this stays silent to avoid a duplicate warning.
// Explicit ranges that do not intersect the media are rejected by the pipeline.
func warnEmptyCut(em *emitter, cs *CutSpec, pres pipeline.Result, sbHadSegments bool) {
	if cs != nil && cs.SponsorBlock != nil && sbHadSegments && len(cs.Ranges) == 0 && !pres.Cut && pres.SourceDuration > 0 {
		em.warn(WarnRangesEmpty, "SponsorBlock segments fell outside the media; delivered uncut")
	}
}

// warnCutSnapped reports the interior joins a packet copy moved inward to the
// packet grid. It fires on nearly every multi-span packet cut, SponsorBlock on
// Opus included: that is the point, since the alternative policy delivered
// audio from inside a removed span. Result.CutSnaps and Result.CutSnapMax
// carry the same figures for a consumer that wants numbers.
func warnCutSnapped(em *emitter, pres pipeline.Result) {
	if pres.CutSnaps == 0 {
		return
	}
	// The remedy has to fit the mode that ran. Under copy-exact the tails are
	// already exact and what moved is the heads, which no container can state
	// a trim for, so only a decode fixes them; telling that run to use
	// copy-exact would name the mode it is already in.
	remedy := "--cut-mode accurate re-encodes with exact joins, and copy-exact keeps the copy exact on a .mka/.webm output"
	if pres.CutMode == media.ModeCopyExact {
		remedy = "these are the heads, which no container can state a trim for, so only --cut-mode accurate places them exactly"
	}
	em.warn(WarnCutSnapped, fmt.Sprintf("the packet copy moved %s inward to the packet grid (largest move %s), so under one packet of wanted audio is missing at each; %s",
		plural(pres.CutSnaps, "join"), pres.CutSnapMax.Round(time.Millisecond), remedy))
}

// plural renders "1 join" / "3 joins"; the CLI's countOf is not importable here.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// warnBitrateAdjusted reports the rate the encoder really ran at when it is
// not the one the request named. The figure is WaxFlow's own plan of the
// encode, so the two cannot disagree.
func warnBitrateAdjusted(em *emitter, spec ProcessSpec, pres pipeline.Result) {
	if spec.Transcode == nil || spec.Transcode.Bitrate <= 0 || pres.EncodeBitRate <= 0 || pres.EncodeBitRate == spec.Transcode.Bitrate {
		return
	}
	em.warn(WarnBitrateAdjusted, fmt.Sprintf("the %s encoder cannot run at %d b/s; it encoded at %d b/s, the nearest rate it supports",
		pres.OutputCodec, spec.Transcode.Bitrate, pres.EncodeBitRate))
}

// loudnessMissWarnDB is the miss, in LU, that turns a loudness shortfall from a
// detail into something the user needs told. Below it the miss is within the noise
// of a lossy encode.
//
// It applies to the two single-pass policies (cap, and album mode in either
// peak mode), whose miss is a clamp computed up front rather than the residue of
// a search. Limit mode uses [loudness.ConvergeToleranceDB] instead; see
// warnLimiterTargetMissed for why the two thresholds differ.
const loudnessMissWarnDB = 1.0

// warnLoudnessTargetMissed reports that normalization did not reach the requested
// loudness. The two peak policies miss for different reasons, so each is detected
// where its cause actually lives.
//
// For PeakCap the cause is the true-peak clamp, and it is read from the clamp
// rather than from the achieved loudness on purpose: pipeline.Result.OutputLoudness
// is best-effort, so comparing against it would silently drop the warning whenever
// the post-measure fails, and a lossy encode can miss by more than a LU for
// reasons that have nothing to do with the ceiling, which would make the detail
// text a lie. Asking the loudness package what its clamp held back, on the
// InputLoudness that fed it (folded the way the encode folds, explicit downmix
// or the encoder's own), is deterministic and correctly attributed.
//
// For PeakLimit there is no clamp to attribute anything to: the gain aims at the
// target and the limiter gives back an amount only a measurement can reveal. So
// that branch is derived from the measured output, and stays silent when there is
// no usable measurement.
func warnLoudnessTargetMissed(em *emitter, ls *LoudnessSpec, pres pipeline.Result) {
	if ls == nil || ls.Mode != LoudnessApply {
		return
	}
	if ls.PeakMode == PeakLimit {
		warnLimiterTargetMissed(em, ls, pres)
		return
	}
	if pres.InputLoudness == nil {
		return
	}
	// PeakShortfall reports only what the true-peak clamp cost, so the detail below
	// can name that cause. It returns 0 for a non-finite measurement, so silence
	// (-Inf, which would otherwise be an infinite shortfall) stays quiet.
	short := loudness.PeakShortfall(ls.Target, *pres.InputLoudness)
	if short <= loudnessMissWarnDB {
		return
	}
	detail := fmt.Sprintf("true-peak capping at %g dBTP held the gain %.1f dB short of the %g LUFS target",
		loudness.TruePeakCeilingDB, short, ls.Target)
	if out := pres.OutputLoudness; out != nil && out.Finite() {
		detail += fmt.Sprintf("; delivered %.1f LUFS", out.IntegratedLUFS)
	}
	// The remedy stays worded as "closer" rather than "hits the target": limit
	// iterates onto the target but is still bounded by the limiter's saturation, so
	// promising the target is what produced this finding in the first place.
	// cmd/waxtap/batch_render.go aggregates on the code and surfaces this detail,
	// which is why the "--peak-mode limit" substring belongs in it.
	em.warn(WarnLoudnessTargetMissed, detail+"; use --peak-mode limit to get closer to the target")
}

// warnLimiterTargetMissed reports a limit-mode normalization the true-peak limiter
// held away from the target, in either direction: an overshoot is as much a
// silently wrong delivery as a shortfall, and the gain search can produce one.
//
// Its threshold is [loudness.ConvergeToleranceDB], not loudnessMissWarnDB, and
// the difference is the whole point of the mode. Limit is documented as
// iterating onto the target, and the search stops the moment it is inside that
// tolerance, so anything outside it is the search having given up rather than a
// miss too small to matter: a stop at 0.74 LU is precisely the case the mode
// promises to have converged and did not. The single-pass policies keep the
// wider threshold because their miss is a clamp they can name up front.
func warnLimiterTargetMissed(em *emitter, ls *LoudnessSpec, pres pipeline.Result) {
	out := pres.OutputLoudness
	if out == nil || !out.Finite() {
		return // no measurement, nothing honest to report
	}
	miss := ls.Target - out.IntegratedLUFS
	if math.Abs(miss) <= loudness.ConvergeToleranceDB {
		return
	}
	// Two wordings, because an overshoot is not something the limiter "held": a
	// shortfall is the limiter giving gain back, an overshoot is the gain search
	// stepping past the target.
	//
	// The count comes from the result rather than from maxLoudnessWrites, since
	// tolerance or saturation can end the search early. At this threshold a single
	// pass is reachable two ways - the corrected gain pinned at WaxFlow's clamp, so
	// the next write would encode the same file, and a correction write that failed
	// - so the plural is not safe and the noun agrees with the number.
	tmpl := "the true-peak limiter held the output %.1f LU short of the %g LUFS target after %s; delivered %.1f LUFS"
	if miss < 0 {
		tmpl = "normalization landed %.1f LU above the %g LUFS target after %s; delivered %.1f LUFS"
	}
	em.warn(WarnLoudnessTargetMissed, fmt.Sprintf(tmpl,
		math.Abs(miss), ls.Target, encodePasses(pres.LoudnessPasses), out.IntegratedLUFS))
}

// encodePasses renders a completed-pass count with a noun that agrees with it.
func encodePasses(n int) string {
	if n == 1 {
		return "1 encode pass"
	}
	return fmt.Sprintf("%d encode passes", n)
}

// warnOutputClipping surfaces the pipeline's level measurement: WaxFlow read
// the delivered encode past full scale, as clamped samples or as a true peak
// the stored samples only cross between themselves. The note is WaxFlow's
// wording; WaxTap adds the policy of when it is worth a warning and what to
// suggest.
//
// A source whose decode can overshoot never warns (decodeOvershoots). Such a
// decoder legitimately reconstructs past full scale on loud masters (a
// brickwalled release decodes with overs on most commercial music), so the
// clamp is inherent to any faithful integer conversion, and the post-clamp
// measurement carries no signal that could separate that from a defect: the
// meter taps the chain output after the quantizer, so the counts and peaks of
// an ordinary conversion look exactly like a real one. The defects this
// warning exists for (a float master stored past full scale, a normalization
// that attenuated but not enough) all read from sources whose decode stays in
// range, where a clipped sample is never the decoder's doing.
func warnOutputClipping(em *emitter, ls *LoudnessSpec, pres pipeline.Result) {
	note := pres.Levels.Note()
	if note == "" || decodeOvershoots(pres.SourceCodec) {
		return
	}
	em.warn(WarnOutputClipping, note+clipRemedy(ls, pres.Levels))
}

// minGateableDuration is the length of one EBU R128 momentary block. Integrated
// loudness is the gated mean of those blocks, so audio shorter than one block
// yields nothing to gate and has no integrated loudness to report: not a
// measurement that failed, but one that does not exist.
const minGateableDuration = 400 * time.Millisecond

// unmeasurableLoudnessCause explains a non-finite integrated loudness, or ""
// when l is nil or its integrated loudness is finite.
//
// d is the duration of the audio measured; d <= 0 means it is unknown, and the
// too-short cause is then not claimed rather than guessed at. empty says the
// track holds no frames, which the duration alone cannot distinguish from an
// unstated length. The order matters: a 200 ms silence is both too short and
// silent, and the length is the more useful thing to be told, because it is the
// one the user can change. Emptiness outranks both, being the only one of the
// three that is about the file rather than the signal in it.
func unmeasurableLoudnessCause(d time.Duration, empty bool, l *loudness.Loudness) string {
	if l == nil || !nonFiniteFloat(l.IntegratedLUFS) {
		return ""
	}
	switch {
	case empty:
		// Without this the zero duration skips the too-short branch and a
		// -Inf sample peak wins, calling a file with no frames "digital
		// silence" - which describes samples, of which there are none.
		return "the track contains no audio frames"
	case d > 0 && d < minGateableDuration:
		// Truncated, not rounded: a 399.7 ms clip must not render as "400ms,
		// shorter than the 400 ms block".
		return fmt.Sprintf("the clip is %s, shorter than the 400 ms block EBU R128 gating needs", d.Truncate(time.Millisecond))
	case math.IsInf(l.SamplePeakDB, -1):
		return "the audio is digital silence"
	default:
		return "the signal stays below the R128 gates (under -70 LUFS, or the relative gate removed every block)"
	}
}

// nonFiniteFloat reports whether v is NaN or infinite, the two shapes an
// unusable measurement arrives in.
func nonFiniteFloat(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }

// warnLoudnessUnmeasurable reports each measured side whose integrated loudness
// came back non-finite, so the nulls in --json and the "n/a" in the human
// output arrive explained rather than merely blank.
//
// Both sides are reported when both are unusable. They fail for the same reason
// here but not always (a cut can leave an output shorter than its input), and
// a reader checking that a normalization landed reads the output line.
func warnLoudnessUnmeasurable(em *emitter, pres pipeline.Result) {
	if pres.LoudnessMeasured && pres.InputLoudness != nil {
		// The meter's own read length decides the too-short case: it is what the
		// gate actually saw, where the container's declared duration can overstate
		// it (a cut, or a decode that ended early on a damaged file).
		d := pres.InputLoudness.Duration
		if d == 0 {
			d = pres.SourceDuration - pres.Removed
		}
		if cause := unmeasurableLoudnessCause(d, pres.SourceEmpty, pres.InputLoudness); cause != "" {
			em.warn(WarnLoudnessUnmeasurable, "input integrated loudness could not be measured: "+cause)
		}
	}
	// Gated on the measurement, not on LoudnessApplied: an unmeasurable input
	// leaves LoudnessApplied false (no gain could apply) on exactly the runs whose
	// output is also unmeasurable, which is the pair this warning exists to
	// explain. OutputLoudness is only ever set when normalization was requested.
	if pres.OutputLoudness != nil {
		d := pres.OutputLoudness.Duration
		if d == 0 && pres.OutputProbe != nil {
			d = pres.OutputProbe.Format.Duration
		}
		// An empty input yields an empty output, so the same fact explains both
		// sides; nothing else here can observe the output's frame count.
		if cause := unmeasurableLoudnessCause(d, pres.SourceEmpty, pres.OutputLoudness); cause != "" {
			em.warn(WarnLoudnessUnmeasurable, "output integrated loudness could not be measured: "+cause)
		}
	}
}

// warnUnboundSourcePolicy reports a prefer:<codec> that named a codec family
// this video does not carry. Such a policy is inert rather than wrong, and an
// inert one is invisible: the delivery is byte-for-byte the run the user would
// have got with no policy at all, so nothing distinguishes "the preference was
// honored" from "the preference never applied".
//
// A preference that was present but outranked stays silent. Ranking a better
// source above a preferred codec is what the soft bias documents itself as
// doing, so warning there would fire on correct behavior.
func warnUnboundSourcePolicy(em *emitter, policy SourcePolicy, formats []Format, chosen Format, verb string) {
	want := policy.Preferred()
	if want == "" {
		return
	}
	// The selector's own eligibility rule decides what counts as available, so
	// a family carried only by a format selection would never pick cannot
	// silence the warning.
	available := format.AvailableFamilies(formats)
	if slices.Contains(available, want) {
		return
	}
	have := strings.Join(available, ", ")
	if have == "" {
		have = "none reported"
	}
	em.warn(WarnSourcePolicyUnmatched, fmt.Sprintf(
		"--source-policy prefer:%s matched no available source (available codecs: %s); %s %s",
		want, have, verb, codecOrUnknown(chosen.Codec)))
}

// codecOrUnknown names a delivered codec for a warning, standing in when the
// player response omitted it.
func codecOrUnknown(codec string) string {
	if fam := format.CodecFamily(codec); fam != "" {
		return fam
	}
	return "an unnamed codec"
}

// warnInputDamage reports a local input the decoder had to work around, so a
// short output is explained rather than merely delivered. The run succeeds:
// the audio that read is real audio, and the only alternative is refusing a
// file the user can still use. The engine's remarks on an input that is not
// damaged go out beside it under their own code.
//
// Only local processing calls this. A YouTube delivery cannot produce it (the
// containers on that path either probe exactly or fail outright), and firing it
// there would blame the user's input for a delivery of ours that came up short.
func warnInputDamage(em *emitter, pres pipeline.Result) {
	if note := inputDamageNote(pres.SourceWarnings); note != "" {
		em.warn(WarnInputDamage, note)
	}
	if note := inputNote(pres.SourceNotes); note != "" {
		em.warn(WarnInputNote, note)
	}
}

// warnEmptyInput reports a local input carrying no audio frames at all. The run
// succeeds: an empty input converts faithfully to an empty output, and failing
// would take a batch down over one file the user can see for themselves.
//
// It fires alongside the loudness warning rather than instead of it. That one
// explains why a number is null; this one says the file had nothing in it,
// which is the fact a caller with no loudness request would otherwise never be
// told.
//
// Only local processing calls this, for warnInputDamage's reason: a delivery of
// ours coming back empty is our failure to report as one, not the user's input
// to warn about.
func warnEmptyInput(em *emitter, pres pipeline.Result) {
	if pres.SourceEmpty {
		em.warn(WarnEmptyInput, "the input contains no audio frames; the output holds no audio")
	}
}

// inputDamageNote renders the source's damage notes as one detail line, or ""
// when there are none: the verdict, then the notes in the decoder's (or the
// short-decode check's) exact words. The verdict is honest now that the list
// holds damage alone. While the engine folded its remarks on well-formed files
// into the same list (an extra stream ignored, a trailing tag skipped, a
// rescaled timescale), a lead like this lied about half the entries and the
// detail was the bare notes; those remarks are inputNote's now.
//
// The notes are copied because capNotes truncates in place and the probe's
// slice belongs to its caller.
func inputDamageNote(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return "the input is damaged: " + strings.Join(capNotes(slices.Clone(notes)), "; ")
}

// inputNote renders the engine's remarks on a well-formed input as one detail
// line, or "" when there are none. No lead: the warning's own code says these
// are not damage, and each remark names what the engine did in its own words.
func inputNote(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return strings.Join(capNotes(slices.Clone(notes)), "; ")
}

// mergeRemarks appends each found list to have, each line once, for a track
// whose damage was read more than once: the probe reports the headers' share,
// and the measurement and the encode each report the whole, the probe's
// entries included.
func mergeRemarks(have []string, found ...[]string) []string {
	out := slices.Clone(have)
	for _, list := range found {
		for _, w := range list {
			if !slices.Contains(out, w) {
				out = append(out, w)
			}
		}
	}
	return out
}

// lossySource reports whether the probed source codec name is a lossy family:
// a transform codec, or a companded or ADPCM coding, which carries less than
// the PCM it came from all the same. It is losslessSource's counterpart for
// classifying every decoder the engine registers (TestSourceCodecClassParity);
// the clipping warnings ask the narrower question decodeOvershoots answers.
func lossySource(codec string) bool {
	switch codec {
	case "opus", "aac", "he-aac", "mp3", "vorbis", "wma", "wmapro", "wmavoice", "musepack",
		"alaw", "mulaw", "ima-adpcm", "ms-adpcm":
		return true
	}
	return false
}

// decodeOvershoots reports whether a source's decode can legitimately land
// past full scale, which is what makes an output clip on it uninformative (see
// warnOutputClipping): the transform codecs do, on any loud master. The
// companded and ADPCM codings are lossy too, but they decode to whole 16-bit
// samples that never leave the range, so a clip after one is the gain's doing
// and worth the warning, exactly as on a lossless source.
func decodeOvershoots(codec string) bool {
	switch codec {
	case "alaw", "mulaw", "ima-adpcm", "ms-adpcm":
		return false
	}
	return lossySource(codec)
}

// losslessSource reports whether the probed source codec name is a lossless
// family. It is not lossySource's complement: an unknown codec is neither, so
// each warning that keys on the distinction fails closed rather than firing on
// a codec it cannot classify. TestSourceCodecClassParity pins both tables to
// media.Codec.IsLossless.
func losslessSource(codec string) bool {
	switch codec {
	case "flac", "alac", "wavpack", "ape", "wav", "aiff", "wmalossless":
		return true
	}
	return strings.HasPrefix(codec, "pcm")
}

// warnImplicitLossy reports a lossy re-encode the request never named: the
// spec asked for a copy (or nothing at all), and automatic processing promoted
// it to the output container's default encoder because the source codec cannot
// enter that container. A cut of in.wv written to out.mka re-encodes to Opus
// this way, correctly, and used to say so only in the result's codec field.
//
// A lossy source counts too. It loses a generation rather than its first, and
// the request said no more about it than it did about a lossless one; an MP3
// cut into a .mka is a second generation nobody asked for. A request that
// names an encode took its cost knowingly and does not warn.
func warnImplicitLossy(em *emitter, spec ProcessSpec, pres pipeline.Result) {
	if namedAnEncode(spec.Transcode) {
		return
	}
	if !pres.Transcoded || pres.OutputCodec.IsLossless() {
		return
	}
	// The claim below is that the container could not carry the source, so it
	// is checked rather than inferred from how the request was built. A caller
	// that could not learn the source codec up front (a URL, whose codec only
	// selection settles) leaves the same flags set as one whose container
	// really does refuse it, and this warning would then tell a user that Opus
	// cannot enter a Matroska file.
	if ext := strings.TrimPrefix(filepath.Ext(pres.OutputPath), "."); ext != "" &&
		media.ContainerAccepts(ext, pres.SourceCodec) {
		return
	}
	var detail string
	if losslessSource(pres.SourceCodec) {
		detail = fmt.Sprintf("the request named no encode, but %s audio cannot enter the output container, so it was re-encoded to %s (lossy)",
			pres.SourceCodec, pres.OutputCodec)
	} else {
		detail = fmt.Sprintf("the request named no encode, but %s audio cannot enter the output container, so it was re-encoded to %s: a second lossy generation",
			pres.SourceCodec, pres.OutputCodec)
	}
	if exts := media.ContainersFor(pres.SourceCodec); len(exts) > 0 {
		detail += fmt.Sprintf("; keep the codec with a matching extension (%s) or pass a lossless --format", strings.Join(exts, "/"))
	} else {
		detail += "; pass a lossless --format to avoid the quality loss"
	}
	em.warn(WarnImplicitLossy, detail)
}

// warnGaplessDropped reports a copy whose destination states no gapless
// trim, so the samples the source trimmed are delivered as audio.
//
// Only the halves the source states are named. A zero is not reported as a
// zero: a container that carries its end trim on the last packet rather than
// in its headers (Matroska) states none until a walk has read that packet,
// and "0 of padding" would be a claim rather than a silence (see
// media.Trim).
func warnGaplessDropped(em *emitter, pres pipeline.Result) {
	d := pres.TrimDropped
	var what string
	switch {
	case d.Delay > 0 && d.Padding > 0:
		what = fmt.Sprintf("the %d samples of encoder delay and %d of padding the source trimmed", d.Delay, d.Padding)
	case d.Delay > 0:
		what = fmt.Sprintf("the %d samples of encoder delay the source trimmed", d.Delay)
	case d.Padding > 0:
		what = fmt.Sprintf("the %d samples of padding the source trimmed", d.Padding)
	default:
		return
	}
	ext := strings.TrimPrefix(filepath.Ext(pres.OutputPath), ".")
	em.warn(WarnGaplessDropped, fmt.Sprintf("the .%s container states no gapless trim, so %s play as audio; keep the trim with .m4a, or re-encode", ext, what))
}

// warnCutDecoded reports a cut that would have copied packets and decoded
// instead, into a lossy encoder. A lossless fallback (a FLAC or WavPack cut
// is bit exact) says nothing, and a decode the request named (accurate
// mode, a crossfade, a format the source was not in) never tried a copy.
func warnCutDecoded(em *emitter, pres pipeline.Result) {
	if pres.CutDeclined == media.NotDeclined || pres.OutputCodec.IsLossless() {
		return
	}
	var why string
	fix := "a lossless --format loses nothing further, and --cut-mode copy refuses instead"
	switch pres.CutDeclined {
	case media.DeclinedCodec:
		why = fmt.Sprintf("%s packets cannot be cut in place (%s can)", pres.SourceCodec, strings.Join(media.CutFormats(), " and "))
	case media.DeclinedStreamStart:
		why = "an HE-AAC packet cut must keep the stream start"
	case media.DeclinedContainer:
		why = "raw ADTS (.aac) cannot state the trims this cut needs"
		fix = "name .m4a or .mka to copy the packets"
	default:
		why = "the packet copy declined this cut's shape (a span must hold a whole packet, and the packet walk must index the source)"
	}
	em.warn(WarnCutDecoded, fmt.Sprintf("the cut decoded and re-encoded the audio as %s, a second lossy generation, because %s; %s", pres.OutputCodec, why, fix))
}

// warnRendered raises the warnings any finished pipeline run can carry,
// whatever its source kind. The two callers add what only their kind knows
// (an empty cut's SponsorBlock half; a local input's damage and emptiness),
// so a warning added here reaches both.
func warnRendered(em *emitter, spec ProcessSpec, pres pipeline.Result) {
	warnCutSnapped(em, pres)
	warnCutDecoded(em, pres)
	warnLoudnessTargetMissed(em, spec.Loudness, pres)
	warnImplicitDownmix(em, spec, pres)
	warnImplicitLossy(em, spec, pres)
	warnGaplessDropped(em, pres)
	warnBitrateAdjusted(em, spec, pres)
	warnOutputClipping(em, spec.Loudness, pres)
}

// namedAnEncode reports whether the caller asked for the encoder that ran. A
// nil spec asks for nothing, FormatCopy asks for a copy, and a format taken
// from the output's container (TranscodeSpec.FromContainer) is the container's
// choice rather than the caller's; anything else is a target they named, and a
// forced encode is the caller's whatever chose the format.
func namedAnEncode(t *TranscodeSpec) bool {
	if t == nil || (t.FromContainer && !t.Force) {
		return false
	}
	return transcodeCodec(t.Format) != media.CodecCopy
}

// clipRemedy picks the suffix for an output-clipping detail: the knob the run
// has not turned yet, or nothing when every knob it has is already turned.
func clipRemedy(ls *LoudnessSpec, l media.Levels) string {
	if ls == nil || ls.Mode != LoudnessApply {
		// The stored samples of a true-peak-only over are all in range, so the
		// remedy names the true peak rather than telling the user their level is
		// past full scale.
		what := "the level"
		if l.ClippedSamples == 0 {
			what = "the true peak"
		}
		return "; normalize to bring " + what + " under full scale"
	}
	if ls.PeakMode == PeakLimit {
		// Limit mode only runs the limiter on a boosting gain, so an attenuating
		// pass on a hot source can still clip. Cap derives its clamp from the
		// measured peak instead, which is the knob left to turn.
		return clipRemedyCap
	}
	// Cap already held everything it could see; suggesting more would point at
	// knobs already turned.
	return ""
}

// clipRemedyCap is the remedy for a normalization that still clipped: shared
// with the album path, whose runs are always normalizing.
const clipRemedyCap = "; use --peak-mode cap to hold the true peak under the ceiling"

// warnImplicitDownmix reports a channel fold the request never asked for: a
// lossy encoder that cannot hold the source layout folds it to stereo, correctly,
// and a run that halves a 5.1 master used to exit 0 with an empty warnings array
// and nothing but a raw WaxFlow log line on stderr to say so.
//
// It reads WaxTap's own probes rather than parsing that log line: SourceChannels
// is what the pipeline measured on the input and OutputProbe is what it measured
// on the written file, both already authoritative for everything else the result
// reports. A Downmix request means the caller chose the fold and does not need
// telling.
func warnImplicitDownmix(em *emitter, spec ProcessSpec, pres pipeline.Result) {
	if spec.Downmix || pres.SourceChannels <= 0 || pres.OutputProbe == nil {
		return
	}
	out, ok := pres.OutputProbe.AudioStream()
	if !ok || out.Channels <= 0 || out.Channels >= pres.SourceChannels {
		return
	}
	em.warn(WarnImplicitDownmix, fmt.Sprintf(
		"%s cannot hold %d channels, so the encode folded them to %d; pass --downmix to choose the fold, or a format that keeps the layout",
		outputCodecLabel(pres.OutputCodec), pres.SourceChannels, out.Channels))
}

// outputCodecLabel names the encoder a fold is attributable to, falling back to
// neutral wording for a container copy that carries no codec of its own.
func outputCodecLabel(c media.Codec) string {
	if c == media.CodecCopy {
		return "the output format"
	}
	return c.String()
}

// warnAlbumTargetMissed reports an album normalization that did not reach the
// requested loudness. Album mode used to emit no warning at all: it never calls
// em.warn, and warnLimiterTargetMissed returns early without a measured output,
// which album mode did not produce. A 1.16 LU miss on a -14 target was therefore
// silent, past the same threshold that warns loudly on a single file.
//
// The split mirrors warnLoudnessTargetMissed, and for the same reasons: the cap
// branch attributes the miss to the clamp it can compute deterministically, the
// limit branch reads it off the delivered album because there is no clamp to
// attribute anything to. What differs is the remedy, since album mode is a single
// pass by design and has no gain search to have given up.
func warnAlbumTargetMissed(em *emitter, target float64, mode PeakMode, album loudness.Loudness, perTrack []loudness.Loudness, delivered *LoudnessInfo) {
	deliveredLUFS := func() (float64, bool) {
		if delivered == nil || math.IsInf(delivered.IntegratedLUFS, 0) || math.IsNaN(delivered.IntegratedLUFS) {
			return 0, false
		}
		return delivered.IntegratedLUFS, true
	}

	if mode == PeakCap {
		short := loudness.AlbumPeakShortfall(target, album, perTrack)
		if short <= loudnessMissWarnDB {
			return
		}
		detail := fmt.Sprintf("true-peak capping at %g dBTP held the album gain %.1f dB short of the %g LUFS target",
			loudness.TruePeakCeilingDB, short, target)
		if lufs, ok := deliveredLUFS(); ok {
			detail += fmt.Sprintf("; delivered %.1f LUFS", lufs)
		}
		// The trade is stated because it is the whole reason both modes exist: limit
		// gets closer, and gives up the exact spacing that cap was chosen for.
		em.warn(WarnLoudnessTargetMissed, detail+"; the loudest track sets the album's headroom, so drop --peak-mode cap to get closer to the target at the cost of the exact track-to-track spacing")
		return
	}

	lufs, ok := deliveredLUFS()
	if !ok {
		return // no measurement, nothing honest to report
	}
	miss := target - lufs
	// Album mode keeps loudnessMissWarnDB where the single-file limit branch
	// dropped to the converge tolerance, and so leaves a band where a miss
	// neither converges further nor warns: a 0.7 LU album miss is silent. That
	// is the same shape as the finding this warning exists to close, so it is
	// written down rather than left for the next end-to-end pass to rediscover.
	// It stands because album mode has no gain search: one uniform pass is the
	// design, nothing here ever aimed at 0.3 LU, and a sub-LU miss is inside the
	// noise of the encode. The single-file limit path warns tighter precisely
	// because it does iterate and stopping short means it gave up.
	if math.Abs(miss) <= loudnessMissWarnDB {
		return
	}
	tmpl := "the true-peak limiter held the album %.1f LU short of the %g LUFS target; delivered %.1f LUFS"
	if miss < 0 {
		tmpl = "the normalized album landed %.1f LU above the %g LUFS target; delivered %.1f LUFS"
	}
	// No "encode passes" count and no offer to iterate: album mode applies one
	// uniform gain in one pass on purpose, because correcting per track is exactly
	// the per-track variation it exists to remove.
	em.warn(WarnLoudnessTargetMissed, fmt.Sprintf(tmpl, math.Abs(miss), target, lufs)+
		"; album mode is a single uniform-gain pass and cannot iterate onto the target the way a single file does")
}

// needsProcessing reports whether the spec needs audio processing and a staged input. When
// false, a download can stream straight to the sink with no temp file. Any
// non-nil Transcode counts, including an explicit FormatCopy remux (distinct from
// a nil Transcode, which keeps the source bytes). A downmix request also counts:
// the fold needs a probe to decide and an encode to apply. An embed request also
// counts: the metadata post-pass rewrites a staged file, so even a keep-source
// download to a Writer stages first.
func needsProcessing(s ProcessSpec) bool {
	return cutRequested(s.Cut) || s.Transcode != nil || s.Loudness != nil || s.Downmix || embedRequested(s)
}

// toSource maps a resolved stream to a download Source, selecting the query-range
// strategy for googlevideo media hosts (which answer &range= with a 200) and the
// default header-range strategy elsewhere.
func toSource(rs youtube.ResolvedStream) download.Source {
	src := download.Source{
		URL:           rs.URL,
		ContentLength: rs.ContentLength,
		Headers:       rs.Headers,
		ExpiresAt:     rs.ExpiresAt,
	}
	if isGoogleVideoHost(rs.URL) {
		src.RangeStrategy = download.QueryRange{}
	}
	return src
}

// isGoogleVideoHost reports whether rawURL points at a googlevideo media host.
func isGoogleVideoHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Hostname()), "googlevideo.com")
}

// newProcessResult builds a Result from a pipeline outcome and the source format.
// target is the loudness target (used only when loudness was measured/applied).
func newProcessResult(kind SourceKind, p pipeline.Result, srcFmt Format, target float64) *Result {
	res := &Result{
		SourceKind:       kind,
		SourceFormat:     srcFmt,
		OutputFormat:     srcFmt,
		Transcoded:       p.Transcoded,
		CutApplied:       p.Cut,
		CutSnaps:         p.CutSnaps,
		CutSnapMax:       p.CutSnapMax,
		LoudnessMeasured: p.LoudnessMeasured,
		LoudnessApplied:  p.LoudnessApplied,
	}
	if p.Transcoded {
		res.OutputFormat = outputFormat(p.OutputCodec, srcFmt)
		// The container the file is really in, when the caller named one.
		// outputFormat derives the extension from the codec's usual container
		// (Opus gives ".opus"), which is right for a path that named none and
		// wrong for one that did: an encode into a .mka would otherwise be
		// reported as an .opus that is nowhere on disk.
		if ext := strings.TrimPrefix(filepath.Ext(p.OutputPath), "."); ext != "" {
			res.OutputFormat.Extension = strings.ToLower(ext)
		}
	} else if ext := strings.TrimPrefix(filepath.Ext(p.OutputPath), "."); p.OutputPath != "" && ext != "" {
		// A copy keeps the codec and takes the container the output was
		// named for, which is the one it was written into: the pipeline
		// copies only into a container the extension names, so the source's
		// extension would name a container the output is not in (an MP3
		// carried in a WAV lands in a bare .mp3, Opus in WebM in .mka).
		// Reported the way the source's own is, as the file's extension (see
		// the local Format Process builds), rather than as the engine's
		// container name, which is not an extension ("ogg" for an .opus).
		res.OutputFormat.Extension = strings.ToLower(ext)
	}
	if p.Cut {
		res.CutMode = publicCutMode(p.CutMode)
		// A cut shrinks the output. For a copy cut OutputFormat is still srcFmt, whose
		// Duration and ContentLength describe the uncut source; for a fused cut+encode
		// it is the codec/extension target with zero numerics. Either way, set the
		// post-cut duration as a baseline (a probe supersedes it) and clear
		// ContentLength: the cut byte size is unknown without a probe, and the source
		// size would be wrong.
		if d := p.SourceDuration - p.Removed; d > 0 {
			res.OutputFormat.Duration = d
		} else {
			res.OutputFormat.Duration = 0
		}
		res.OutputFormat.ContentLength = 0
	}
	if p.OutputProbe != nil {
		// Overlay authoritative rate/channels/bitrate/duration/size from the
		// written file.
		applyProbe(&res.OutputFormat, *p.OutputProbe)
	}
	if p.LoudnessMeasured {
		res.Loudness = &LoudnessResult{
			Input:      toLoudnessInfo(p.InputLoudness),
			Output:     toLoudnessInfo(p.OutputLoudness),
			Target:     target,
			GainDB:     p.GainDB,
			HeaderGain: p.GainInHeader,
		}
	}
	return res
}

// applyProbe fills a candidate Format with authoritative values from a probe of
// its resolved stream (InfoProbe depth) or written output. It overwrites only the
// measured numeric fields and duration, leaving the codec id from the player
// response, which is more specific than the probe's normalized name.
func applyProbe(f *Format, pr media.ProbeResult) {
	if a, ok := pr.AudioStream(); ok {
		if a.SampleRate > 0 {
			f.SampleRate = a.SampleRate
		}
		if a.Channels > 0 {
			f.Channels = a.Channels
		}
		if a.Duration > 0 {
			f.Duration = a.Duration
		}
	}
	if pr.Format.Duration > 0 {
		f.Duration = pr.Format.Duration
	}
	if pr.Format.Size > 0 {
		f.ContentLength = pr.Format.Size
	}
	// A probe often leaves the audio-stream bitrate zero for VBR/lossless. Fall
	// back to a size/duration estimate, so both the info --probe row and a
	// download's OutputFormat report a usable bitrate.
	if f.Bitrate == 0 {
		if secs := f.Duration.Seconds(); secs > 0 && pr.Format.Size > 0 {
			f.Bitrate = int(float64(pr.Format.Size) * 8 / secs)
		}
	}
}

// outputFormat describes the transcode output. A copy keeps the source format;
// otherwise the codec and extension come from the target codec's preset.
func outputFormat(c media.Codec, src Format) Format {
	if c == media.CodecCopy {
		return src
	}
	return Format{Codec: c.String(), Extension: c.Extension()}
}

// toLoudnessInfo maps an internal loudness measurement to the public info type,
// preserving nil.
func toLoudnessInfo(l *loudness.Loudness) *LoudnessInfo {
	if l == nil {
		return nil
	}
	v := loudnessInfo(*l)
	return &v
}

// loudnessInfo maps an internal loudness value to the public info type.
func loudnessInfo(l loudness.Loudness) LoudnessInfo {
	return LoudnessInfo{
		IntegratedLUFS: l.IntegratedLUFS,
		TruePeakDBTP:   l.TruePeakDBTP,
		LRA:            l.LRA,
		SamplePeakDB:   l.SamplePeakDB,
	}
}

// withTimeout derives a child context bounded by d. A non-positive d returns the
// parent with a no-op cancel, so callers can always defer cancel.
func withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, d)
}

// specFormat returns the requested output format, or FormatCopy for a nil spec.
func specFormat(t *TranscodeSpec) TranscodeFormat {
	if t == nil {
		return FormatCopy
	}
	return t.Format
}

// sourceExt returns the staging extension for a downloaded source format,
// defaulting to .webm when the format carries no extension.
func sourceExt(f Format) string {
	if f.Extension != "" {
		return "." + f.Extension
	}
	return ".webm"
}

// outputExt returns the extension the processed output should use: the target
// codec's extension for a re-encode, or the source extension for a copy.
func outputExt(t *TranscodeSpec, srcExt string) string {
	c := transcodeCodec(specFormat(t))
	if c == media.CodecCopy {
		return srcExt
	}
	return "." + c.Extension()
}

// makeJobDir creates a per-job directory under TempDir. A configured TempDir is
// created first when necessary.
func (c *Client) makeJobDir() (string, error) {
	if c.opts.TempDir != "" {
		if err := os.MkdirAll(c.opts.TempDir, 0o777); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(c.opts.TempDir, "waxtap-job-*")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// rejectSeparatorPath rejects an output path ending in a path separator, which
// names a directory rather than a file. filepath.Dir("d/a.wav/") is "d/a.wav",
// so letting one through to MkdirAll created a DIRECTORY named after the file
// the caller asked for and left it there forever, with every collision and
// overwrite check downstream reasoning about a directory.
//
// It rejects rather than trimming: silently retargeting an output path is worse
// than refusing one, since the caller then cannot tell where the file went. The
// error wraps ErrIncompatibleSpec, never WrapOutput, so it classifies as the
// bad request it is rather than as a filesystem failure.
//
// Process and Download call it ahead of their SkipIfExists checks, because a
// directory already sitting at the bad path stats as existing and the skip
// would otherwise answer "already done" to a request that never named a file.
// ensureParentDir repeats it as the backstop for every path that reaches a
// mkdir without passing those seams (ProcessAlbum's tracks funnel through it).
func rejectSeparatorPath(path string) error {
	if path != "" && os.IsPathSeparator(path[len(path)-1]) {
		return fmt.Errorf("%w: output path %q ends in a path separator, which names a directory rather than a file", waxerr.ErrIncompatibleSpec, path)
	}
	return nil
}

// ensureParentDir creates the parent directory for an output path. The caller's
// umask controls permissions; private internal directories use stricter modes.
// For a bare filename, filepath.Dir returns "." and MkdirAll is a no-op.
func ensureParentDir(path string) error {
	if err := rejectSeparatorPath(path); err != nil {
		return err
	}
	return tempfile.WrapOutput("mkdir", os.MkdirAll(filepath.Dir(path), 0o777))
}

// sameFile reports whether two paths refer to the same file, falling back to an
// absolute-path comparison when either does not yet exist.
func sameFile(a, b string) bool {
	fa, ea := os.Stat(a)
	fb, eb := os.Stat(b)
	if ea == nil && eb == nil {
		return os.SameFile(fa, fb)
	}
	pa, e1 := filepath.Abs(a)
	pb, e2 := filepath.Abs(b)
	return e1 == nil && e2 == nil && pa == pb
}

// streamFileTo copies path to w and returns the byte count.
func streamFileTo(w io.Writer, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return io.Copy(w, f)
}

// copyFile copies src to dst atomically (temp + rename in dst's directory) and
// returns the path it published. out's exclusivity decides whether an occupied
// destination fails with fs.ErrExist, renumbers, or is replaced.
func copyFile(src, dst string, out Output) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	tf, err := tempfile.New(dst)
	if err != nil {
		return "", err
	}
	defer tf.Discard()
	if _, err := io.Copy(tf, in); err != nil {
		return "", err
	}
	switch {
	case out.renumber:
		return tf.CommitNewNumbered()
	case out.exclusive:
		return dst, tf.CommitNew()
	default:
		return dst, tf.Commit()
	}
}

// moveFile renames src to dst, falling back to a copy when they live on different
// filesystems (a temp dir versus the destination).
func moveFile(src, dst string) error {
	// Through a symlinked destination, as tempfile's own staged publish goes:
	// this is the same publish reached by a different route (a staging file on
	// another filesystem), and without it whether the link survives would
	// depend on which filesystem the job directory happened to be on.
	dst = tempfile.ResolveLink(dst)
	// A replacing publish keeps the destination's permission bits: overwriting a
	// file replaces its content, which is what was asked for, not the mode its
	// owner chose. tempfile does the same on the paths that go through it.
	srcMode := fs.FileMode(0)
	if fi, err := os.Stat(src); err == nil {
		srcMode = fi.Mode().Perm()
	}
	tempfile.PreserveReplacedMode(src, dst)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// The rename failed (typically EXDEV), so src still exists and now carries
	// dst's mode, which may lack owner read (a 0200 destination). Restore its
	// own mode so the copy fallback can open it; that path preserves dst's mode
	// itself, inside tempfile's commit.
	if srcMode != 0 {
		_ = os.Chmod(src, srcMode)
	}
	if _, err := copyFile(src, dst, Output{kind: outputFile, path: dst}); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

// moveFileNew is moveFile for a destination that must not already exist. A src
// on the destination's filesystem publishes with a hard link; anything else
// (a job temp dir on another device) is copied into the destination directory
// first, so the final step is exclusive either way. It returns the published
// path, which differs from out.path only when out renumbers.
func moveFileNew(src string, out Output) (string, error) {
	published, err := publishNewMaybeNumbered(src, out)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return published, err
	}
	published, cerr := copyFile(src, out.path, out)
	if cerr != nil {
		return "", cerr
	}
	_ = os.Remove(src)
	return published, nil
}

// publishNewMaybeNumbered claims out's destination for src, renumbering when out
// asks for it. It is the one place the two exclusive publishes are chosen
// between.
func publishNewMaybeNumbered(src string, out Output) (string, error) {
	if out.renumber {
		return tempfile.PublishNewNumbered(src, out.path)
	}
	return out.path, tempfile.PublishNew(src, out.path)
}

// deliverFile publishes the produced file src as out's path, honoring out's
// exclusivity, and returns the path it delivered to. Callers that let a producer
// write the destination directly do not reach it.
func deliverFile(src string, out Output) (string, error) {
	if out.exclusive {
		return moveFileNew(src, out)
	}
	return out.path, moveFile(src, out.path)
}

// warnName is the file name to use in a warning about a written output. A
// staged delivery writes a temp path the user never asked for and will never
// see, so warnings name the destination instead. An empty dest (a writer sink,
// which has no path) falls back to the file actually written.
func warnName(dest, written string) string {
	if dest == "" {
		dest = written
	}
	return filepath.Base(dest)
}

// publishProduced delivers the file a producer wrote to out. It is the one
// statement of "publish the staged file, or move the produced one, never both",
// shared by Process, deliverSource, and downloadAndProcess so the three cannot
// drift into meaning different things.
//
// deliver must be the path the producer actually wrote. A caller whose producer
// may deliver something it must not move (Client.Process hands back the
// untouched input for a measure-only pass) handles that case before calling.
// It returns the path actually published, which differs from out.path only for
// a renumbering output that found its destination taken.
func publishProduced(deliver string, staging *tempfile.External, out Output) (string, error) {
	switch {
	case staging != nil && deliver == staging.Path():
		// Exclusive: publish the staged output onto a path nothing else holds.
		if out.renumber {
			return staging.CommitNewNumbered()
		}
		return out.path, staging.CommitNew()
	case deliver == out.path:
		return out.path, nil // the producer wrote the destination directly
	default:
		return deliverFile(deliver, out)
	}
}

// stageExclusive reserves a staging path next to an exclusive output's
// destination, for a producer that writes its output path directly (the
// pipeline, or the downloader). Publishing that staged file is then what claims
// the destination, so the producer never holds it, and the loudness converge
// loop is free to rewrite the staged path.
//
// It returns nil for a non-exclusive or non-file output; the caller keeps
// writing the destination directly, as before. Callers must defer Discard.
func stageExclusive(out Output) (*tempfile.External, error) {
	if out.kind != outputFile || !out.exclusive {
		return nil, nil
	}
	// An empty ext keeps the destination's extension on the staged name, which
	// the container-inferring encoders need.
	return tempfile.NewExternal(out.path, "")
}

// selectIndex resolves an audio selector against the candidate formats. An
// explicit selector with no match returns ErrRequestedFormatUnavailable and
// lists alternatives. A best-audio miss returns ErrNoAudioFormats.
func selectIndex(sel AudioSelector, policy SourcePolicy, target format.Target, formats []Format) (int, error) {
	if len(formats) == 0 {
		return -1, waxerr.ErrNoAudioFormats
	}
	idx, err := sel.Select(formats, policy, target)
	if err != nil {
		if errors.Is(err, format.ErrNoMatch) {
			if sel.Explicit() {
				itags, codecs := availableAudio(formats)
				rfe := &waxerr.RequestedFormatError{Selector: sel.String()}
				// Report alternatives of the same kind as the selector.
				if sel.IsCodec() {
					rfe.Codecs = codecs
				} else {
					rfe.Itags = itags
				}
				return -1, rfe
			}
			return -1, fmt.Errorf("%w: %v", waxerr.ErrNoAudioFormats, err)
		}
		return -1, err
	}
	return idx, nil
}

// availableAudio returns the distinct audio itags and codec families among the
// candidates, for naming alternatives when a requested format is unavailable.
func availableAudio(formats []Format) (itags []int, codecs []string) {
	seenItag := map[int]bool{}
	seenCodec := map[string]bool{}
	for _, f := range formats {
		if format.IsVideo(f) {
			continue // explicit video/* streams are not audio candidates
		}
		if f.Itag != 0 && !seenItag[f.Itag] {
			seenItag[f.Itag] = true
			itags = append(itags, f.Itag)
		}
		if fam := format.CodecFamily(f.Codec); fam != "" && !seenCodec[fam] {
			seenCodec[fam] = true
			codecs = append(codecs, fam)
		}
	}
	return itags, codecs
}
