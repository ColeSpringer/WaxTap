package tests

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
)

// fullMetaPlayerJSON is an ANDROID_VR-style /player response: no microformat, so
// no publish date and no availability, and chapters live only on the watch page.
const fullMetaPlayerJSON = `{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"dummyVideo0","title":"T","author":"A","channelId":"UCabcdefghijklmnopqrstuv","lengthSeconds":"100"},"streamingData":{"adaptiveFormats":[{"itag":251,"mimeType":"audio/webm; codecs=\"opus\"","bitrate":130000,"contentLength":"1000","audioSampleRate":"48000","audioChannels":2,"url":"https://r.googlevideo.com/videoplayback?itag=251"}]}}`

// fullMetaWatchHTML carries chapters (ytInitialData) and the availability
// microformat behind ytInitialPlayerResponse.
const fullMetaWatchHTML = `<html><head>` +
	`<script>var ytInitialData = {"playerOverlays":{"playerOverlayRenderer":{"decoratedPlayerBarRenderer":{"decoratedPlayerBarRenderer":{"playerBar":{"multiMarkersPlayerBarRenderer":{"markersMap":[{"key":"DESCRIPTION_CHAPTERS","value":{"chapters":[{"chapterRenderer":{"title":{"simpleText":"Intro"},"timeRangeStartMillis":0}},{"chapterRenderer":{"title":{"simpleText":"Body"},"timeRangeStartMillis":30000}}]}}]}}}}}}};</script>` +
	`<script>var ytInitialPlayerResponse = {"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"dummyVideo0","title":"T","author":"A","channelId":"UCabcdefghijklmnopqrstuv","lengthSeconds":"100"},"streamingData":{"adaptiveFormats":[{"itag":251,"mimeType":"audio/webm; codecs=\"opus\"","bitrate":130000,"contentLength":"1000","audioSampleRate":"48000","audioChannels":2,"url":"https://r.googlevideo.com/videoplayback?itag=251"}]},"microformat":{"playerMicroformatRenderer":{"publishDate":"2020-01-02","isUnlisted":false}}};</script>` +
	`</head><body></body></html>`

// TestFacade_WithFullMetadata checks the opt-in watch-page enrichment: the default
// path skips it, WithFullMetadata backfills publish date/chapters/availability with
// one extra fetch, and WithNoFallback makes it a no-op.
func TestFacade_WithFullMetadata(t *testing.T) {
	var watchCalls int
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			watchCalls++
			return resp(http.StatusOK, []byte(fullMetaWatchHTML)), nil
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return resp(http.StatusOK, []byte(fullMetaPlayerJSON)), nil
		default:
			return resp(http.StatusOK, nil), nil // homepage bootstrap, best-effort
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	base, err := c.InfoResult(ctx, "dummyVideo0", waxtap.InfoBasic)
	if err != nil {
		t.Fatalf("InfoResult: %v", err)
	}
	if base.FullMetadata {
		t.Error("FullMetadata should be false without WithFullMetadata")
	}
	if len(base.Video.Chapters) != 0 || !base.Video.PublishDate.IsZero() {
		t.Errorf("base video already enriched: chapters=%d publish=%v", len(base.Video.Chapters), base.Video.PublishDate)
	}
	if base.Video.Availability != waxtap.AvailabilityUnknown {
		t.Errorf("availability = %v, want unknown before enrichment", base.Video.Availability)
	}
	if watchCalls != 0 {
		t.Errorf("watchCalls = %d, want 0 without enrichment", watchCalls)
	}

	full, err := c.InfoResult(ctx, "dummyVideo0", waxtap.InfoBasic, waxtap.WithFullMetadata())
	if err != nil {
		t.Fatalf("InfoResult full: %v", err)
	}
	if !full.FullMetadata {
		t.Error("FullMetadata should be true after the pass")
	}
	if len(full.Video.Chapters) != 2 {
		t.Errorf("chapters = %d, want 2", len(full.Video.Chapters))
	}
	if got := full.Video.PublishDate.Format("2006-01-02"); got != "2020-01-02" {
		t.Errorf("publishDate = %s, want 2020-01-02", got)
	}
	if full.Video.Availability != waxtap.AvailabilityPublic {
		t.Errorf("availability = %v, want public", full.Video.Availability)
	}
	if watchCalls != 1 {
		t.Errorf("watchCalls = %d, want 1", watchCalls)
	}

	// WithNoFallback wins over WithFullMetadata: the watch page is forbidden.
	nf, err := c.InfoResult(ctx, "dummyVideo0", waxtap.InfoBasic, waxtap.WithFullMetadata(), waxtap.WithNoFallback())
	if err != nil {
		t.Fatalf("InfoResult no-fallback: %v", err)
	}
	if nf.FullMetadata {
		t.Error("FullMetadata should be false with WithNoFallback")
	}
	if watchCalls != 1 {
		t.Errorf("watchCalls = %d, want still 1 (no extra fetch under NoFallback)", watchCalls)
	}
}
