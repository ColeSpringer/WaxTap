package main

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/format"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// bindSourceSelectionFlags registers the source-selection flags shared by
// download, cut, and transcode.
func bindSourceSelectionFlags(f *pflag.FlagSet, channels *string, downmix, noFallback *bool) {
	f.StringVar(channels, "channels", "stereo", "channel layout to prefer: mono|stereo|surround|any")
	f.BoolVar(downmix, "downmix", false, "fold the selected source down to --channels when it has more channels")
	f.BoolVar(noFallback, "no-fallback", false, "disable WEB-context, watch-page, and incomplete-download fallbacks")
}

// rejectChangedFlags returns a usage error for the first explicitly set flag in
// names. It is used when a flag is valid for a command but not for its selected
// mode or input shape.
func rejectChangedFlags(cmd *cobra.Command, reason string, names ...string) error {
	for _, name := range names {
		if cmd.Flags().Changed(name) {
			return usagef("--%s %s", name, reason)
		}
	}
	return nil
}

// transcodeFormatNames lists the encoded output formats --format accepts, in the
// order help text and error messages present them: the lossless set, then the
// lossy set. It is the one place the set is written down, and
// TestTranscodeFormatParity pins it to the engine's media.OutputFormats(): a
// format WaxFlow registers and WaxTap forgets to expose is what produced the
// half-wired AIFF the 2026-07-28 pass found.
var transcodeFormatNames = []string{"flac", "alac", "wav", "aiff", "wavpack", "ape", "mp3", "aac", "he-aac", "opus", "vorbis"}

// formatSpellingNote documents the aliases parseTranscodeFormat accepts beyond
// the canonical names. It sits outside formatChoices, which is pinned to exactly
// the format set, and omits remux=copy on purpose: cut and normalize share this
// text and must not mention copy at all.
const formatSpellingNote = " (case-insensitive; ogg=vorbis, m4a=aac, aif=aiff, wv=wavpack, heaac=he-aac)"

// formatChoices renders the --format choices for help text and errors. withCopy
// prepends the copy pseudo-format, which remuxes rather than encoding, so the
// commands that cannot remux (normalize, cut) leave it out.
func formatChoices(withCopy bool) string {
	if withCopy {
		return "copy|" + strings.Join(transcodeFormatNames, "|")
	}
	return strings.Join(transcodeFormatNames, "|")
}

// parseTranscodeFormat maps a user codec name to a TranscodeFormat. An empty
// string is the caller's signal for "no transcode" and is rejected here.
func parseTranscodeFormat(s string) (waxtap.TranscodeFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "copy", "remux":
		return waxtap.FormatCopy, nil
	case "flac":
		return waxtap.FormatFLAC, nil
	case "alac":
		return waxtap.FormatALAC, nil
	case "wav":
		return waxtap.FormatWAV, nil
	case "aiff", "aif", "aifc", "afc":
		// Four accepted spellings, one output extension. .aifc/.afc name the
		// compressed-capable variant so a file of that type can be named as input;
		// transcodeExt still delivers .aiff.
		return waxtap.FormatAIFF, nil
	case "mp3":
		return waxtap.FormatMP3, nil
	case "aac", "m4a":
		return waxtap.FormatAAC, nil
	case "he-aac", "heaac":
		return waxtap.FormatHEAAC, nil
	case "opus":
		return waxtap.FormatOpus, nil
	case "vorbis", "ogg":
		return waxtap.FormatVorbis, nil
	case "wavpack", "wv":
		return waxtap.FormatWavPack, nil
	case "ape":
		return waxtap.FormatAPE, nil
	default:
		return 0, usagef("unknown transcode format %q (want %s)", s, formatChoices(true))
	}
}

