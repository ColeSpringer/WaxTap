package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/sponsorblock"
)

// previewPlayerJSON is a minimal /player response whose only interesting field
// is the length: the preview extracts for that and nothing else.
const previewPlayerJSON = `{
  "playabilityStatus": {"status": "OK"},
  "streamingData": {
    "expiresInSeconds": "21540",
    "adaptiveFormats": [
      {"itag": 251, "url": "https://r1.googlevideo.com/videoplayback?expire=9999999999",
       "mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 130000, "contentLength": "27",
       "audioQuality": "AUDIO_QUALITY_MEDIUM", "lastModified": "1700000000000001"}
    ]
  },
  "videoDetails": {"videoId": "dummyVideo0", "title": "Preview Fixture", "lengthSeconds": "19", "author": "T"}
}`

type previewRoundTrip func(*http.Request) (*http.Response, error)

func (f previewRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// previewEnv builds an appEnv whose SponsorBlock server answers with one
// segment and whose /player answers with previewPlayerJSON (or an error, when
// playerFails is set).
func previewEnv(t *testing.T, segStart, segEnd float64, playerFails bool) (*appEnv, *bytes.Buffer) {
	t.Helper()
	segments := fmt.Sprintf(`[{"videoID":"dummyVideo0","segments":[{"category":"music_offtopic","actionType":"skip","segment":[%g,%g],"UUID":"u1","locked":1,"votes":3}]}]`, segStart, segEnd)
	ok := func(body string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}
	// One transport serves both legs: the facade hands its HTTPClient to the
	// SponsorBlock client as well as to extraction.
	rt := previewRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if playerFails {
				return nil, fmt.Errorf("player unreachable")
			}
			return ok(previewPlayerJSON)
		case strings.Contains(r.URL.Path, "/api/skipSegments/"):
			return ok(segments)
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})

	client, err := waxtap.New(waxtap.Options{
		HTTPClient:   &http.Client{Transport: rt},
		SponsorBlock: waxtap.SponsorBlockOptions{BaseURL: "https://sb.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return &appEnv{client: client, cfg: &appConfig{json: true}, out: &out, errOut: io.Discard, notes: &noteCollector{}}, &out
}

// The preview checks segments against the video's own length: one past the end
// is marked and removes nothing, one that overruns removes only the part
// inside, and a length it could not fetch is reported as a note.
func TestSponsorBlockPreviewChecksTheVideoLength(t *testing.T) {
	ctx := context.Background()
	cats := []sponsorblock.Category{sponsorblock.CategoryMusicOffTopic}

	t.Run("past the end", func(t *testing.T) {
		env, out := previewEnv(t, 25, 30, false)
		if err := runSponsorBlockPreview(ctx, env, "dummyVideo0", cats); err != nil {
			t.Fatal(err)
		}
		doc := decodePreview(t, out)
		if got := doc["removedSeconds"]; got != float64(0) {
			t.Errorf("removedSeconds = %v, want 0: the segment is past the end", got)
		}
		if got := doc["durationSeconds"]; got != float64(19) {
			t.Errorf("durationSeconds = %v, want 19", got)
		}
		segs, _ := doc["segments"].([]any)
		if len(segs) != 1 {
			t.Fatalf("segments = %v, want one", doc["segments"])
		}
		if s, _ := segs[0].(map[string]any); s["pastEnd"] != true {
			t.Errorf("segments[0] = %v, want pastEnd true", segs[0])
		}
	})

	t.Run("overruns the end", func(t *testing.T) {
		env, out := previewEnv(t, 17, 25, false)
		if err := runSponsorBlockPreview(ctx, env, "dummyVideo0", cats); err != nil {
			t.Fatal(err)
		}
		doc := decodePreview(t, out)
		if got := doc["removedSeconds"]; got != float64(2) {
			t.Errorf("removedSeconds = %v, want 2 (17s to the 19s end)", got)
		}
		segs, _ := doc["segments"].([]any)
		if s, _ := segs[0].(map[string]any); s["pastEnd"] != nil {
			t.Errorf("segments[0] = %v, want pastEnd omitted: it starts inside", segs[0])
		}
	})

	t.Run("length unknown", func(t *testing.T) {
		env, out := previewEnv(t, 25, 30, true)
		if err := runSponsorBlockPreview(ctx, env, "dummyVideo0", cats); err != nil {
			t.Fatal(err)
		}
		doc := decodePreview(t, out)
		if got := doc["removedSeconds"]; got != float64(5) {
			t.Errorf("removedSeconds = %v, want the raw 5: nothing checked it", got)
		}
		if _, ok := doc["durationSeconds"]; ok {
			t.Errorf("durationSeconds = %v, want the key omitted", doc["durationSeconds"])
		}
		notes, _ := doc["notes"].([]any)
		if len(notes) != 1 {
			t.Fatalf("notes = %v, want the length-unchecked note", doc["notes"])
		}
		if n, _ := notes[0].(map[string]any); n["code"] != string(noteLengthUnchecked) {
			t.Errorf("notes[0] = %v, want %s", notes[0], noteLengthUnchecked)
		}
	})
}

func decodePreview(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	return doc
}
