package youtube

import (
	"errors"
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

		{"hostless id with trailing junk", id + "&feature=share", id, nil},

		{"too short", "abc", "", waxerr.ErrVideoIDTooShort},
		{"empty", "   ", "", waxerr.ErrVideoIDTooShort},
		{"bad id in watch param", "https://www.youtube.com/watch?v=short", "", waxerr.ErrInvalidVideoID},
		{"overlong bare token not truncated", id + "x", "", waxerr.ErrInvalidVideoID},
		{"overlong bare token plus more", id + "xy", "", waxerr.ErrInvalidVideoID},
		{"overlong id in youtu.be path", "https://youtu.be/" + id + "x", "", waxerr.ErrInvalidVideoID},
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