// transcodeExt returns the output file extension (without a dot) for a transcode
// format. FormatCopy returns "" because its container follows the source.
func transcodeExt(f waxtap.TranscodeFormat) string {
	switch f {
	case waxtap.FormatFLAC:
		return "flac"
	case waxtap.FormatALAC, waxtap.FormatAAC, waxtap.FormatHEAAC:
		return "m4a"
	case waxtap.FormatWAV:
		return "wav"
	case waxtap.FormatAIFF:
		return "aiff"
	case waxtap.FormatMP3:
		return "mp3"
	case waxtap.FormatOpus:
		return "opus"
	case waxtap.FormatVorbis:
		return "ogg"
	case waxtap.FormatWavPack:
		return "wv"
	case waxtap.FormatAPE:
		return "ape"
	default:
		return ""
	}
}

// parseCutMode maps a mode name to a CutMode.
func parseCutMode(s string) (waxtap.CutMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "smart":
		return waxtap.CutSmart, nil
	case "copy":
		return waxtap.CutCopy, nil
	case "accurate":
		return waxtap.CutAccurate, nil
	default:
		return 0, usagef("invalid --cut-mode %q (want smart|copy|accurate)", s)
	}
}

// parsePeakMode maps a mode name to a PeakMode.
func parsePeakMode(s string) (waxtap.PeakMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "cap":
		return waxtap.PeakCap, nil
	case "limit":
		return waxtap.PeakLimit, nil
	default:
		return 0, usagef("invalid --peak-mode %q (want cap|limit)", s)
	}
}

// parseCoverArt maps a mode name to a CoverArtMode.
func parseCoverArt(s string) (waxtap.CoverArtMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "frame":
		return waxtap.CoverArtFrame, nil
	case "square":
		return waxtap.CoverArtSquare, nil
	default:
		return 0, usagef("invalid --cover-art %q (want frame|square)", s)
	}
}

// parseSourcePolicy maps a policy name to a SourcePolicy. "prefer:<codec>"
// selects PreferCodec, a soft bias that ranks the named codec first but still
// falls back to other sources, unlike the hard --codec filter.
func parseSourcePolicy(s string) (waxtap.SourcePolicy, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "", s == "minimize-loss", s == "minimize":
		return waxtap.MinimizeLoss(), nil
	case s == "best-native", s == "native":
		return waxtap.BestNative(), nil
	case strings.HasPrefix(s, "prefer:"):
		codec := strings.TrimPrefix(s, "prefer:")
		if codec == "" {
			return waxtap.SourcePolicy{}, usagef("--source-policy prefer: needs a codec, e.g. prefer:opus")
		}
		// Restricting to the known families is deliberate typo protection, not a
		// statement that nothing else could ever bind: the selector matches
		// unknown ids verbatim, so an exotic exact id (say a Dolby codec) could
		// in principle be preferred. That exact-id selection stays first-class
		// through --codec; here, an unrecognized name is far more likely a typo,
		// and a silently inert policy is worse than a rejected one.
		known := format.KnownCodecFamilies()
		if !slices.Contains(known, format.CodecFamily(codec)) {
			return waxtap.SourcePolicy{}, usagef("invalid --source-policy prefer:%s (known codecs: %s)", codec, strings.Join(known, ", "))
		}
		return waxtap.PreferCodec(codec), nil
	default:
		return waxtap.SourcePolicy{}, usagef("invalid --source-policy %q (want minimize-loss|best-native|prefer:<codec>)", s)
	}
}

// parseSponsorErrorPolicy maps the SponsorBlock fetch-failure policy name.
func parseSponsorErrorPolicy(s string) (waxtap.SponsorBlockErrorPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "proceed", "proceed-uncut", "uncut":
		return waxtap.ProceedUncut, nil
	case "fail", "fail-download":
		return waxtap.FailDownload, nil
	default:
		return 0, usagef("invalid --sponsorblock-on-error %q (want proceed|fail)", s)
	}
}

