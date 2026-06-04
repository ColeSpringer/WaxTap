package main

import (
	"testing"
	"time"

	"github.com/colespringer/waxtap"
)

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
	for _, in := range [][]string{{"nodash"}, {"5-5"}, {"10-5"}, {"a-b"}} {
		if _, err := parseRanges(in); err == nil {
			t.Errorf("parseRanges(%v) expected error", in)
		}
	}
}

func TestParseTranscodeFormat(t *testing.T) {
	cases := map[string]waxtap.TranscodeFormat{
		"copy": waxtap.FormatCopy, "flac": waxtap.FormatFLAC, "alac": waxtap.FormatALAC,
		"wav": waxtap.FormatWAV, "mp3": waxtap.FormatMP3, "aac": waxtap.FormatAAC,
		"m4a": waxtap.FormatAAC, "opus": waxtap.FormatOpus, "vorbis": waxtap.FormatVorbis,
		"ogg": waxtap.FormatVorbis, "FLAC": waxtap.FormatFLAC,
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
	}
	for f, want := range cases {
		if got := transcodeExt(f); got != want {
			t.Errorf("transcodeExt(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestAudioSelectorMutualExclusion(t *testing.T) {
	if _, err := audioSelector(140, "opus"); err == nil {
		t.Error("--itag and --codec together should error")
	}
	if _, err := audioSelector(140, ""); err != nil {
		t.Errorf("itag alone: %v", err)
	}
	if _, err := audioSelector(0, "opus"); err != nil {
		t.Errorf("codec alone: %v", err)
	}
	if _, err := audioSelector(0, ""); err != nil {
		t.Errorf("neither (best audio): %v", err)
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
