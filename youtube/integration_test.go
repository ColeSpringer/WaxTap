//go:build integration

// Live integration tests for extraction and resolution. They are excluded from
// the default build; run them with:
//
//	go test -tags=integration ./youtube/...
//
// These tests make real YouTube requests and are more brittle than the offline
// tests. Useful environment variables:
//   - WAXTAP_TEST_VIDEO=<id>  override the default video.
//   - WAXTAP_TEST_DEBUG=1     stream per-client slog to stderr (diagnose which
//     client won or why each fell through).
//
// TestLive_Extract covers metadata and format extraction. TestLive_ResolveAndRangeRead
// covers signed URL resolution and skips when the current network requires a PO
// token but no provider is configured.
package youtube_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube"
)

func liveVideoID() string {
	if v := os.Getenv("WAXTAP_TEST_VIDEO"); v != "" {
		return v
	}
	return "rFejpH_tAHM" // dotGo 2015, Rob Pike; stable, public, not age-gated
}

func newLiveClient() *youtube.Client {
	var cfg youtube.Config
	if os.Getenv("WAXTAP_TEST_DEBUG") != "" {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return youtube.New(cfg)
}

// TestLive_Extract checks that a stable public video still yields metadata and
// audio formats. It does not require stream resolution or a PO token.
func TestLive_Extract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ext, err := newLiveClient().Extract(ctx, liveVideoID())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if ext.Video().Title == "" {
		t.Error("extracted an empty title")
	}
	if len(ext.Video().Formats) == 0 {
		t.Fatal("no audio formats extracted")
	}
	t.Logf("extracted %q (%d audio formats)", ext.Video().Title, len(ext.Video().Formats))
}

// TestLive_ResolveAndRangeRead resolves the first audio format and reads a byte
// range from the signed URL. If only PO-token clients are available and no
// provider is configured, the test skips.
func TestLive_ResolveAndRangeRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := newLiveClient()
	ext, err := c.Extract(ctx, liveVideoID())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}

	plan, err := c.Resolve(ctx, ext, 0)
	if errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Skipf("skipping resolve/read: a PO token is required but no provider is configured "+
			"(no-token clients were unavailable on this network): %v", err)
	}
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.SABR != nil {
		t.Skip("skipping range read: format resolved to a SABR stream (no direct URL)")
	}
	rs := plan.Direct
	if rs == nil || rs.URL == "" {
		t.Fatal("resolved URL is empty")
	}
	t.Logf("resolved itag %d (expires %s, clen %d)", ext.Video().Formats[0].Itag, rs.ExpiresAt, rs.ContentLength)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rs.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range rs.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("Range", "bytes=0-1023")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range GET status = %d, want 200 or 206 (URL did not resolve to a playable stream)", resp.StatusCode)
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if n == 0 {
		t.Fatal("resolved stream returned no bytes")
	}
	t.Logf("read %d bytes from resolved stream (status %d)", n, resp.StatusCode)
}
