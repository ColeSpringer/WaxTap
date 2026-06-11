//go:build integration

// Live capture helpers for playlist browse shapes (the moving part behind
// ErrPlaylistParse). Excluded from the default build; run with
// -tags=integration. Capture what YouTube serves, then re-run the parser over
// the file offline:
//
//	WAXTAP_TEST_PLAYLIST=<id> go test -tags=integration -run TestLive_BrowseCapture ./youtube/ -v
//	WAXTAP_TEST_BROWSE_FILE=<path> go test -tags=integration -run TestLive_ParseBrowseFile ./youtube/ -v
package youtube

import (
	"context"
	"os"
	"testing"
)

// TestLive_BrowseCapture fetches a playlist's initial browse page through the
// same request path Enumerate uses and writes the raw body next to -v output,
// so a shape change can be inspected and minimized into a fixture.
func TestLive_BrowseCapture(t *testing.T) {
	id := os.Getenv("WAXTAP_TEST_PLAYLIST")
	if id == "" {
		t.Skip("set WAXTAP_TEST_PLAYLIST=<playlistID> to capture a live browse response")
	}
	c := New(Config{})
	sess, err := c.newBootstrappedSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	profile := c.playlistProfile()
	body, err := c.innertubePost(context.Background(), profile, sess, browseEndpoint, c.newPlaylistRequest(profile, sess, id, ""))
	if err != nil {
		t.Fatal(err)
	}
	out := "browse-" + id + ".json"
	if err := os.WriteFile(out, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d bytes to %s", len(body), out)
}

// TestLive_ParseBrowseFile runs parseBrowseInitial over a captured response
// file, validating the parser against live bytes without network access.
func TestLive_ParseBrowseFile(t *testing.T) {
	path := os.Getenv("WAXTAP_TEST_BROWSE_FILE")
	if path == "" {
		t.Skip("set WAXTAP_TEST_BROWSE_FILE=<path> to parse a captured browse response")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	meta, items, token, err := parseBrowseInitial(body)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("title=%q author=%q items=%d token=%dB", meta.title, meta.author, len(items), len(token))
	for i, it := range items[:min(3, len(items))] {
		e, err := it.toEntry(i)
		if err != nil {
			t.Errorf("toEntry(%d): %v", i, err)
			continue
		}
		t.Logf("entry %d: id=%s dur=%s author=%q title=%.40q", i, e.VideoID, e.Duration, e.Author, e.Title)
	}
}
