package youtube

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/waxerr"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParsePlayerResponse_OK(t *testing.T) {
	pr, err := parsePlayerResponse(readFixture(t, "player_ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perr := pr.playabilityError(); perr != nil {
		t.Fatalf("playabilityError = %v, want nil", perr)
	}

	v, raw, err := pr.toVideo("testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Errorf("raw audio formats = %d, want 2 (url/cipher retained)", len(raw))
	}
	if v.Title != "Test Song" {
		t.Errorf("title = %q", v.Title)
	}
	if v.Author != "Test Channel" {
		t.Errorf("author = %q", v.Author)
	}
	if v.Duration != 212*time.Second {
		t.Errorf("duration = %v, want 3m32s", v.Duration)
	}
	if v.PublishDate.Year() != 2009 {
		t.Errorf("publishDate = %v, want 2009", v.PublishDate)
	}
	if len(v.Thumbnails) != 1 {
		t.Errorf("thumbnails = %d, want 1", len(v.Thumbnails))
	}

	// Only the two audio formats survive; the video format is filtered out.
	if len(v.Formats) != 2 {
		t.Fatalf("formats = %d, want 2 (audio only)", len(v.Formats))
	}
	f0 := v.Formats[0]
	if f0.Itag != 251 || f0.Codec != "opus" || f0.Extension != "webm" {
		t.Errorf("format0 = %+v, want itag 251 opus webm", f0)
	}
	if f0.SampleRate != 48000 || f0.Channels != 2 || f0.AverageBitrate != 130000 {
		t.Errorf("format0 audio attrs = %+v", f0)
	}
	if f0.AudioQuality != format.QualityMedium {
		t.Errorf("format0 audioQuality = %v, want medium", f0.AudioQuality)
	}
	f1 := v.Formats[1]
	if f1.Itag != 140 || f1.Codec != "mp4a.40.2" || f1.Extension != "m4a" {
		t.Errorf("format1 = %+v, want itag 140 mp4a m4a", f1)
	}
	if f1.ContentLength != 3400000 {
		t.Errorf("format1 contentLength = %d", f1.ContentLength)
	}
	if f1.AudioQuality != format.QualityMedium {
		t.Errorf("format1 audioQuality = %v, want medium", f1.AudioQuality)
	}
}

func TestParseAudioQualityTier(t *testing.T) {
	cases := map[string]format.AudioQualityTier{
		"AUDIO_QUALITY_HIGH":     format.QualityHigh,
		"AUDIO_QUALITY_MEDIUM":   format.QualityMedium,
		"AUDIO_QUALITY_LOW":      format.QualityLow,
		"AUDIO_QUALITY_ULTRALOW": format.QualityUltraLow,
		"":                       format.QualityUnknown,
		"AUDIO_QUALITY_FUTURE":   format.QualityUnknown,
	}
	for in, want := range cases {
		if got := parseAudioQualityTier(in); got != want {
			t.Errorf("parseAudioQualityTier(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestPlayabilityClassification(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    error
	}{
		{"login required", "player_login_required.json", waxerr.ErrLoginRequired},
		{"private", "player_private.json", waxerr.ErrVideoRestricted},
		{"unavailable", "player_unavailable.json", waxerr.ErrVideoUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr, err := parsePlayerResponse(readFixture(t, tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			if perr := pr.playabilityError(); !errors.Is(perr, tc.want) {
				t.Fatalf("playabilityError = %v, want %v", perr, tc.want)
			}
		})
	}
}

func TestParseWatchPageFallback(t *testing.T) {
	pr, err := parseWatchPage(readFixture(t, "watch_page.html"))
	if err != nil {
		t.Fatalf("parseWatchPage: %v", err)
	}
	v, _, err := pr.toVideo("testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	if v.Title != "From Watch Page" {
		t.Errorf("title = %q", v.Title)
	}
	// The decoy ytInitialData (with a brace inside a string) must not confuse the
	// brace matcher, and the escaped quotes in the codec string must survive.
	if len(v.Formats) != 1 || v.Formats[0].Codec != "opus" {
		t.Errorf("formats = %+v, want one opus format", v.Formats)
	}
}
