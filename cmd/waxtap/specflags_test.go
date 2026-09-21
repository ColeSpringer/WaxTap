package main

import (
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/spf13/cobra"
)

func TestValidateItag(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"unset itag ok", []string{"dummyVideo0"}, false},
		{"itag zero rejected", []string{"dummyVideo0", "--itag", "0"}, true},
		{"itag negative rejected", []string{"dummyVideo0", "--itag=-5"}, true},
		{"itag positive ok", []string{"dummyVideo0", "--itag", "140"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newDownloadCmd()
			if err := cmd.ParseFlags(tc.args); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.args, err)
			}
			itag, _ := cmd.Flags().GetInt("itag")
			err := validateItag(cmd, itag)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateItag(itag=%d) = %v, wantErr=%v", itag, err, tc.wantErr)
			}
			if err != nil && !isUsageError(err) {
				t.Errorf("validateItag(itag=%d) err is %T, want usageError", itag, err)
			}
		})
	}
}

func TestParseCutInputs_RejectsNegativeCrossfade(t *testing.T) {
	_, _, _, err := parseCutInputs([]string{"0-3"}, "smart", "proceed", -time.Second)
	if err == nil || !isUsageError(err) {
		t.Fatalf("parseCutInputs(crossfade=-1s) err = %v, want a usage error", err)
	}
	if _, _, _, err := parseCutInputs([]string{"0-3"}, "smart", "proceed", 500*time.Millisecond); err != nil {
		t.Errorf("parseCutInputs with a non-negative crossfade should pass: %v", err)
	}
}

func TestParseTimestamp(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"90", 90 * time.Second, true},
		{"1.5", 1500 * time.Millisecond, true},
		{"1:30", 90 * time.Second, true},
		{"1:02:03", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"0:00:05.5", 5500 * time.Millisecond, true},
		{"2m30s", 150 * time.Second, true},
		{"", 0, false},
		{"abc", 0, false},
		{"1:2:3:4", 0, false},
		// The bare-seconds form is digits with an optional fraction, not
		// everything strconv.ParseFloat happens to read.
		{"1e3", 0, false},
		{"1_000", 0, false},
		{"0x1p3", 0, false},
		// "inf" converts to MinInt64 as a Duration, so accepting it built a cut
		// range starting 292 years before the file. "nan" was already rejected;
		// the row is here so it stays that way.
		{"inf", 0, false},
		{"Inf", 0, false},
		{"nan", 0, false},
		// Timestamps are unsigned in every form, including the Go duration one.
		{"+1", 0, false},
		{"+1s", 0, false},
		// Digits that overflow the multiply into nanoseconds flip sign the same
		// way "inf" did.
		{"99999999999999999999999", 0, false},
	}
	for _, tt := range tests {
		got, err := parseTimestamp(tt.in)
		if tt.ok && (err != nil || got != tt.want) {
			t.Errorf("parseTimestamp(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
		if !tt.ok && err == nil {
			t.Errorf("parseTimestamp(%q) expected error", tt.in)
		}
	}
}

func TestParseClockStrict(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"0:01", time.Second, true},
		{"0:59", 59 * time.Second, true},
		{"1:30.5", 90500 * time.Millisecond, true},
		{"1:02:03", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"100:00:00", 100 * time.Hour, true}, // a leading field may exceed 60
		{"9:99", 0, false},                   // seconds must be < 60
		{"0:60", 0, false},                   // seconds must be < 60
		{"1:60:00", 0, false},                // minutes must be < 60
		{"1.5:00", 0, false},                 // only the seconds field may be fractional
		{"nan:00", 0, false},
		{"1:inf", 0, false},
		{"1e1:00", 0, false},                  // fields take digits, not exponents
		{"+1:00", 0, false},                   // no sign, in any field
		{"99999999999999999999:00", 0, false}, // too large to hold as a Duration
	}
	for _, tt := range tests {
		got, err := parseClock(tt.in)
		switch {
		case tt.ok && (err != nil || got != tt.want):
			t.Errorf("parseClock(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		case !tt.ok && err == nil:
			t.Errorf("parseClock(%q) expected error", tt.in)
		}
	}
}

