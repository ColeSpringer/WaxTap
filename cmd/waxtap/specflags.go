package main

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v2"
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
	case "mp3":
		return waxtap.FormatMP3, nil
	case "aac", "m4a":
		return waxtap.FormatAAC, nil
	case "opus":
		return waxtap.FormatOpus, nil
	case "vorbis", "ogg":
		return waxtap.FormatVorbis, nil
	default:
		return 0, usagef("unknown transcode format %q (want copy|flac|alac|wav|mp3|aac|opus|vorbis)", s)
	}
}

// transcodeExt returns the output file extension (without a dot) for a transcode
// format. FormatCopy returns "" because its container follows the source.
func transcodeExt(f waxtap.TranscodeFormat) string {
	switch f {
	case waxtap.FormatFLAC:
		return "flac"
	case waxtap.FormatALAC, waxtap.FormatAAC:
		return "m4a"
	case waxtap.FormatWAV:
		return "wav"
	case waxtap.FormatMP3:
		return "mp3"
	case waxtap.FormatOpus:
		return "opus"
	case waxtap.FormatVorbis:
		return "ogg"
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

// parseTimestamp parses [HH:]MM:SS[.frac], a Go duration, or bare seconds.
func parseTimestamp(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, usagef("empty timestamp")
	}
	if strings.Contains(s, ":") {
		return parseClock(s)
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second)), nil
	}
	return 0, usagef("invalid timestamp %q", s)
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
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
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
	return time.Duration(total * float64(time.Second)), nil
}
