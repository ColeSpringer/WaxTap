package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const validPlayerContextJSON = `{
  "status": "OK",
  "player_url": "https://www.youtube.com/s/player/444511ca/player_es6.vflset/en_US/base.js",
  "server_abr_streaming_url": "https://rr3.googlevideo.com/videoplayback?n=SCRAMBLED&sabr=1",
  "video_playback_ustreamer_config": "dXN0cmVhbWVy",
  "visitor_data": "CgtWSVNJVE9S",
  "client_version": "2.20260606.02.00",
  "title": "Big Buck Bunny",
  "author": "Blender",
  "length_seconds": 634,
  "audio_formats": [
    {"itag":251,"lmt":"1719185012384481","xtags":"","mime_type":"audio/webm; codecs=\"opus\"","bitrate":143452,"audio_channels":2,"audio_sample_rate":48000,"content_length":9700000,"approx_duration_ms":634624}
  ]
}`

func newTestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/player-context" {
			t.Errorf("path = %q, want /player-context", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestPlayerContextProviderDecode(t *testing.T) {
	srv := newTestServer(t, http.StatusOK, validPlayerContextJSON)
	defer srv.Close()

	pc, err := newPlayerContextProvider(srv.URL+"/player-context", "").ProvidePlayerContext(context.Background(), "aqz-KE-bpKQ")
	if err != nil {
		t.Fatalf("ProvidePlayerContext: %v", err)
	}
	if pc.ServerAbrURL == "" || pc.VisitorData != "CgtWSVNJVE9S" || pc.ClientVersion != "2.20260606.02.00" {
		t.Errorf("decoded context = %+v", pc)
	}
	if pc.PlayerURL != "https://www.youtube.com/s/player/444511ca/player_es6.vflset/en_US/base.js" {
		t.Errorf("player_url = %q", pc.PlayerURL)
	}
	if pc.Title != "Big Buck Bunny" || pc.LengthSeconds != 634 {
		t.Errorf("metadata = title %q length %d", pc.Title, pc.LengthSeconds)
	}
	if len(pc.AudioFormats) != 1 {
		t.Fatalf("audio formats = %d, want 1", len(pc.AudioFormats))
	}
	f := pc.AudioFormats[0]
	if f.Itag != 251 || f.LMT != "1719185012384481" || f.ContentLength != 9700000 || f.AudioSampleRate != 48000 {
		t.Errorf("format = %+v", f)
	}
}

func TestPlayerContextProviderErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"non-200", http.StatusInternalServerError, "boom", "returned"},
		{"status not OK", http.StatusOK, `{"status":"ERROR: bot check"}`, "status"},
		{"missing url", http.StatusOK, `{"status":"OK","visitor_data":"v","audio_formats":[{"itag":251}]}`, "missing"},
		{"missing visitor", http.StatusOK, `{"status":"OK","server_abr_streaming_url":"u","audio_formats":[{"itag":251}]}`, "missing"},
		{"no formats", http.StatusOK, `{"status":"OK","server_abr_streaming_url":"u","visitor_data":"v"}`, "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, tc.status, tc.body)
			defer srv.Close()
			_, err := newPlayerContextProvider(srv.URL+"/player-context", "").ProvidePlayerContext(context.Background(), "v")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
