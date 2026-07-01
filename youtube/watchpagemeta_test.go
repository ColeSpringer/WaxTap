package youtube

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestWatchPageMetadata fetches and parses publish date, chapters, and unlisted
// state from the watch page.
func TestWatchPageMetadata(t *testing.T) {
	html := readFixture(t, "watch_page.html")
	var calls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/watch" {
			calls++
			return fixtureResp(http.StatusOK, html), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	meta, err := c.WatchPageMetadata(context.Background(), "testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("watch calls = %d, want 1", calls)
	}
	if len(meta.Chapters) != 3 {
		t.Errorf("chapters = %d, want 3", len(meta.Chapters))
	}
	if got := meta.PublishDate.Format("2006-01-02"); got != "2021-05-20" {
		t.Errorf("publishDate = %s, want 2021-05-20", got)
	}
	if meta.Unlisted {
		t.Errorf("unlisted = true, want false")
	}
}

// TestWatchPageMetadataContextCanceled treats caller cancellation as fatal.
func TestWatchPageMetadataContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err() // transport honors cancellation
	}))
	if _, err := c.WatchPageMetadata(ctx, "testVideo01"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
