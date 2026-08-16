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
	case FormatAAC:
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
	if err := validateOutputContainer(s); err != nil {
		return err
	}
	if err := validateCutEncodeNeed(s); err != nil {
		return err
	}
	if err := validateLoudness(s.Loudness); err != nil {
		return err
	}
	if err := validateBitrate(s.Transcode); err != nil {
		return err
	}
	if err := validateBitDepth(s.Transcode); err != nil {
		return err
	}
	return validateCoverArt(s)
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
	if cut && s.Cut.Mode == CutCopy && target != media.CodecCopy {
		return fmt.Errorf("%w: --cut-mode copy cannot be combined with --format %s, which re-encodes; drop one",
			waxerr.ErrIncompatibleSpec, target)
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
		return fmt.Errorf("%w: cutting without re-encoding keeps the source codec, which needs a container extension on the output (e.g. .opus/.m4a/.webm/.ogg/.mka) or pass --format", waxerr.ErrIncompatibleSpec)
	}
	return nil
}

// copyCutNeedsExtension reports whether a stream-copy cut to path lacks a usable
// container extension. It mirrors the pipeline's runtime guard (ext "" or "copy").
func copyCutNeedsExtension(path string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	return ext == "" || ext == "copy"
}

// validateLoudness checks targets used for loudness application. Measure-only
// specs do not use a target.
func validateLoudness(l *LoudnessSpec) error {
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
	case CutAccurate:
		return media.ModeAccurate
	default:
		return media.ModeSmart
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

// loudnessMissWarnDB is the miss, in LU, that turns a loudness shortfall from a
// detail into something the user needs told. Below it the miss is within the noise
// of a lossy encode.
//
// It is deliberately well above [loudness.ConvergeToleranceDB] (0.3 LU), which
// leaves a band where a limit-mode miss neither converges further nor warns: a
// 0.8 LU miss is silent. That is intended, for the reason above, but it is the
// same shape as the finding this warning exists to close, so it is written down
// here rather than left for the next end-to-end pass to rediscover.
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
// InputLoudness that fed it (downmix fold included), is deterministic and correctly
// attributed.
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
func warnLimiterTargetMissed(em *emitter, ls *LoudnessSpec, pres pipeline.Result) {
	out := pres.OutputLoudness
	if out == nil || !out.Finite() {
		return // no measurement, nothing honest to report
	}
	miss := ls.Target - out.IntegratedLUFS
	if math.Abs(miss) <= loudnessMissWarnDB {
		return
	}
	// Two wordings, because an overshoot is not something the limiter "held": a
	// shortfall is the limiter giving gain back, an overshoot is the gain search
	// stepping past the target.
	//
	// Plural is safe: reaching this line needs a miss above loudnessMissWarnDB,
	// which is far above the correction floor, so the search always ran at least one
	// correction and the count is at least 2. Do not add singular handling for a
	// case that cannot occur. The count comes from the result rather than from
	// maxLoudnessWrites, since tolerance or saturation can end the search early.
	tmpl := "the true-peak limiter held the output %.1f LU short of the %g LUFS target after %d encode passes; delivered %.1f LUFS"
	if miss < 0 {
		tmpl = "normalization landed %.1f LU above the %g LUFS target after %d encode passes; delivered %.1f LUFS"
	}
	em.warn(WarnLoudnessTargetMissed, fmt.Sprintf(tmpl,
		math.Abs(miss), ls.Target, pres.LoudnessPasses, out.IntegratedLUFS))
}

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
		LoudnessMeasured: p.LoudnessMeasured,
		LoudnessApplied:  p.LoudnessApplied,
	}
	if p.Transcoded {
		res.OutputFormat = outputFormat(p.OutputCodec, srcFmt)
	}
	if p.Cut {
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
		// Overlay authoritative rate/channels/bitrate/duration from the written file.
		applyProbe(&res.OutputFormat, *p.OutputProbe)
		if sz := p.OutputProbe.Format.Size; sz > 0 {
			res.OutputFormat.ContentLength = sz
		}
	}
	if p.LoudnessMeasured {
		res.Loudness = &LoudnessResult{
			Input:  toLoudnessInfo(p.InputLoudness),
			Output: toLoudnessInfo(p.OutputLoudness),
			Target: target,
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
		if a.BitRate > 0 {
			f.Bitrate = a.BitRate
		}
		if a.Duration > 0 {
			f.Duration = a.Duration
		}
	}
	if pr.Format.Duration > 0 {
		f.Duration = pr.Format.Duration
	}
	// A probe often leaves the audio-stream bitrate zero for VBR/lossless. Fall back
	// to the container bitrate, then a size/duration estimate, so both the
	// info --probe row and a download's OutputFormat report a usable bitrate.
	if f.Bitrate == 0 {
		switch secs := f.Duration.Seconds(); {
		case pr.Format.BitRate > 0:
			f.Bitrate = pr.Format.BitRate
		case secs > 0 && pr.Format.Size > 0:
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

// ensureParentDir creates the parent directory for an output path. The caller's
// umask controls permissions; private internal directories use stricter modes.
// For a bare filename, filepath.Dir returns "." and MkdirAll is a no-op.
func ensureParentDir(path string) error {
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

// copyFile copies src to dst atomically (temp + rename in dst's directory).
// exclusive publishes onto a free path only, failing with fs.ErrExist when
// something is already there.
func copyFile(src, dst string, exclusive bool) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tf, err := tempfile.New(dst)
	if err != nil {
		return err
	}
	defer tf.Discard()
	if _, err := io.Copy(tf, in); err != nil {
		return err
	}
	if exclusive {
		return tf.CommitNew()
	}
	return tf.Commit()
}

// moveFile renames src to dst, falling back to a copy when they live on different
// filesystems (a temp dir versus the destination).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst, false); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

// moveFileNew is moveFile for a destination that must not already exist. A src
// on the destination's filesystem publishes with a hard link; anything else
// (a job temp dir on another device) is copied into the destination directory
// first, so the final step is exclusive either way.
func moveFileNew(src, dst string) error {
	err := tempfile.PublishNew(src, dst)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return err
	}
	if cerr := copyFile(src, dst, true); cerr != nil {
		return cerr
	}
	_ = os.Remove(src)
	return nil
}

// deliverFile publishes the produced file src as out's path, honoring out's
// exclusivity. Callers that let a producer write the destination directly do not
// reach it.
func deliverFile(src string, out Output) error {
	if out.exclusive {
		return moveFileNew(src, out.path)
	}
	return moveFile(src, out.path)
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
func publishProduced(deliver string, staging *tempfile.External, out Output) error {
	switch {
	case staging != nil && deliver == staging.Path():
		// Exclusive: publish the staged output onto a path nothing else holds.
		return staging.CommitNew()
	case deliver == out.path:
		return nil // the producer wrote the destination directly
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
