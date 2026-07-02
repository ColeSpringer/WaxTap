package youtube

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/waxerr"
)

func TestExtractVideoID(t *testing.T) {
	const id = "testVideo01"
	tests := []struct {
		name  string
		input string
		want  string
		err   error
	}{
		{"bare id", id, id, nil},
		{"watch url", "https://www.youtube.com/watch?v=" + id, id, nil},
		{"watch url extra params", "https://www.youtube.com/watch?v=" + id + "&t=43s&feature=share", id, nil},
		{"watch url no scheme", "youtube.com/watch?v=" + id, id, nil},
		{"youtu.be", "https://youtu.be/" + id, id, nil},
		{"youtu.be no scheme", "youtu.be/" + id, id, nil},
		{"youtu.be with query", "https://youtu.be/" + id + "?t=10", id, nil},
		{"embed", "https://www.youtube.com/embed/" + id, id, nil},
		{"shorts", "https://www.youtube.com/shorts/" + id, id, nil},
		{"v path", "https://www.youtube.com/v/" + id, id, nil},
		{"live path", "https://www.youtube.com/live/" + id, id, nil},
		{"music host", "https://music.youtube.com/watch?v=" + id, id, nil},
		{"m host", "https://m.youtube.com/watch?v=" + id, id, nil},
		{"nocookie embed", "https://www.youtube-nocookie.com/embed/" + id, id, nil},
		{"watch in a playlist still downloads the video", "https://www.youtube.com/watch?v=" + id + "&list=PLabcdefghijabcdef", id, nil},

		{"playlist only url", "https://www.youtube.com/playlist?list=PLabcdefghijabcdef", "", waxerr.ErrIsPlaylist},
		{"list param no video", "https://www.youtube.com/watch?list=PLabcdefghijabcdef", "", waxerr.ErrIsPlaylist},

		// Bare playlist IDs are not malformed video IDs.
		{"bare playlist id", "PLabcdefghijabcdef", "", waxerr.ErrIsPlaylist},
		{"bare radio playlist id", "RDabcdefghijabcdef", "", waxerr.ErrIsPlaylist},

		// Recognized YouTube URLs can still lack a video reference.
		{"youtube homepage", "https://www.youtube.com/", "", waxerr.ErrInvalidVideoID},
		{"youtube host bare", "youtube.com", "", waxerr.ErrInvalidVideoID},

		// Channel URLs are neither a video nor a playlist.
		{"handle channel", "https://www.youtube.com/@Blender", "", waxerr.ErrIsChannel},
		{"c channel", "https://www.youtube.com/c/Blender", "", waxerr.ErrIsChannel},
		{"channel id", "https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv", "", waxerr.ErrIsChannel},
		{"user channel", "https://www.youtube.com/user/Blender", "", waxerr.ErrIsChannel},

		// Bare channel IDs and @handles are channels too, mirroring the URL branch so
		// info/formats can guide the caller instead of rejecting an overlong ID.
		{"bare channel id", "UCabcdefghijklmnopqrstuv", "", waxerr.ErrIsChannel},
		{"bare handle", "@Blender", "", waxerr.ErrIsChannel},

		{"hostless id with trailing junk", id + "&feature=share", id, nil},

		{"too short", "abc", "", waxerr.ErrVideoIDTooShort},
		{"empty", "   ", "", waxerr.ErrVideoIDTooShort},

		// Eleven-character malformed tokens are invalid rather than too short.
		{"eleven chars with stray symbol", "aaaaaaaaaa!", "", waxerr.ErrInvalidVideoID},
		{"eleven symbol-heavy chars", "abc!@#=+;:_", "", waxerr.ErrInvalidVideoID},
		{"bad id in watch param", "https://www.youtube.com/watch?v=short", "", waxerr.ErrVideoIDTooShort},
		// An all-ID-character token of the wrong length is a length problem, not a
		// truncation or an invalid-character one.
		{"overlong bare token not truncated", id + "x", "", waxerr.ErrVideoIDTooLong},
		{"overlong bare token plus more", id + "xy", "", waxerr.ErrVideoIDTooLong},
		// The URL branch validates the path segment exactly and now classifies its
		// shape, so an all-ID-character overlong segment is too-long and a short one
		// is too-short, mirroring the bare-ID path.
		{"overlong id in youtu.be path", "https://youtu.be/" + id + "x", "", waxerr.ErrVideoIDTooLong},
		{"short id in youtu.be path", "https://youtu.be/short", "", waxerr.ErrVideoIDTooShort},
		{"short id in shorts path", "https://www.youtube.com/shorts/abc", "", waxerr.ErrVideoIDTooShort},

		// A non-YouTube host is not a video reference, even with a valid-looking ID.
		{"non-youtube host", "https://example.com/watch?v=" + id, "", waxerr.ErrInvalidVideoID},
		{"non-youtube host no scheme", "example.com/video/" + id, "", waxerr.ErrInvalidVideoID},
		{"non-youtube host bare path", "vimeo.com/123456", "", waxerr.ErrInvalidVideoID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractVideoID(tc.input)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("id = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractVideoIDStrict(t *testing.T) {
	const id = "testVideo01"
	// Strict extraction accepts a clean ID and recognized URLs but rejects strings
	// that only contain an 11-character run, so a mistyped local path does not
	// resolve to the embedded ID.
	reject := []string{
		id + ".flac",          // <id>.<ext>, the output-template shape
		id + " backup",        // space-delimited name
		"/tmp/x/" + id,        // path separator
		"D:" + id,             // Windows drive prefix
		id + "&feature=share", // query-style junk
	}
	for _, in := range reject {
		t.Run("reject "+in, func(t *testing.T) {
			if got, err := ExtractVideoIDStrict(in); err == nil {
				t.Fatalf("ExtractVideoIDStrict(%q) = %q, want an error", in, got)
			}
		})
	}

	accept := map[string]string{
		id:                                      id,
		"https://www.youtube.com/watch?v=" + id: id,
		"https://youtu.be/" + id:                id,
	}
	for in, want := range accept {
		t.Run("accept "+in, func(t *testing.T) {
			got, err := ExtractVideoIDStrict(in)
			if err != nil {
				t.Fatalf("ExtractVideoIDStrict(%q): %v", in, err)
			}
			if got != want {
				t.Fatalf("id = %q, want %q", got, want)
			}
		})
	}

	// The loose extractor still accepts trailing junk, which strict rejects above.
	if got, err := ExtractVideoID(id + "&feature=share"); err != nil || got != id {
		t.Fatalf("ExtractVideoID(loose) = %q, %v; want %q", got, err, id)
	}
}

func TestExtractVideoID_TooLong(t *testing.T) {
	// A 12-character all-valid-character token is too long, not invalid-character.
	_, err := ExtractVideoID("testVideo012")
	if !errors.Is(err, waxerr.ErrVideoIDTooLong) {
		t.Fatalf("err = %v, want ErrVideoIDTooLong", err)
	}
	if !strings.Contains(err.Error(), "11 characters") {
		t.Errorf("message = %q, want a length message", err)
	}
}

func TestExtractVideoID_NonYouTubeHostMessage(t *testing.T) {
	_, err := ExtractVideoID("https://example.com/watch?v=testVideo01")
	if !errors.Is(err, waxerr.ErrInvalidVideoID) {
		t.Fatalf("err = %v, want ErrInvalidVideoID", err)
	}
	if !strings.Contains(err.Error(), "not a recognized YouTube URL") {
		t.Errorf("message = %q, want it to mention an unrecognized YouTube URL", err)
	}
}

func TestExtractVideoID_NoVideoInURLMessage(t *testing.T) {
	_, err := ExtractVideoID("https://www.youtube.com/")
	if !errors.Is(err, waxerr.ErrInvalidVideoID) {
		t.Fatalf("err = %v, want ErrInvalidVideoID", err)
	}
	if !strings.Contains(err.Error(), "not a recognized YouTube video URL or video ID") {
		t.Errorf("message = %q, want the no-video-in-URL message", err)
	}
}

func TestExtractVideoID_ChannelMessage(t *testing.T) {
	_, err := ExtractVideoID("https://www.youtube.com/@Blender")
	if !errors.Is(err, waxerr.ErrIsChannel) {
		t.Fatalf("err = %v, want ErrIsChannel", err)
	}
	// A channel URL must not be misclassified as a malformed video ID.
	if errors.Is(err, waxerr.ErrInvalidVideoID) {
		t.Errorf("channel URL should not unwrap to ErrInvalidVideoID: %v", err)
	}
}

// TestExtractVideoID_BareChannel confirms the loose (target) path classifies a
// bare UC id or @handle as a channel, including an @handle whose body is an
// 11-character run (which must not be mis-extracted as a video ID). The strict
// (process-source) path deliberately does not classify a bare channel, so a
// mistyped @-prefixed or UC-shaped local path reports a missing file instead.
func TestExtractVideoID_BareChannel(t *testing.T) {
	// "@elevenchars" has an exactly-11-character body; the loose extractor must still
	// report a channel rather than returning "elevenchars" as a video ID.
	for _, in := range []string{"UCabcdefghijklmnopqrstuv", "@Blender", "@elevenchars"} {
		if _, err := ExtractVideoID(in); !errors.Is(err, waxerr.ErrIsChannel) {
			t.Errorf("ExtractVideoID(%q) err = %v, want ErrIsChannel", in, err)
		}
	}
	// Strict skips channel classification, so these fall to the shape-appropriate
	// video-ID length error, which the process commands map to "no such file".
	if _, err := ExtractVideoIDStrict("@Blender"); !errors.Is(err, waxerr.ErrVideoIDTooShort) {
		t.Errorf("ExtractVideoIDStrict(@Blender) err = %v, want ErrVideoIDTooShort", err)
	}
	if _, err := ExtractVideoIDStrict("UCabcdefghijklmnopqrstuv"); !errors.Is(err, waxerr.ErrVideoIDTooLong) {
		t.Errorf("ExtractVideoIDStrict(UC...) err = %v, want ErrVideoIDTooLong", err)
	}
}

func TestExtractPlaylistID(t *testing.T) {
	const list = "PLabcdefghijabcdef"
	tests := []struct {
		name  string
		input string
		want  string
		err   error
	}{
		{"bare", list, list, nil},
		{"playlist url", "https://www.youtube.com/playlist?list=" + list, list, nil},
		{"watch with list", "https://www.youtube.com/watch?v=testVideo01&list=" + list, list, nil},
		{"radio mix", "https://www.youtube.com/watch?v=testVideo01&list=RDabcdefghij", "RDabcdefghij", nil},
		{"education course", "ECabcdefghijklmnop", "ECabcdefghijklmnop", nil},
		{"popular uploads", "PUabcdefghijklmnop", "PUabcdefghijklmnop", nil},
		{"not a playlist", "https://www.youtube.com/watch?v=testVideo01", "", waxerr.ErrInvalidPlaylistID},
		// Exactly 11 chars: a video-id-shaped token that happens to start with a
		// playlist prefix pair must not validate as a playlist.
		{"video-id-shaped token", "PLabcdefghi", "", waxerr.ErrInvalidPlaylistID},
		{"garbage", "hello", "", waxerr.ErrInvalidPlaylistID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExtractPlaylistID(tc.input)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("id = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsShortsPlaylistID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"shorts shelf", "UUSHabcdefghijklmnopqrstuv", true},
		// Replacing UC with UU for a UCSH... channel also produces a UUSH prefix,
		// but the resulting uploads playlist ID is two characters shorter.
		{"uploads of a UCSH channel", "UUSHabcdefghijklmnopqrst", false},
		{"regular uploads", "UUabcdefghijklmnopqrstuv", false},
		{"PL playlist", "PLabcdefghijklmnopqrst", false},
		{"too long", "UUSHabcdefghijklmnopqrstuvw", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isShortsPlaylistID(tc.id); got != tc.want {
				t.Errorf("isShortsPlaylistID(%q) = %v, want %v (len %d)", tc.id, got, tc.want, len(tc.id))
			}
		})
	}
}
