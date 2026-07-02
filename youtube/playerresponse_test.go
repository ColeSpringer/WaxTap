package youtube

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2/format"
	"github.com/colespringer/waxtap/v2/waxerr"
)

// TestToFormat_DRCPresenceMapsAbsentToNo verifies YouTube's presence-based DRC
// flag while preserving the separate unknown state for IsOriginal.
func TestToFormat_DRCPresenceMapsAbsentToNo(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		drc  *bool
		want format.Tri
	}{
		{"absent -> No (non-DRC)", nil, format.No},
		{"explicit true -> Yes", &yes, format.Yes},
		{"explicit false -> No", &no, format.No},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := rawFormat{Itag: 251, MimeType: `audio/webm; codecs="opus"`, IsDrc: tc.drc}.toFormat()
			if f.IsDRC != tc.want {
				t.Errorf("IsDRC = %v, want %v", f.IsDRC, tc.want)
			}
		})
	}
	if f := (rawFormat{Itag: 251, MimeType: "audio/webm"}).toFormat(); f.IsOriginal != format.Unknown {
		t.Errorf("IsOriginal = %v, want Unknown for an absent default-track flag", f.IsOriginal)
	}
}

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

func TestParsePlayerResponse_SABRConfig(t *testing.T) {
	pr, err := parsePlayerResponse(readFixture(t, "player_sabr.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perr := pr.playabilityError(); perr != nil {
		t.Fatalf("playabilityError = %v, want nil", perr)
	}

	if got := pr.serverAbrURL(); got == "" {
		t.Error("serverAbrURL() = empty, want the SABR endpoint")
	}
	if got := pr.ustreamerConfig(); got != "Q0FFU0FnZ0I=" {
		t.Errorf("ustreamerConfig() = %q, want the base64 blob", got)
	}

	// SABR formats do not carry URLs or signature ciphers. Preserve the fields
	// needed to identify the selected encoding in later requests.
	raw := pr.audioFormats()
	if len(raw) != 2 {
		t.Fatalf("audio formats = %d, want 2", len(raw))
	}
	for _, rf := range raw {
		if rf.URL != "" || rf.SignatureCipher != "" {
			t.Errorf("itag %d has a direct URL or cipher: url=%q cipher=%q", rf.Itag, rf.URL, rf.SignatureCipher)
		}
	}
	opus := raw[0]
	if opus.Itag != 251 || opus.LastModified != "1700000000000001" || opus.XTags != "acont=original:lang=en" {
		t.Errorf("opus format identity = %+v", opus)
	}
	if atoi64(opus.ContentLength) != 3500000 {
		t.Errorf("opus contentLength = %q, want 3500000", opus.ContentLength)
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
		// LOGIN_REQUIRED with an age reason stays login-required; only
		// AGE_CHECK_REQUIRED maps to ErrAgeRestricted.
		{"login required", "player_login_required.json", waxerr.ErrLoginRequired},
		{"private", "player_private.json", waxerr.ErrVideoRestricted},
		{"unavailable", "player_unavailable.json", waxerr.ErrVideoUnavailable},
		{"age restricted", "player_age_restricted.json", waxerr.ErrAgeRestricted},
		// CONTENT_CHECK_REQUIRED is an interactive confirm gate, distinct from age.
		{"content check", "player_content_check.json", waxerr.ErrLoginRequired},
		{"members only", "player_members_only.json", waxerr.ErrMembersOnly},
		{"geo blocked", "player_geo_blocked.json", waxerr.ErrGeoBlocked},
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

// TestClassifyUnplayableReason checks the reason-string mapping and that a
// "remember"-style word does not false-match the members-only rule.
func TestClassifyUnplayableReason(t *testing.T) {
	cases := []struct {
		reason string
		want   error
	}{
		{"available to this channel's members", waxerr.ErrMembersOnly},
		{"not available in your country", waxerr.ErrGeoBlocked},
		{"blocked in your region", waxerr.ErrGeoBlocked},
		{"Remember to like and subscribe", waxerr.ErrVideoUnavailable}, // not members
		{"This video is unavailable", waxerr.ErrVideoUnavailable},
	}
	for _, tc := range cases {
		if got := classifyUnplayableReason(tc.reason); !errors.Is(got, tc.want) {
			t.Errorf("classifyUnplayableReason(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

// TestPlayabilityLiveStates covers the OK-status live/upcoming split and
// LIVE_STREAM_OFFLINE, which do not come from a fixture with a status.
func TestPlayabilityLiveStates(t *testing.T) {
	upcoming := &playerResponse{}
	upcoming.PlayabilityStatus.Status = "OK"
	upcoming.VideoDetails.IsUpcoming = true
	if perr := upcoming.playabilityError(); !errors.Is(perr, waxerr.ErrLiveNotStarted) {
		t.Errorf("upcoming = %v, want ErrLiveNotStarted", perr)
	}

	live := &playerResponse{}
	live.PlayabilityStatus.Status = "OK"
	live.Microformat.PlayerMicroformatRenderer.LiveBroadcastDetails.IsLiveNow = true
	if perr := live.playabilityError(); !errors.Is(perr, waxerr.ErrLiveContent) {
		t.Errorf("live = %v, want ErrLiveContent", perr)
	}

	offline := &playerResponse{}
	offline.PlayabilityStatus.Status = "LIVE_STREAM_OFFLINE"
	if perr := offline.playabilityError(); !errors.Is(perr, waxerr.ErrLiveNotStarted) {
		t.Errorf("offline = %v, want ErrLiveNotStarted", perr)
	}

	// A completed livestream VOD (isLiveContent, not live, not upcoming) is allowed.
	vod := &playerResponse{}
	vod.PlayabilityStatus.Status = "OK"
	vod.VideoDetails.IsLiveContent = true
	if perr := vod.playabilityError(); perr != nil {
		t.Errorf("completed VOD = %v, want nil (allowed)", perr)
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
