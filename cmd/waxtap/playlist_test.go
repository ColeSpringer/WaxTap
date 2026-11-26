package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
)

// An entry whose duration is unknown, a live item or a finished stream that
// answered no length, is rendered as a dash like the other optional columns,
// not as a zero-length video. --json already omits the zero.
func TestEmitPlaylistListRendersUnknownDurationAsDash(t *testing.T) {
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
	pl := &waxtap.Playlist{Entries: []waxtap.PlaylistEntry{
		{VideoID: "dummyVideo0", Title: "Known", Duration: 90 * time.Second},
		{VideoID: "dummyVideo1", Title: "Unknown", Index: 1},
	}}
	if err := emitPlaylistList(env, pl, 0, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "dummyVideo0  1:30") {
		t.Errorf("want the known duration in its column, got:\n%s", got)
	}
	if !strings.Contains(got, "dummyVideo1  -") {
		t.Errorf("want a dash in the duration column for the unknown one, got:\n%s", got)
	}
}

// The listing's own live marker is what the duration column shows for an entry
// that has no length because it is streaming or scheduled, and --json carries
// it as its own key rather than leaving the consumer to infer it from a
// missing duration.
func TestEmitPlaylistListRendersLiveMarkers(t *testing.T) {
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
	pl := &waxtap.Playlist{Entries: []waxtap.PlaylistEntry{
		{VideoID: "dummyVideo0", Title: "Now", LiveStatus: waxtap.LiveNow},
		{VideoID: "dummyVideo1", Title: "Soon", Index: 1, LiveStatus: waxtap.LiveUpcoming},
	}}
	if err := emitPlaylistList(env, pl, 0, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dummyVideo0  live", "dummyVideo1  upcoming"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("want %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	env.cfg.json = true
	if err := emitPlaylistList(env, pl, 0, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"liveStatus": "live"`) || !strings.Contains(out.String(), `"liveStatus": "upcoming"`) {
		t.Errorf("want liveStatus keys in:\n%s", out.String())
	}
}

// The title line says what a --max-items cap cut, or that YouTube counts more
// than it listed; the document carries the count and the owner when known.
func TestEmitPlaylistListCountAndAuthor(t *testing.T) {
	entries := func(n int) []waxtap.PlaylistEntry {
		out := make([]waxtap.PlaylistEntry, n)
		for i := range out {
			out[i] = waxtap.PlaylistEntry{VideoID: fmt.Sprintf("dummyVide%02d", i), Title: "T", Index: i}
		}
		return out
	}
	cases := []struct {
		name     string
		pl       *waxtap.Playlist
		maxItems int
		header   string
	}{
		{"cut by --max-items", &waxtap.Playlist{Title: "List", VideoCount: 120, Entries: entries(5)}, 5, "List (5 of 120 items)\n"},
		{"uncut, YouTube counts more", &waxtap.Playlist{Title: "List", VideoCount: 120, Entries: entries(117)}, 0, "List (117 items; YouTube counts 120)\n"},
		{"cap above the listing", &waxtap.Playlist{Title: "List", VideoCount: 120, Entries: entries(117)}, 200, "List (117 items; YouTube counts 120)\n"},
		{"count matches", &waxtap.Playlist{Title: "List", VideoCount: 3, Entries: entries(3)}, 0, "List (3 items)\n"},
		{"listing past the stated count", &waxtap.Playlist{Title: "List", VideoCount: 2, Entries: entries(3)}, 0, "List (3 items)\n"},
		{"no count", &waxtap.Playlist{Title: "List", Entries: entries(5)}, 5, "List (5 items)\n"},
		{"one", &waxtap.Playlist{Title: "List", VideoCount: 7, Entries: entries(1)}, 1, "List (1 of 7 items)\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
			if err := emitPlaylistList(env, tc.pl, tc.maxItems, false); err != nil {
				t.Fatal(err)
			}
			if first, _, _ := strings.Cut(out.String(), "\n"); first+"\n" != tc.header {
				t.Errorf("header = %q, want %q", first+"\n", tc.header)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		pl     *waxtap.Playlist
		count  any
		author any
	}{
		{"stated", &waxtap.Playlist{ID: "PLx", VideoCount: 120, Author: "Owner Name", Entries: entries(1)}, float64(120), "Owner Name"},
		{"unstated", &waxtap.Playlist{ID: "PLx", Entries: entries(1)}, nil, nil},
	} {
		t.Run("json "+tc.name, func(t *testing.T) {
			var out bytes.Buffer
			env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}}
			if err := emitPlaylistList(env, tc.pl, 1, false); err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatalf("unmarshal %q: %v", out.String(), err)
			}
			if doc["videoCount"] != tc.count || doc["author"] != tc.author {
				t.Errorf("videoCount = %v, author = %v; want %v, %v", doc["videoCount"], doc["author"], tc.count, tc.author)
			}
			if doc["count"] != float64(1) {
				t.Errorf("count = %v, want the 1 entry listed", doc["count"])
			}
		})
	}
}

// An enrichment failure names the entry it belongs to: without the ID and
// index a --json consumer cannot tell which of a hundred entries came back
// unenriched.
func TestEmitPlaylistListErrorsNameTheEntry(t *testing.T) {
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{json: true}, notes: &noteCollector{}}
	env.note(noteEnumerationError, "one entry failed")
	pl := &waxtap.Playlist{
		Entries: []waxtap.PlaylistEntry{{VideoID: "dummyVideo0", Title: "One"}},
		Errors:  []error{&waxtap.EnrichError{VideoID: "dummyVideo1", Index: 4, Err: waxtap.ErrVideoUnavailable}},
	}
	if err := emitPlaylistList(env, pl, 0, false); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`"videoId": "dummyVideo1"`, `"index": 5`, `"notes"`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}

// A listing with entries left carries the token that resumes it, as --json's
// continuation alone: a human listing has no use for the blob. One that
// reached the end carries none.
func TestEmitPlaylistListContinuation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  any
	}{
		{"entries left", "RESUME_POINT", "RESUME_POINT"},
		{"the end", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pl := &waxtap.Playlist{ID: "PLx", Title: "List", Continuation: tc.token,
				Entries: []waxtap.PlaylistEntry{{VideoID: "dummyVideo0", Title: "T"}}}
			var out, errOut bytes.Buffer
			env := &appEnv{out: &out, errOut: &errOut, cfg: &appConfig{json: true}}
			if err := emitPlaylistList(env, pl, 1, false); err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatalf("unmarshal %q: %v", out.String(), err)
			}
			if doc["continuation"] != tc.want {
				t.Errorf("continuation = %v, want %v", doc["continuation"], tc.want)
			}

			out.Reset()
			env.cfg.json = false
			if err := emitPlaylistList(env, pl, 1, false); err != nil {
				t.Fatal(err)
			}
			if tc.token != "" && strings.Contains(out.String()+errOut.String(), tc.token) {
				t.Errorf("the human listing prints the token:\n%s%s", out.String(), errOut.String())
			}
		})
	}
}

// A resumed listing is a stretch of the playlist, so its title line counts it
// against YouTube's total, capped or not, where a listing from the top that
// came up short reports the difference.
func TestEmitPlaylistListResumedCount(t *testing.T) {
	pl := &waxtap.Playlist{Title: "List", VideoCount: 120}
	for i := 100; i < 120; i++ {
		pl.Entries = append(pl.Entries, waxtap.PlaylistEntry{VideoID: fmt.Sprintf("dummyVid%03d", i), Title: "T", Index: i})
	}
	var out bytes.Buffer
	env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
	if err := emitPlaylistList(env, pl, 0, true); err != nil {
		t.Fatal(err)
	}
	if first, _, _ := strings.Cut(out.String(), "\n"); first != "List (20 of 120 items)" {
		t.Errorf("header = %q, want %q", first, "List (20 of 120 items)")
	}
	if !strings.Contains(out.String(), "101  dummyVid100") {
		t.Errorf("want the first row numbered 101, its playlist position, in:\n%s", out.String())
	}
}
