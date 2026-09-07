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
