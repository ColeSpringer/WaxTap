package main

import (
	"bytes"
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
	if err := emitPlaylistList(env, pl); err != nil {
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
	if err := emitPlaylistList(env, pl); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dummyVideo0  live", "dummyVideo1  upcoming"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("want %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	env.cfg.json = true
	if err := emitPlaylistList(env, pl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"liveStatus": "live"`) || !strings.Contains(out.String(), `"liveStatus": "upcoming"`) {
		t.Errorf("want liveStatus keys in:\n%s", out.String())
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
	if err := emitPlaylistList(env, pl); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`"videoId": "dummyVideo1"`, `"index": 5`, `"notes"`} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in:\n%s", want, got)
		}
	}
}
