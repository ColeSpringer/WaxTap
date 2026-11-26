package youtube

import (
	"errors"
	"testing"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// linkRow pins what each extractor answers for one input. video and strict
// are ExtractVideoID's and ExtractVideoIDStrict's ID, or "!" and the class of
// their error; list is ExtractPlaylistID's ID or ""; channel is
// ExtractChannelRef's reference as "id:" or "url:" and its value, or "".
type linkRow struct {
	in, video, strict, list, channel string
}

var linkCorpus = []linkRow{
	{"dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"  dummyVideo0\n", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://www.youtube.com/watch?v=dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://www.youtube.com/watch?v=dummyVideo0&t=43s&feature=share", "dummyVideo0", "dummyVideo0", "", ""},
	{"youtube.com/watch?v=dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"www.youtube.com/watch?v=dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"HTTPS://WWW.YOUTUBE.COM/watch?v=dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://youtu.be/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"youtu.be/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://youtu.be/dummyVideo0?t=10", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://www.youtube.com/embed/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://www.youtube.com/shorts/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://www.youtube.com/v/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://www.youtube.com/live/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://music.youtube.com/watch?v=dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://m.youtube.com/watch?v=dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"https://www.youtube-nocookie.com/embed/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"dummyVideo0&feature=share", "dummyVideo0", "!invalid", "", ""},
	{"please download information now", "information", "!invalid", "", ""},
	{"information", "information", "information", "", ""},
	{"https://www.youtube.com/watch?v=dummyVideo0&list=PLdummyPlaylist000", "dummyVideo0", "dummyVideo0", "PLdummyPlaylist000", ""},
	{"https://youtu.be/dummyVideo0?list=PLdummyPlaylist000", "dummyVideo0", "dummyVideo0", "PLdummyPlaylist000", ""},
	{"https://www.youtube.com/watch?v=dummyVideo0&list=a%20b", "dummyVideo0", "dummyVideo0", "", ""},
	{"dummyVideo0&list=PLdummyPlaylist000", "dummyVideo0", "!invalid", "", ""},
	{"abc", "!short", "!short", "", ""},
	{"   ", "!short", "!short", "", ""},
	{"aaaaaaaaaa!", "!invalid", "!invalid", "", ""},
	{"https://www.youtube.com/watch?v=short", "!short", "!short", "", ""},
	{"dummyVideo012", "!long", "!long", "", ""},
	{"https://youtu.be/dummyVideo0x", "!long", "!long", "", ""},
	{"https://www.youtube.com/shorts/abc", "!short", "!short", "", ""},
	{"https://www.youtube.com/watch?v=bad&list=PLdummyPlaylist000", "!short", "!short", "PLdummyPlaylist000", ""},
	{"/tmp/x/dummyVideo0", "dummyVideo0", "!invalid", "", ""},
	{"dummyVideo0.flac", "dummyVideo0", "!invalid", "", ""},
	{"dummyVideo0 backup", "dummyVideo0", "!invalid", "", ""},
	{"D:dummyVideo0", "dummyVideo0", "!invalid", "", ""},
	{"https://www.youtube.com/", "!no-video", "!no-video", "", ""},
	{"youtube.com", "!no-video", "!no-video", "", ""},
	{"https://example.com/watch?v=dummyVideo0", "!not-youtube", "!not-youtube", "", ""},
	{"example.com/video/dummyVideo0", "!not-youtube", "!not-youtube", "", ""},
	{"vimeo.com/123456", "!not-youtube", "!not-youtube", "", ""},
	{"https://www.youtube.com/playlist?list=PLdummyPlaylist000", "!playlist", "!playlist", "PLdummyPlaylist000", ""},
	{"https://www.youtube.com/watch?list=PLdummyPlaylist000", "!playlist", "!playlist", "PLdummyPlaylist000", ""},
	{"PLdummyPlaylist000", "!playlist", "!playlist", "PLdummyPlaylist000", ""},
	{"RDdummyPlaylist000", "!playlist", "!playlist", "RDdummyPlaylist000", ""},
	{"ECdummyPlaylist000", "!playlist", "!playlist", "ECdummyPlaylist000", ""},
	{"PLdummyVide", "PLdummyVide", "PLdummyVide", "", ""},
	{"https://www.youtube.com/playlist?list=WL", "!playlist", "!playlist", "WL", ""},
	{"https://music.youtube.com/playlist?list=OLAK5uy_dummyAlbum00000000000000000000", "!playlist", "!playlist", "OLAK5uy_dummyAlbum00000000000000000000", ""},
	{"https://www.youtube.com/@SomeHandle?list=PLdummyPlaylist000", "!playlist", "!playlist", "PLdummyPlaylist000", "url:https://www.youtube.com/@SomeHandle"},
	{"https://www.youtube.com/watch?list=hello", "!playlist", "!playlist", "hello", ""},
	{"example.com/?list=PLdummyPlaylist000", "!not-youtube", "!not-youtube", "", ""},
	{"https://www.youtube.com/watch?list=+", "!bad-list", "!bad-list", "", ""},
	{"https://www.youtube.com/watch?list=a%20b", "!bad-list", "!bad-list", "", ""},
	{"https://www.youtube.com/@SomeHandle?list=+", "!channel", "!channel", "", "url:https://www.youtube.com/@SomeHandle"},
	{"https://www.youtube.com/playlist", "!bad-list", "!bad-list", "", ""},
	{"https://www.youtube.com/embed/videoseries?list=PLdummyPlaylist000", "!playlist", "!playlist", "PLdummyPlaylist000", ""},
	{"PLdummyPlaylist000&si=abcdefghijk", "!playlist", "!invalid", "PLdummyPlaylist000", ""},
	{"PLdummyPlaylist000?si=abcdefghijk", "!playlist", "!invalid", "PLdummyPlaylist000", ""},
	{"https://www.youtube.com/@SomeHandle", "!channel", "!channel", "", "url:https://www.youtube.com/@SomeHandle"},
	{"https://www.youtube.com/@SomeHandle/streams", "!channel", "!channel", "", "url:https://www.youtube.com/@SomeHandle"},
	{"@SomeHandle", "!channel", "!invalid", "", "url:https://www.youtube.com/@SomeHandle"},
	{"@elevenchars", "!channel", "!invalid", "", "url:https://www.youtube.com/@elevenchars"},
	{"https://www.youtube.com/c/SomeName", "!channel", "!channel", "", "url:https://www.youtube.com/c/SomeName"},
	{"https://www.youtube.com/user/SomeName/featured", "!channel", "!channel", "", "url:https://www.youtube.com/user/SomeName"},
	{"https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv", "!channel", "!channel", "", "id:UCabcdefghijklmnopqrstuv"},
	{"https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv/videos", "!channel", "!channel", "", "id:UCabcdefghijklmnopqrstuv"},
	{"UCabcdefghijklmnopqrstuv", "!channel", "!long", "", "id:UCabcdefghijklmnopqrstuv"},
	{"https://example.com/@SomeHandle", "!not-youtube", "!not-youtube", "", ""},
	{"https://www.youtube.com/c", "!no-video", "!no-video", "", ""},
	{"https://www.youtube.com/user", "!no-video", "!no-video", "", ""},
	{"https://www.youtube.com/channel/foo", "!no-video", "!no-video", "", ""},
	{"https://www.youtube.com/@", "!no-video", "!no-video", "", ""},
	{"https://www.youtube.com/@ab", "!channel", "!channel", "", "url:https://www.youtube.com/@ab"},
	{"https://www.youtube.com/@日本語チャンネル", "!channel", "!channel", "", "url:https://www.youtube.com/@%E6%97%A5%E6%9C%AC%E8%AA%9E%E3%83%81%E3%83%A3%E3%83%B3%E3%83%8D%E3%83%AB"},
	{"https://www.youtube.com/@%E6%97%A5%E6%9C%AC", "!channel", "!channel", "", "url:https://www.youtube.com/@%E6%97%A5%E6%9C%AC"},
	{"https://www.youtube.com/c/名前", "!channel", "!channel", "", "url:https://www.youtube.com/c/%E5%90%8D%E5%89%8D"},
	{"https://youtu.be/@SomeHandle", "!invalid", "!invalid", "", ""},
	{"@SomeHandle/videos", "!channel", "!not-youtube", "", "url:https://www.youtube.com/@SomeHandle"},
	{"@SomeHandle/videos?list=PLdummyPlaylist000", "!playlist", "!not-youtube", "PLdummyPlaylist000", "url:https://www.youtube.com/@SomeHandle"},
	{"@youtu.be/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"@example.com/watch?v=dummyVideo0", "!not-youtube", "!not-youtube", "", ""},
	{"@some.name/videos", "!not-youtube", "!not-youtube", "", ""},
	{"@www.youtube.com/watch?v=dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"UCabcdefghijklmnopqrstuv/videos", "!channel", "!not-youtube", "", "id:UCabcdefghijklmnopqrstuv"},
	{"https://www.youtube.com/embed/live_stream?channel=UCabcdefghijklmnopqrstuv", "!no-video", "!no-video", "", ""},
	{"WWW.YOUTUBE-NOCOOKIE.COM/embed/dummyVideo0", "dummyVideo0", "dummyVideo0", "", ""},
	{"youtube.com/watch?v=dummyVideo0&u=https://a", "dummyVideo0", "dummyVideo0", "", ""},
	{"youtube.com/playlist?list=PLdummyPlaylist000&u=https://a", "!playlist", "!playlist", "PLdummyPlaylist000", ""},
	{"@notes.txt", "!channel", "!short", "", "url:https://www.youtube.com/@notes.txt"},
}

// linkErrClass names an extractor's answer the way linkRow spells it.
func linkErrClass(id string, err error) string {
	switch {
	case err == nil:
		return id
	case err == errNotYouTubeURL:
		return "!not-youtube"
	case err == errNoVideoInURL:
		return "!no-video"
	case errors.Is(err, waxerr.ErrIsPlaylist):
		return "!playlist"
	case errors.Is(err, waxerr.ErrIsChannel):
		return "!channel"
	case errors.Is(err, waxerr.ErrVideoIDTooShort):
		return "!short"
	case errors.Is(err, waxerr.ErrVideoIDTooLong):
		return "!long"
	case errors.Is(err, waxerr.ErrInvalidPlaylistID):
		return "!bad-list"
	case errors.Is(err, waxerr.ErrInvalidVideoID):
		return "!invalid"
	}
	return "!unclassified: " + err.Error()
}

func channelRefString(ref ChannelRef, err error) string {
	switch {
	case err != nil:
		return ""
	case ref.ID != "":
		return "id:" + ref.ID
	default:
		return "url:" + ref.URL
	}
}

func TestLinkCorpus(t *testing.T) {
	for _, r := range linkCorpus {
		t.Run(r.in, func(t *testing.T) {
			if got := linkErrClass(ExtractVideoID(r.in)); got != r.video {
				t.Errorf("ExtractVideoID = %q, want %q", got, r.video)
			}
			if got := linkErrClass(ExtractVideoIDStrict(r.in)); got != r.strict {
				t.Errorf("ExtractVideoIDStrict = %q, want %q", got, r.strict)
			}
			list, err := ExtractPlaylistID(r.in)
			if err != nil {
				if !errors.Is(err, waxerr.ErrInvalidPlaylistID) {
					t.Errorf("ExtractPlaylistID err = %v, want ErrInvalidPlaylistID", err)
				}
				list = ""
			}
			if list != r.list {
				t.Errorf("ExtractPlaylistID = %q, want %q", list, r.list)
			}
			if got := channelRefString(ExtractChannelRef(r.in)); got != r.channel {
				t.Errorf("ExtractChannelRef = %q, want %q", got, r.channel)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	const (
		vid  = "dummyVideo0"
		list = "PLdummyPlaylist000"
	)
	cases := []struct {
		in   string
		want Link
		err  error
	}{
		{"https://www.youtube.com/watch?v=" + vid + "&list=" + list, Link{Kind: LinkVideo, VideoID: vid, PlaylistID: list}, nil},
		{"https://www.youtube.com/playlist?list=" + list, Link{Kind: LinkPlaylist, PlaylistID: list}, nil},
		{list + "&si=abcdefghijk", Link{Kind: LinkPlaylist, PlaylistID: list}, nil},
		{"https://www.youtube.com/@SomeHandle?list=" + list, Link{Kind: LinkPlaylist, PlaylistID: list, ChannelURL: "https://www.youtube.com/@SomeHandle"}, nil},
		{"@SomeHandle/videos", Link{Kind: LinkChannel, ChannelURL: "https://www.youtube.com/@SomeHandle"}, nil},
		{testChannelID, Link{Kind: LinkChannel, ChannelID: testChannelID}, nil},
		// As lenient as the commands: an ID-shaped word is a video.
		{"information", Link{Kind: LinkVideo, VideoID: "information"}, nil},
		{"please download information now", Link{Kind: LinkVideo, VideoID: "information"}, nil},
		// A malformed video is refused even beside a list, which the Link
		// still carries; Enumerate would list it.
		{"https://www.youtube.com/watch?v=bad&list=" + list, Link{PlaylistID: list}, waxerr.ErrVideoIDTooShort},
		{"https://www.youtube.com/playlist", Link{}, waxerr.ErrInvalidPlaylistID},
		{"https://example.com/watch?v=" + vid + "&list=" + list, Link{}, waxerr.ErrInvalidVideoID},
		{"https://www.youtube.com/c", Link{}, waxerr.ErrInvalidVideoID},
		{"", Link{}, waxerr.ErrVideoIDTooShort},
	}
	for _, tc := range cases {
		got, err := Classify(tc.in)
		if got != tc.want {
			t.Errorf("Classify(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if tc.err == nil && err != nil || tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("Classify(%q) err = %v, want %v", tc.in, err, tc.err)
		}
	}
}

func TestLinkKind_String(t *testing.T) {
	for k, want := range map[LinkKind]string{LinkUnknown: "unknown", LinkVideo: "video", LinkPlaylist: "playlist", LinkChannel: "channel", LinkKind(9): "unknown"} {
		if got := k.String(); got != want {
			t.Errorf("LinkKind(%d).String() = %q, want %q", k, got, want)
		}
	}
}

// checkLinkInvariants asserts that Classify and the extractors agree on in.
func checkLinkInvariants(t *testing.T, in string) {
	t.Helper()
	got, err := Classify(in)
	vid, verr := ExtractVideoID(in)
	list, lerr := ExtractPlaylistID(in)
	ref, cerr := ExtractChannelRef(in)
	isList, isChannel := errors.Is(verr, waxerr.ErrIsPlaylist), errors.Is(verr, waxerr.ErrIsChannel)

	// Classify succeeds exactly when ExtractVideoID routes the input, and
	// otherwise returns that same error.
	if (err == nil) != (verr == nil || isList || isChannel) || err != nil && err != verr {
		t.Errorf("Classify(%q) err = %v, ExtractVideoID err = %v", in, err, verr)
	}
	want := LinkUnknown
	switch {
	case err != nil:
	case verr == nil:
		want = LinkVideo
	case isList:
		want = LinkPlaylist
	case isChannel:
		want = LinkChannel
	}
	if got.Kind != want {
		t.Errorf("Classify(%q).Kind = %v, want %v", in, got.Kind, want)
	}
	if isList && lerr != nil {
		t.Errorf("%q: ErrIsPlaylist, but ExtractPlaylistID: %v", in, lerr)
	}
	if isChannel && (lerr == nil || cerr != nil) {
		t.Errorf("%q: ErrIsChannel, but ExtractPlaylistID err %v, ExtractChannelRef err %v", in, lerr, cerr)
	}
	if lerr != nil {
		list = ""
	}
	if got.VideoID != vid || got.PlaylistID != list || got.ChannelID != ref.ID || got.ChannelURL != ref.URL {
		t.Errorf("Classify(%q) = %+v; extractors say video %q, list %q, channel %+v", in, got, vid, list, ref)
	}
	// Strict mode reads a subset: any video it accepts, loose reads the same.
	if sid, serr := ExtractVideoIDStrict(in); serr == nil && (verr != nil || sid != vid) {
		t.Errorf("%q: strict reads %q, loose %q (%v)", in, sid, vid, verr)
	}
}

func TestLinkInvariants(t *testing.T) {
	for _, r := range linkCorpus {
		checkLinkInvariants(t, r.in)
	}
}

func FuzzClassify(f *testing.F) {
	for _, r := range linkCorpus {
		f.Add(r.in)
	}
	f.Fuzz(checkLinkInvariants)
}