func TestParseRanges(t *testing.T) {
	got, err := parseRanges([]string{"1:00-1:30", "90-120,2:00-2:10"})
	if err != nil {
		t.Fatal(err)
	}
	want := []waxtap.TimeRange{
		{Start: 60 * time.Second, End: 90 * time.Second},
		{Start: 90 * time.Second, End: 120 * time.Second},
		{Start: 120 * time.Second, End: 130 * time.Second},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d ranges, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("range %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseRangesRejectsBad(t *testing.T) {
	for _, in := range [][]string{{"nodash"}, {"5-5"}, {"10-5"}, {"a-b"}, {"+1-+2"}} {
		if _, err := parseRanges(in); err == nil {
			t.Errorf("parseRanges(%v) expected error", in)
		}
	}
}

// TestParseRangesRejectsInfinity pins the "inf" fix at the range level, where the
// damage showed: time.Duration(math.Inf(1)*1e9) is MinInt64, so `--cut-range
// inf-2` used to pass the end-after-start check and hand the pipeline a range
// starting at -2562047h.
func TestParseRangesRejectsInfinity(t *testing.T) {
	got, err := parseRanges([]string{"inf-2"})
	if err == nil {
		t.Fatalf("parseRanges(inf-2) = %v, want a usage error", got)
	}
	if !isUsageError(err) {
		t.Errorf("parseRanges(inf-2) err is %T, want usageError", err)
	}
}

func TestParseTranscodeFormat(t *testing.T) {
	cases := map[string]waxtap.TranscodeFormat{
		"copy": waxtap.FormatCopy, "flac": waxtap.FormatFLAC, "alac": waxtap.FormatALAC,
		"wav": waxtap.FormatWAV, "mp3": waxtap.FormatMP3, "aac": waxtap.FormatAAC,
		"m4a": waxtap.FormatAAC, "opus": waxtap.FormatOpus, "vorbis": waxtap.FormatVorbis,
		"ogg": waxtap.FormatVorbis, "FLAC": waxtap.FormatFLAC,
		// Four input spellings, one output extension (see transcodeExt).
		"aiff": waxtap.FormatAIFF, "aif": waxtap.FormatAIFF,
		"aifc": waxtap.FormatAIFF, "afc": waxtap.FormatAIFF,
		"AIFF": waxtap.FormatAIFF,
		// WaxFlow's wav row writes four spellings and its mp3 row two, for
		// the same reason: an input named with one parses, transcodeExt
		// still delivers .wav and .mp3.
		"wave": waxtap.FormatWAV, "rf64": waxtap.FormatWAV, "bw64": waxtap.FormatWAV,
		"mpga": waxtap.FormatMP3,
	}
	for in, want := range cases {
		got, err := parseTranscodeFormat(in)
		if err != nil || got != want {
			t.Errorf("parseTranscodeFormat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseTranscodeFormat("bogus"); err == nil {
		t.Error("expected error for bogus format")
	}
}

func TestTranscodeExt(t *testing.T) {
	cases := map[waxtap.TranscodeFormat]string{
		waxtap.FormatFLAC: "flac", waxtap.FormatAAC: "m4a", waxtap.FormatALAC: "m4a",
		waxtap.FormatVorbis: "ogg", waxtap.FormatOpus: "opus", waxtap.FormatCopy: "",
		waxtap.FormatWAV: "wav", waxtap.FormatMP3: "mp3", waxtap.FormatAIFF: "aiff",
	}
	for f, want := range cases {
		if got := transcodeExt(f); got != want {
			t.Errorf("transcodeExt(%v) = %q, want %q", f, got, want)
		}
	}
}

// formatParitySkip names engine output formats WaxTap deliberately does not
// expose through --format, each with the reason. It is empty: every format
// WaxFlow registers is reachable. Adding an entry is a decision, not a fix.
var formatParitySkip = map[string]string{}

// TestTranscodeFormatParity pins the CLI's format surface to the engine's.
//
// It covers the three tables reachable from this package: parseTranscodeFormat
// (the entry point aiff was missing from, while doctor advertised it),
// transcodeExt, and audioExts. TestTranscodeCodecParity in the waxtap package
// covers transcodeCodec and Codec.String; the remaining hand-maintained tables
// (isLosslessFormat, Codec.Extension/IsLossless, containerCodec,
// ContainerAccepts, ContainersFor, extPossiblyCodec) are keyed on codecs and
// extensions rather than format names and are checked in their own packages.
func TestTranscodeFormatParity(t *testing.T) {
	engine := map[string]bool{}
	for _, name := range media.OutputFormats() {
		engine[name] = true
	}
	exposed := map[string]bool{}
	for _, name := range transcodeFormatNames {
		exposed[name] = true
	}

	for name := range engine {
		if reason, skipped := formatParitySkip[name]; skipped {
			t.Logf("skipping %q: %s", name, reason)
			continue
		}
		if !exposed[name] {
			t.Errorf("the engine produces %q but --format does not offer it; wire it up or add it to formatParitySkip with a reason", name)
			continue
		}
		tf, err := parseTranscodeFormat(name)
		if err != nil {
			t.Errorf("parseTranscodeFormat(%q) = %v, want the engine format to parse", name, err)
			continue
		}
		ext := transcodeExt(tf)
		if ext == "" {
			t.Errorf("transcodeExt(%q) is empty; every encoded format needs an output extension", name)
			continue
		}
		// A format WaxTap writes must be one directory processing reads back, or
		// `transcode ./dir -f X` produces files its own next run calls ignored. This
		// is the audioExts gap the 2026-07-28 pass found for .aiff.
		if !audioExts["."+ext] {
			t.Errorf("transcodeExt(%q) = %q but audioExts has no %q; directory processing would ignore its own output", name, ext, "."+ext)
		}
	}
	for name := range exposed {
		if !engine[name] {
			t.Errorf("--format offers %q but the engine does not produce it", name)
		}
	}
	for name := range formatParitySkip {
		if !engine[name] {
			t.Errorf("formatParitySkip names %q, which the engine no longer produces; drop the entry", name)
		}
	}
}

// TestFormatChoicesRendersOneList: the four --format help strings and the parse
// error all come from transcodeFormatNames, so they cannot drift apart.
func TestFormatChoicesRendersOneList(t *testing.T) {
	if got, want := formatChoices(false), "flac|alac|wav|aiff|wavpack|ape|mp3|aac|he-aac|opus|vorbis"; got != want {
		t.Errorf("formatChoices(false) = %q, want %q", got, want)
	}
	if got, want := formatChoices(true), "copy|flac|alac|wav|aiff|wavpack|ape|mp3|aac|he-aac|opus|vorbis"; got != want {
		t.Errorf("formatChoices(true) = %q, want %q", got, want)
	}
	// copy/remux is a pseudo-format: the commands that must encode omit it.
	for _, c := range []struct {
		name     string
		cmd      *cobra.Command
		wantCopy bool
	}{
		{"download", newDownloadCmd(), true},
		{"transcode", newTranscodeCmd(), true},
		{"cut", newCutCmd(), false},
		{"normalize", newNormalizeCmd(), false},
	} {
		usage := c.cmd.Flags().Lookup("format").Usage
		if !strings.Contains(usage, formatChoices(c.wantCopy)) {
			t.Errorf("%s --format usage = %q, want the generated %q list", c.name, usage, formatChoices(c.wantCopy))
		}
		// Containment alone cannot catch a stray copy: formatChoices(false) is a
		// substring of formatChoices(true), so the no-copy commands need the
		// exclusion asserted directly.
		if hasCopy := strings.Contains(usage, "copy"); hasCopy != c.wantCopy {
			t.Errorf("%s --format usage mentions copy = %v, want %v (%s cannot remux)", c.name, hasCopy, c.wantCopy, c.name)
		}
	}
}

func TestParsePeakMode(t *testing.T) {
	cases := map[string]waxtap.PeakMode{
		"":        waxtap.PeakCap, // the flag default, and the zero value
		"cap":     waxtap.PeakCap,
		"CAP":     waxtap.PeakCap,
		"limit":   waxtap.PeakLimit,
		" Limit ": waxtap.PeakLimit,
	}
	for in, want := range cases {
		got, err := parsePeakMode(in)
		if err != nil || got != want {
			t.Errorf("parsePeakMode(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parsePeakMode("clip"); err == nil {
		t.Error("expected error for an unknown peak mode")
	}
}

func TestParseCoverArt(t *testing.T) {
	cases := map[string]waxtap.CoverArtMode{
		"":         waxtap.CoverArtFrame, // an unset flag, and the zero value
		"frame":    waxtap.CoverArtFrame,
		"FRAME":    waxtap.CoverArtFrame,
		"square":   waxtap.CoverArtSquare,
		" Square ": waxtap.CoverArtSquare,
	}
	for in, want := range cases {
		got, err := parseCoverArt(in)
		if err != nil || got != want {
			t.Errorf("parseCoverArt(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	// "none" reads as "embed no cover art", which is what --embed-thumbnail
	// controls, so it is deliberately not a value.
	for _, bad := range []string{"round", "none"} {
		if _, err := parseCoverArt(bad); err == nil {
			t.Errorf("parseCoverArt(%q) should be rejected", bad)
		}
	}
}

func TestAudioSelectorMutualExclusion(t *testing.T) {
	if _, err := audioSelector(140, "opus", waxtap.LayoutStereo); err == nil {
		t.Error("--itag and --codec together should error")
	}
	if _, err := audioSelector(140, "", waxtap.LayoutStereo); err != nil {
		t.Errorf("itag alone: %v", err)
	}
	if _, err := audioSelector(0, "opus", waxtap.LayoutStereo); err != nil {
		t.Errorf("codec alone: %v", err)
	}
	if _, err := audioSelector(0, "", waxtap.LayoutStereo); err != nil {
		t.Errorf("neither (best audio): %v", err)
	}
}

func TestParseChannels(t *testing.T) {
	cases := map[string]waxtap.ChannelLayout{
		"":         waxtap.LayoutStereo,
		"stereo":   waxtap.LayoutStereo,
		"mono":     waxtap.LayoutMono,
		"surround": waxtap.LayoutSurround,
		"any":      waxtap.LayoutAny,
		"ANY":      waxtap.LayoutAny,
	}
	for in, want := range cases {
		got, err := parseChannels(in)
		if err != nil {
			t.Errorf("parseChannels(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseChannels(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseChannels("quad"); err == nil {
		t.Error("parseChannels(quad) should error")
	}
}

func TestChannelsAndDownmix_RejectsSurroundAndAny(t *testing.T) {
	for _, c := range []string{"surround", "any"} {
		if _, _, err := channelsAndDownmix(c, true); err == nil {
			t.Errorf("--downmix with --channels %s should be a usage error", c)
		}
	}
	for _, c := range []string{"mono", "stereo"} {
		if _, on, err := channelsAndDownmix(c, true); err != nil || !on {
			t.Errorf("--downmix with --channels %s: on=%v err=%v, want on=true err=nil", c, on, err)
		}
	}
}

// TestValidateLocalSourceFlagsChecksChannelsValue pins the order inside
// validateLocalSourceFlags: an unparseable --channels is a typo report before it
// is an inert-flag report. resolveChannels is the only other caller of
// parseChannels, and it runs later and only once a downmix is in force, so a
// misspelled layout on a local input used to come back as "has no effect".
func TestValidateLocalSourceFlagsChecksChannelsValue(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"typo alone", []string{"--channels", "sterio"}, "invalid --channels"},
		{"typo with downmix", []string{"--channels", "sterio", "--downmix"}, "invalid --channels"},
		{"valid layout is still inert", []string{"--channels", "mono"}, "has no effect on a local file"},
		{"valid layout with downmix passes", []string{"--channels", "mono", "--downmix"}, ""},
		{"unset passes", nil, ""},
	}
	// Every process command shares the helper, so running the same table against
	// each one is what shows the five call sites cannot drift apart.
	commands := map[string]func() *cobra.Command{
		"cut":       newCutCmd,
		"transcode": newTranscodeCmd,
		"normalize": newNormalizeCmd,
	}
	for name, newCmd := range commands {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				cmd := newCmd()
				if err := cmd.ParseFlags(tc.args); err != nil {
					t.Fatalf("ParseFlags(%v): %v", tc.args, err)
				}
				downmix, _ := cmd.Flags().GetBool("downmix")
				err := validateLocalSourceFlags(cmd, &appConfig{}, true, downmix)
				if tc.want == "" {
					if err != nil {
						t.Fatalf("validateLocalSourceFlags(%v) = %v, want nil", tc.args, err)
					}
					return
				}
				if err == nil {
					t.Fatalf("validateLocalSourceFlags(%v) = nil, want %q", tc.args, tc.want)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("validateLocalSourceFlags(%v) = %q, want it to mention %q", tc.args, err, tc.want)
				}
				if !isUsageError(err) {
					t.Errorf("validateLocalSourceFlags(%v) err is %T, want usageError", tc.args, err)
				}
			})
		}
	}
}

func TestParseSourcePolicy(t *testing.T) {
	for _, in := range []string{"", "minimize-loss", "best-native", "prefer:opus"} {
		if _, err := parseSourcePolicy(in); err != nil {
			t.Errorf("parseSourcePolicy(%q): %v", in, err)
		}
	}
	for _, in := range []string{"prefer:", "weird"} {
		if _, err := parseSourcePolicy(in); err == nil {
			t.Errorf("parseSourcePolicy(%q) expected error", in)
		}
	}
}

func TestParseCategories(t *testing.T) {
	def, err := parseCategories("")
	if err != nil || len(def) != 1 {
		t.Fatalf("empty default: %v %v", def, err)
	}
	got, err := parseCategories("sponsor, intro ,music_offtopic")
	if err != nil || len(got) != 3 {
		t.Errorf("parsed %v, %v", got, err)
	}
	if _, err := parseCategories("notacategory"); err == nil {
		t.Error("expected error for invalid category")
	}
}

// TestIsLosslessFormatEnginePinned pins the CLI's hand-maintained lossless set
// to the engine's own classification, so a format added to one cannot silently
// classify differently in the other (the knob notes and clipping suppression
// both key on it).
func TestIsLosslessFormatEnginePinned(t *testing.T) {
	for _, name := range transcodeFormatNames {
		lossy, known := media.LossyFormat(name)
		if !known {
			t.Errorf("engine does not know %q", name)
			continue
		}
		tf, err := parseTranscodeFormat(name)
		if err != nil {
			t.Fatalf("parseTranscodeFormat(%q): %v", name, err)
		}
		if got := isLosslessFormat(tf); got != !lossy {
			t.Errorf("isLosslessFormat(%q) = %v, engine says lossy=%v", name, got, lossy)
		}
	}
	// Copy is WaxTap's own pseudo-format: a remux re-encodes nothing.
	if !isLosslessFormat(waxtap.FormatCopy) {
		t.Error("isLosslessFormat(FormatCopy) = false, want true")
	}
}

// A prefer:<codec> naming a codec no source can ever be is a typo. Accepting it
// silently means the run delivers whatever it would have delivered anyway,
// with nothing to say the preference never applied, so it is rejected at parse
// the same way --codec bogus already is.
func TestParseSourcePolicyRejectsUnknownCodec(t *testing.T) {
	for _, ok := range []string{"prefer:flac", "prefer:opus", "prefer:mp4a.40.2", "prefer:AAC"} {
		if _, err := parseSourcePolicy(ok); err != nil {
			t.Errorf("parseSourcePolicy(%q): %v", ok, err)
		}
	}
	_, err := parseSourcePolicy("prefer:bogus")
	if err == nil {
		t.Fatal("parseSourcePolicy(\"prefer:bogus\") = nil error, want a usage error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bogus") {
		t.Errorf("error does not name the bad codec: %q", msg)
	}
	for _, codec := range format.KnownCodecFamilies() {
		if !strings.Contains(msg, codec) {
			t.Errorf("error does not list %q: %q", codec, msg)
		}
	}
}

// Leading- and trailing-dot decimals parsed before the strict gate existed, so
// the gate must keep them: ".5-1.5" was in use and rejecting it is a narrowing
// nobody asked for.
func TestParseTimestampDotDecimals(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{".5", 500 * time.Millisecond},
		{"5.", 5 * time.Second},
		{"1:.5", time.Minute + 500*time.Millisecond},
	} {
		got, err := parseTimestamp(tc.in)
		if err != nil {
			t.Errorf("parseTimestamp(%q) = %v, want %v", tc.in, err, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTimestamp(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// A bare dot is not a number in any grammar.
	if _, err := parseTimestamp("."); err == nil {
		t.Error(`parseTimestamp(".") accepted a bare dot`)
	}
}

func TestParseCutMode(t *testing.T) {
	cases := []struct {
		in      string
		want    waxtap.CutMode
		wantErr bool
	}{
		{"", waxtap.CutSmart, false},
		{"smart", waxtap.CutSmart, false},
		{"copy", waxtap.CutCopy, false},
		{"copy-exact", waxtap.CutCopyExact, false},
		{"COPY-EXACT", waxtap.CutCopyExact, false},
		{"accurate", waxtap.CutAccurate, false},
		{"exact", 0, true},
	}
	for _, tc := range cases {
		got, err := parseCutMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseCutMode(%q) = %v, want a usage error", tc.in, got)
			} else if !strings.Contains(err.Error(), "copy-exact") {
				t.Errorf("parseCutMode(%q) error = %v, want the choices listed", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseCutMode(%q) = %v, %v, want %v", tc.in, got, err, tc.want)
		}
	}
}

// --codec is a hard filter, so a value that can never bind is a usage error
// rather than a run that fails with "no format matched". An exact codec id,
// which the selector matches literally, is accepted as one.
func TestAudioSelectorValidatesCodec(t *testing.T) {
	for _, ok := range []string{"aac", "AAC", "opus", "mp4a.40.2", "vp9-audio"} {
		if _, err := audioSelector(0, ok, waxtap.LayoutStereo); err != nil {
			t.Errorf("audioSelector(--codec %q) = %v, want nil", ok, err)
		}
	}
	// "flacc" is not one: codecFamily's prefix match reads it as flac, the
	// same leniency prefer:<codec> has.
	for _, bad := range []string{"bogus", "wavpack"} {
		_, err := audioSelector(0, bad, waxtap.LayoutStereo)
		if err == nil {
			t.Errorf("audioSelector(--codec %q) = nil, want a usage error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "known codecs") {
			t.Errorf("err = %v, want the known codecs listed", err)
		}
	}
	// An empty --codec is "no filter", not a bad one.
	if _, err := audioSelector(0, "", waxtap.LayoutStereo); err != nil {
		t.Errorf("empty --codec = %v, want nil", err)
	}
}
