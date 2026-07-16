package sponsorblock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/waxerr"
)

const testVideoID = "testVideo01"

// serveJSON starts a test server returning status and body, and records the last
// request path for prefix/route assertions.
func serveJSON(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{HTTP: httpx.New(httpx.Config{HTTPClient: srv.Client()}), BaseURL: srv.URL})
	return c, &lastPath
}

func TestFetchSegments_HashPrefixRoute(t *testing.T) {
	c, lastPath := serveJSON(t, http.StatusOK, "[]")
	if _, err := c.FetchSegments(context.Background(), testVideoID, []Category{CategoryMusicOffTopic}); err != nil {
		t.Fatalf("FetchSegments: %v", err)
	}
	sum := sha256.Sum256([]byte(testVideoID))
	wantPrefix := hex.EncodeToString(sum[:])[:hashPrefixLen]
	want := "/api/skipSegments/" + wantPrefix
	if *lastPath != want {
		t.Errorf("request path = %q, want %q (4-char hash prefix, full ID not sent)", *lastPath, want)
	}
	if strings.Contains(*lastPath, testVideoID) {
		t.Errorf("request path %q leaked the full video ID", *lastPath)
	}
}

// TestFetchSegments_NormalizesBaseURL verifies a BaseURL with a trailing slash (or
// surrounding whitespace) does not produce a double-slash //api path that some
// servers 404 or redirect, and still reaches the endpoint cleanly.
func TestFetchSegments_NormalizesBaseURL(t *testing.T) {
	var lastPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{HTTP: httpx.New(httpx.Config{HTTPClient: srv.Client()}), BaseURL: "  " + srv.URL + "/  "})
	if _, err := c.FetchSegments(context.Background(), testVideoID, []Category{CategoryMusicOffTopic}); err != nil {
		t.Fatalf("FetchSegments with a padded trailing-slash BaseURL: %v", err)
	}
	if strings.HasPrefix(lastPath, "//") {
		t.Errorf("request path = %q, want no leading double slash from a trailing-slash BaseURL", lastPath)
	}
	if !strings.HasPrefix(lastPath, "/api/skipSegments/") {
		t.Errorf("request path = %q, want a clean /api/skipSegments/ route", lastPath)
	}
}

func TestFetchSegments_NotFoundIsEmpty(t *testing.T) {
	c, _ := serveJSON(t, http.StatusNotFound, "Not Found")
	segs, err := c.FetchSegments(context.Background(), testVideoID, nil)
	if err != nil {
		t.Fatalf("404 should not error, got %v", err)
	}
	if segs != nil {
		t.Errorf("404 segments = %v, want nil", segs)
	}
}

func TestFetchSegments_ServerError(t *testing.T) {
	c, _ := serveJSON(t, http.StatusBadRequest, "bad")
	_, err := c.FetchSegments(context.Background(), testVideoID, nil)
	if _, ok := errors.AsType[*waxerr.HTTPStatusError](err); !ok {
		t.Fatalf("err = %v, want *waxerr.HTTPStatusError", err)
	}
}

func TestFetchSegments_FiltersAndParses(t *testing.T) {
	// The array carries the target video plus another sharing the prefix, a mute
	// action, and an off-category segment. Only matching skip segments in the
	// requested categories survive.
	body := `[
	  {"videoID":"` + testVideoID + `","segments":[
	    {"category":"music_offtopic","actionType":"skip","segment":[0.0,5.5],"UUID":"a","locked":1,"votes":3,"videoDuration":212.0},
	    {"category":"music_offtopic","actionType":"mute","segment":[10.0,12.0],"UUID":"b","locked":0,"votes":9},
	    {"category":"sponsor","actionType":"skip","segment":[20.0,25.0],"UUID":"c","locked":0,"votes":50},
	    {"category":"intro","actionType":"skip","segment":[200.0,212.0],"UUID":"d","locked":0,"votes":1}
	  ]},
	  {"videoID":"someOtherVid","segments":[
	    {"category":"music_offtopic","actionType":"skip","segment":[1.0,2.0],"UUID":"z","locked":0,"votes":99}
	  ]}
	]`
	c, _ := serveJSON(t, http.StatusOK, body)
	segs, err := c.FetchSegments(context.Background(), testVideoID,
		[]Category{CategoryMusicOffTopic, CategoryIntro})
	if err != nil {
		t.Fatalf("FetchSegments: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (mute dropped, sponsor off-category, other video ignored): %+v", len(segs), segs)
	}
	first := segs[0]
	if first.Category != CategoryMusicOffTopic || first.Start != 0 || first.End != 5500*time.Millisecond {
		t.Errorf("segment[0] = %+v, want music_offtopic [0, 5.5s]", first)
	}
	if !first.Locked || first.Votes != 3 {
		t.Errorf("segment[0] locked/votes = %v/%d, want true/3", first.Locked, first.Votes)
	}
	if first.VideoDuration != 212*time.Second {
		t.Errorf("segment[0] video duration = %v, want 212s", first.VideoDuration)
	}
	if segs[1].Category != CategoryIntro {
		t.Errorf("segment[1] category = %v, want intro", segs[1].Category)
	}
}

