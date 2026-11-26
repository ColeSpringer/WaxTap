package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
)

// download routes what Classify reads: a video downloads alone, its list=
// dropped, and a playlist or channel expands. --list ignores the video, as
// Enumerate does, and refuses only an input naming no list or channel.
func TestRouteDownload(t *testing.T) {
	const list = "PLdummyPlaylist000"
	cases := []struct {
		in       string
		listOnly bool
		want     downloadRoute
		err      error // the error's class; nil for a route
		usage    bool  // a usage error instead
	}{
		{"dummyVideo0", false, routeSingle, nil, false},
		{"https://youtu.be/dummyVideo0", false, routeSingle, nil, false},
		{"https://www.youtube.com/watch?v=dummyVideo0&list=" + list, false, routeSingle, nil, false},
		{"https://www.youtube.com/watch?v=dummyVideo0&list=" + list, true, routePlaylist, nil, false},
		{"https://www.youtube.com/playlist?list=" + list, false, routePlaylist, nil, false},
		{list, false, routePlaylist, nil, false},
		{list + "&si=abcdefghijk", false, routePlaylist, nil, false},
		{"https://www.youtube.com/@SomeHandle", false, routePlaylist, nil, false},
		{"@SomeHandle/videos", true, routePlaylist, nil, false},
		{"UCabcdefghijklmnopqrstuv", true, routePlaylist, nil, false},
		// A malformed video beside a list is an error naming the video, not a
		// run over the whole list; --list lists it.
		{"https://www.youtube.com/watch?v=bad&list=" + list, false, 0, waxtap.ErrVideoIDTooShort, false},
		{"https://www.youtube.com/watch?v=bad&list=" + list, true, routePlaylist, nil, false},
		{"https://www.youtube.com/playlist", false, 0, waxtap.ErrInvalidPlaylistID, false},
		{"https://www.youtube.com/playlist", true, 0, nil, true},
		{"https://www.youtube.com/c", false, 0, waxtap.ErrInvalidVideoID, false},
		{"https://example.com/watch?v=dummyVideo0", false, 0, waxtap.ErrInvalidVideoID, false},
		{"https://example.com/playlist?list=" + list, true, 0, nil, true},
		{"dummyVideo0", true, 0, nil, true},
	}
	for _, tc := range cases {
		link, lerr := waxtap.Classify(tc.in)
		got, err := routeDownload(link, lerr, tc.listOnly)
		switch {
		case tc.usage:
			if !isUsageError(err) {
				t.Errorf("route(%q, list=%v) err = %v, want a usage error", tc.in, tc.listOnly, err)
			}
		case tc.err != nil:
			if !errors.Is(err, tc.err) {
				t.Errorf("route(%q, list=%v) err = %v, want %v", tc.in, tc.listOnly, err, tc.err)
			}
		case err != nil || got != tc.want:
			t.Errorf("route(%q, list=%v) = %v, %v; want %v", tc.in, tc.listOnly, got, err, tc.want)
		}
	}
}

// A malformed video beside a list fails with the video's own error, exit 2,
// and the hint names the list the command used to download in its place.
func TestRouteDownload_MalformedVideoBesideListHints(t *testing.T) {
	link, lerr := waxtap.Classify("https://www.youtube.com/watch?v=bad&list=PLdummyPlaylist000")
	_, err := routeDownload(link, lerr, false)
	c := classifyError(err)
	if c.exitCode != 2 || c.code != "invalid-input" || c.message != "video ID is too short (expected 11 characters)" {
		t.Errorf("classified = %+v, want the video's invalid-input error, exit 2", c)
	}
	if !strings.Contains(c.hint, "PLdummyPlaylist000") || !strings.Contains(c.hint, "--list") {
		t.Errorf("hint = %q, want it to name the list and --list", c.hint)
	}

	link, lerr = waxtap.Classify("https://www.youtube.com/watch?v=bad")
	_, err = routeDownload(link, lerr, false)
	if c := classifyError(err); c.hint != "" {
		t.Errorf("hint = %q without a list, want none", c.hint)
	}
}