// parseCutInputs validates the ranges, render mode, crossfade, and SponsorBlock
// error policy shared by the download and cut commands.
func parseCutInputs(ranges []string, cutMode, sbOnError string, crossfade time.Duration) ([]waxtap.TimeRange, waxtap.CutMode, waxtap.SponsorBlockErrorPolicy, error) {
	if crossfade < 0 {
		return nil, 0, 0, usagef("--crossfade must be non-negative")
	}
	rangeList, err := parseRanges(ranges)
	if err != nil {
		return nil, 0, 0, err
	}
	mode, err := parseCutMode(cutMode)
	if err != nil {
		return nil, 0, 0, err
	}
	pol, err := parseSponsorErrorPolicy(sbOnError)
	if err != nil {
		return nil, 0, 0, err
	}
	return rangeList, mode, pol, nil
}

// validateItag rejects an explicitly set non-positive --itag. Zero is the unset
// sentinel, and audioSelector treats only positive values as an exact selection.
func validateItag(cmd *cobra.Command, itag int) error {
	if cmd.Flags().Changed("itag") && itag <= 0 {
		return usagef("--itag must be a positive itag (run `waxtap formats <url>` to list them)")
	}
	return nil
}

// audioSelector builds an AudioSelector from --itag, --codec, and the preferred
// channel layout. An itag identifies an exact encoding and ignores layout.
func audioSelector(itag int, codec string, layout waxtap.ChannelLayout) (waxtap.AudioSelector, error) {
	codec = strings.TrimSpace(codec)
	switch {
	case itag > 0 && codec != "":
		return waxtap.AudioSelector{}, usagef("--itag and --codec are mutually exclusive")
	case itag > 0:
		return waxtap.Itag(itag), nil
	case codec != "":
		return waxtap.Codec(codec).WithChannels(layout), nil
	default:
		return waxtap.BestAudio().WithChannels(layout), nil
	}
}

// parseChannels maps a --channels value to a ChannelLayout. The empty string is
// the CLI's stereo default.
func parseChannels(s string) (waxtap.ChannelLayout, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "stereo":
		return waxtap.LayoutStereo, nil
	case "mono":
		return waxtap.LayoutMono, nil
	case "surround":
		return waxtap.LayoutSurround, nil
	case "any":
		return waxtap.LayoutAny, nil
	default:
		return 0, usagef("invalid --channels %q (want mono|stereo|surround|any)", s)
	}
}

// channelsAndDownmix parses --channels and validates --downmix against it. A fold
// needs a concrete mono or stereo target, so surround and any are rejected with
// --downmix.
func channelsAndDownmix(channels string, downmix bool) (waxtap.ChannelLayout, bool, error) {
	layout, err := parseChannels(channels)
	if err != nil {
		return 0, false, err
	}
	if downmix && layout != waxtap.LayoutMono && layout != waxtap.LayoutStereo {
		return 0, false, usagef("--downmix requires --channels mono or stereo (got %s)", layout)
	}
	return layout, downmix, nil
}

// parseRanges parses repeated "start-end" cut specs into TimeRanges. Each side
// accepts [HH:]MM:SS[.frac], a Go duration (1m30s), or bare seconds.
func parseRanges(specs []string) ([]waxtap.TimeRange, error) {
	var ranges []waxtap.TimeRange
	for _, spec := range specs {
		for _, part := range strings.Split(spec, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			startStr, endStr, ok := strings.Cut(part, "-")
			if !ok {
				return nil, usagef("invalid range %q (want start-end)", part)
			}
			// strings.Cut splits on the first "-", so a residual "-" in the end token
			// means the input carried more than one separator (e.g. 1-2-3). Report the
			// range shape rather than letting parseTimestamp reject the "2-3" fragment.
			if strings.Contains(endStr, "-") {
				return nil, usagef("invalid range %q: want one start-end (e.g. 1:00-2:30)", part)
			}
			start, err := parseTimestamp(startStr)
			if err != nil {
				return nil, err
			}
			end, err := parseTimestamp(endStr)
			if err != nil {
				return nil, err
			}
			if end <= start {
				return nil, usagef("invalid range %q: end must be after start", part)
			}
			ranges = append(ranges, waxtap.TimeRange{Start: start, End: end})
		}
	}
	return ranges, nil
}

