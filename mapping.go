package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxtap/cut"
	"github.com/colespringer/waxtap/download"
	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/internal/pipeline"
	"github.com/colespringer/waxtap/internal/tempfile"
	"github.com/colespringer/waxtap/normalize"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube"
)

// transcodeCodec maps a public TranscodeFormat to a transcode.Codec.
func transcodeCodec(f TranscodeFormat) transcode.Codec {
	switch f {
	case FormatFLAC:
		return transcode.CodecFLAC
	case FormatALAC:
		return transcode.CodecALAC
	case FormatWAV:
		return transcode.CodecWAV
	case FormatMP3:
		return transcode.CodecMP3
	case FormatAAC:
		return transcode.CodecAAC
	case FormatOpus:
		return transcode.CodecOpus
	case FormatVorbis:
		return transcode.CodecVorbis
	default:
		return transcode.CodecCopy
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
	if c == transcode.CodecCopy {
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

// cutRanges maps public TimeRanges to cut.Ranges.
func cutRanges(rs []TimeRange) []cut.Range {
	if len(rs) == 0 {
		return nil
	}
	out := make([]cut.Range, len(rs))
	for i, r := range rs {
		out[i] = cut.Range{Start: r.Start, End: r.End}
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
// acquisition or ffmpeg work begins.
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
	return validateBitrate(s.Transcode)
}

// validateOutputContainer rejects a file transcode when the output extension
// names a container that cannot hold the target codec. Extensionless, codec-
// named, and copy outputs are unconstrained. Writer sinks are not checked here
// because they stage with a derived extension.
func validateOutputContainer(s ProcessSpec) error {
	if s.Transcode == nil || s.Output.kind != outputFile {
		return nil
	}
	return transcode.CheckOutputContainer(transcodeCodec(s.Transcode.Format), s.Output.path)
}

// validateCutEncodeNeed rejects copy-mode cuts that cannot be described by the
// output path alone. Accurate cuts and crossfades require encoding, so copy mode
// needs an explicit target format. A plain copy cut can keep the source samples,
// but a file output still needs a container extension.
//
// Downmix is skipped here because the pipeline needs the probed channel count.
// When the source has more channels than the target, the pipeline chooses an
// encode after probing and the cut is valid without --format. When no fold is
// needed, the pipeline still applies its copy-mode checks before writing.
func validateCutEncodeNeed(s ProcessSpec) error {
	if !cutRequested(s.Cut) || s.Downmix || transcodeCodec(specFormat(s.Transcode)) != transcode.CodecCopy {
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
	// The bounds apply only where bitrate is used. ffmpeg ignores it for lossless
	// and copy targets, so an out-of-range value there is harmless, not an error.
	if t.Bitrate > 0 && t.Bitrate < minPlausibleBitrate && !transcodeCodec(t.Format).IsLossless() {
		return fmt.Errorf("%w: transcode bitrate %d bps is implausibly low (min %d); bitrate is in bits per second, e.g. 128000 for 128 kbps", waxerr.ErrIncompatibleSpec, t.Bitrate, minPlausibleBitrate)
	}
	if t.Bitrate > maxBitrate && !transcodeCodec(t.Format).IsLossless() {
		return fmt.Errorf("%w: transcode bitrate %d bps is implausibly high (max %d)", waxerr.ErrIncompatibleSpec, t.Bitrate, maxBitrate)
	}
	return nil
}

// downmixChannels returns the requested output channel count, or 0 when downmix
// is disabled. validateProcessSpec rejects layouts without a fixed count.
func downmixChannels(layout ChannelLayout, downmix bool) int {
	if !downmix {
		return 0
	}
	return layout.ChannelCount()
}

// cutMode maps a public CutMode to a cut.Mode.
func cutMode(m CutMode) cut.Mode {
	switch m {
	case CutCopy:
		return cut.ModeCopy
	case CutAccurate:
		return cut.ModeAccurate
	default:
		return cut.ModeSmart
	}
}

// pipelineSpec builds the internal pipeline spec from a ProcessSpec and the
// resolved removal ranges (explicit ranges plus any from SponsorBlock).
func pipelineSpec(s ProcessSpec, ranges []cut.Range) pipeline.Spec {
	ps := pipeline.Spec{Remove: ranges, Downmix: downmixChannels(s.Channels, s.Downmix), Threads: s.Threads}
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
		// An explicit FormatCopy is a stream-copy remux (distinct from a nil
		// Transcode, which keeps the source bytes untouched).
		ps.Remux = s.Transcode.Format == FormatCopy
	}
	if s.Loudness != nil {
		ps.Loudness = &pipeline.Loudness{
			Apply:  s.Loudness.Mode == LoudnessApply,
			Target: s.Loudness.Target,
		}
	}
	return ps
}

// sponsorBlockContributed reports whether SponsorBlock removed additional audio
// after clamping and merging. Segments that fall outside the media duration, or
// that are already covered by explicit ranges, do not count as applied work.
func sponsorBlockContributed(explicit, sbRanges []cut.Range, pres pipeline.Result) bool {
	if !pres.Cut || len(sbRanges) == 0 || pres.SourceDuration <= 0 {
		return false
	}
	total := pres.SourceDuration
	combined := append(append([]cut.Range{}, explicit...), sbRanges...)
	explicitKept := cut.OutputDuration(cut.Keeps(explicit, total), 0)
	combinedKept := cut.OutputDuration(cut.Keeps(combined, total), 0)
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

// needsProcessing reports whether the spec needs ffmpeg and a staged input. When
// false, a download can stream straight to the sink with no temp file. Any
// non-nil Transcode counts, including an explicit FormatCopy remux (distinct from
// a nil Transcode, which keeps the source bytes). A downmix request also counts:
// the fold needs a probe to decide and an encode to apply.
func needsProcessing(s ProcessSpec) bool {
	return cutRequested(s.Cut) || s.Transcode != nil || s.Loudness != nil || s.Downmix
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

// applyProbe fills a candidate Format with authoritative values from an ffprobe
// of its resolved stream (InfoProbe depth) or written output. It overwrites only
// the measured numeric fields and duration, leaving the codec id from the player
// response, which is more specific than ffprobe's normalized name.
func applyProbe(f *Format, pr transcode.ProbeResult) {
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
	// ffprobe often leaves the audio-stream bitrate zero for VBR/lossless. Fall back
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
func outputFormat(c transcode.Codec, src Format) Format {
	if c == transcode.CodecCopy {
		return src
	}
	return Format{Codec: c.String(), Extension: c.Extension()}
}

// toLoudnessInfo maps an internal loudness measurement to the public info type,
// preserving nil.
func toLoudnessInfo(l *normalize.Loudness) *LoudnessInfo {
	if l == nil {
		return nil
	}
	v := loudnessInfo(*l)
	return &v
}

// loudnessInfo maps an internal loudness value to the public info type.
func loudnessInfo(l normalize.Loudness) LoudnessInfo {
	return LoudnessInfo{
		IntegratedLUFS: l.IntegratedLUFS,
		TruePeakDBTP:   l.TruePeakDBTP,
		LRA:            l.LRA,
		Threshold:      l.Threshold,
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
	if c == transcode.CodecCopy {
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
func copyFile(src, dst string) error {
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
	return tf.Commit()
}

// moveFile renames src to dst, falling back to a copy when they live on different
// filesystems (a temp dir versus the destination).
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
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