func TestFetchSegments_DefaultsCategories(t *testing.T) {
	// An empty categories slice must still send a query (DefaultCategories), so the
	// route is hit rather than skipped.
	c, lastPath := serveJSON(t, http.StatusOK, "[]")
	if _, err := c.FetchSegments(context.Background(), testVideoID, nil); err != nil {
		t.Fatalf("FetchSegments: %v", err)
	}
	if *lastPath == "" {
		t.Error("empty categories should still query the API")
	}
}

func TestFetchSegments_EmptyVideoID(t *testing.T) {
	c, _ := serveJSON(t, http.StatusOK, "[]")
	if _, err := c.FetchSegments(context.Background(), "", nil); err == nil {
		t.Error("empty video id should error")
	}
}

// serveCounting starts a server returning body for every request and counts the
// requests, so cache behavior is observable. cacheTTL is passed through to the
// client (negative disables caching).
func serveCounting(t *testing.T, cacheTTL time.Duration, body string) (*Client, *int) {
	t.Helper()
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New(Config{
		HTTP:     httpx.New(httpx.Config{HTTPClient: srv.Client()}),
		BaseURL:  srv.URL,
		CacheTTL: cacheTTL,
	})
	return c, &count
}

const oneSegmentBody = `[{"videoID":"` + testVideoID + `","segments":[` +
	`{"category":"music_offtopic","actionType":"skip","segment":[0.0,5.0],"UUID":"a","locked":0,"votes":1}]}]`

func TestFetchSegments_CachesResponse(t *testing.T) {
	c, count := serveCounting(t, 0, oneSegmentBody) // 0 => default TTL (caching on)
	cats := []Category{CategoryMusicOffTopic}

	first, err := c.FetchSegments(context.Background(), testVideoID, cats)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	second, err := c.FetchSegments(context.Background(), testVideoID, cats)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if *count != 1 {
		t.Errorf("server was hit %d times, want 1 (second served from cache)", *count)
	}
	if len(first) != 1 || len(second) != 1 || first[0] != second[0] {
		t.Errorf("cached result differs: %+v vs %+v", first, second)
	}
}

func TestFetchSegments_CacheDisabled(t *testing.T) {
	c, count := serveCounting(t, -1, oneSegmentBody) // negative => caching off
	cats := []Category{CategoryMusicOffTopic}

	for range 2 {
		if _, err := c.FetchSegments(context.Background(), testVideoID, cats); err != nil {
			t.Fatalf("fetch: %v", err)
		}
	}
	if *count != 2 {
		t.Errorf("server was hit %d times, want 2 (caching disabled)", *count)
	}
}

func TestBestPerOverlap(t *testing.T) {
	sec := func(n float64) time.Duration { return time.Duration(n * float64(time.Second)) }
	t.Run("collapses-overlap-to-locked", func(t *testing.T) {
		segs := []Segment{
			{Start: sec(0), End: sec(10), Votes: 2},
			{Start: sec(1), End: sec(11), Locked: true, Votes: 1}, // overlaps; locked wins
		}
		got := bestPerOverlap(segs)
		if len(got) != 1 || !got[0].Locked {
			t.Fatalf("got %+v, want the single locked segment", got)
		}
	})
	t.Run("collapses-overlap-to-most-voted", func(t *testing.T) {
		segs := []Segment{
			{Start: sec(0), End: sec(10), Votes: 2},
			{Start: sec(5), End: sec(15), Votes: 9}, // overlaps; more votes
		}
		got := bestPerOverlap(segs)
		if len(got) != 1 || got[0].Votes != 9 {
			t.Fatalf("got %+v, want the 9-vote segment", got)
		}
	})
	t.Run("keeps-disjoint-and-touching", func(t *testing.T) {
		segs := []Segment{
			{Start: sec(0), End: sec(10), Votes: 1},
			{Start: sec(10), End: sec(20), Votes: 1}, // touches, not overlapping
			{Start: sec(30), End: sec(40), Votes: 1}, // disjoint
		}
		if got := bestPerOverlap(segs); len(got) != 3 {
			t.Fatalf("got %d segments, want 3 distinct regions: %+v", len(got), got)
		}
	})
}