// plainSeconds is the bare-seconds grammar: a decimal number written with
// digits and at most one point, and nothing else. ".5" and "5." stay accepted
// because they parsed before the gate existed and rejecting them would be a
// narrowing nobody asked for; what the gate exists to refuse is ParseFloat's
// exotica (exponents, underscores, hex floats, inf), which either lies about
// being a timestamp or does not survive the trip to a Duration.
var plainSeconds = regexp.MustCompile(`^(\d+(\.\d*)?|\.\d+)$`)

// parseTimestamp parses [HH:]MM:SS[.frac], a Go duration, or bare seconds. Every
// form is unsigned.
func parseTimestamp(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, usagef("empty timestamp")
	}
	// A leading sign is grammar tidying, not a fix: parseRanges cuts on the first
	// "-", so a negative timestamp cannot reach here at all, and "+1" was only
	// ever a second spelling of "1" that time.ParseDuration and ParseFloat happened
	// to take. The grammar is stated here, so it is enforced here.
	if s[0] == '+' || s[0] == '-' {
		return 0, usagef("invalid timestamp %q", s)
	}
	if strings.Contains(s, ":") {
		return parseClock(s)
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// Gating on plainSeconds instead of handing the string straight to ParseFloat
	// drops exponents ("1e3"), digit separators ("1_000") and hex floats ("0x1p3"),
	// none of which this CLI ever documented, and, importantly, "inf": a Duration
	// built from +Inf is MinInt64, so `--cut-range inf-2` used to succeed and cut
	// from 292 years before the file. "nan" was already rejected by the old f >= 0
	// test, which NaN fails.
	if !plainSeconds.MatchString(s) {
		return 0, usagef("invalid timestamp %q", s)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, usagef("invalid timestamp %q", s)
	}
	d, ok := secondsToDuration(f)
	if !ok {
		return 0, usagef("invalid timestamp %q: too large", s)
	}
	return d, nil
}

// secondsToDuration converts seconds to a Duration, reporting false when the
// value does not fit. Callers pass a non-negative, non-NaN value.
//
// Converting an out-of-range float to an integer type is undefined in Go, and in
// practice lands on MinInt64, so an unchecked value does not saturate at the top
// of the range: it flips sign, which is how "inf" produced a cut starting before
// the file. A digits-only grammar alone does not close that, since a long enough
// literal overflows the multiply just as well.
func secondsToDuration(sec float64) (time.Duration, bool) {
	ns := sec * float64(time.Second)
	// float64(math.MaxInt64) rounds up to 1<<63, which is itself out of range, so
	// the bound has to be exclusive.
	if ns >= float64(math.MaxInt64) {
		return 0, false
	}
	return time.Duration(ns), true
}

// parseClock parses a colon-separated [HH:]MM:SS[.frac] timestamp. Only the
// seconds field may be fractional, and each field after the first must be less
// than 60.
func parseClock(s string) (time.Duration, error) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, usagef("invalid timestamp %q", s)
	}
	var total float64
	last := len(parts) - 1
	for i, p := range parts {
		p = strings.TrimSpace(p)
		// Each field takes the same grammar as bare seconds. It replaces the
		// per-field sign, NaN and Inf guards this loop used to carry: those were
		// only ever needed because ParseFloat accepted those spellings first.
		// ParseFloat can still fail on a field this shape, with ErrRange.
		if !plainSeconds.MatchString(p) {
			return 0, usagef("invalid timestamp %q", s)
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, usagef("invalid timestamp %q", s)
		}
		if i != last && v != math.Trunc(v) {
			return 0, usagef("invalid timestamp %q: only the seconds field may be fractional", s)
		}
		if i != 0 && v >= 60 {
			return 0, usagef("invalid timestamp %q: minutes and seconds must be below 60", s)
		}
		total = total*60 + v
	}
	d, ok := secondsToDuration(total)
	if !ok {
		return 0, usagef("invalid timestamp %q: too large", s)
	}
	return d, nil
}
