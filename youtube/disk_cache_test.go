package youtube

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/internal/httpx"
)

// stubTransport serves a synthetic embed page and base.js, counting base.js
// fetches so the disk cache's effect can be asserted. When failBaseJS is set, a
// base.js request is recorded and answered 404, modeling a process that must rely
// on the on-disk source cache.
type stubTransport struct {
	mu          sync.Mutex
	baseJSHits  int
	failBaseJS  bool
	baseJSPath  string
	baseJSBody  string
	embedScript string
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp := func(code int, body string) *http.Response {
		return &http.Response{
			StatusCode: code,
			Status:     http.StatusText(code),
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
			Request:    r,
		}
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/embed/"):
		return resp(http.StatusOK, s.embedScript), nil
	case r.URL.Path == s.baseJSPath:
		s.mu.Lock()
		s.baseJSHits++
		fail := s.failBaseJS
		s.mu.Unlock()
		if fail {
			return resp(http.StatusNotFound, ""), nil
		}
		return resp(http.StatusOK, s.baseJSBody), nil
	default:
		return resp(http.StatusNotFound, ""), nil
	}
}

func (s *stubTransport) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseJSHits
}

// directURLExtraction builds an extraction whose only format is a direct URL with
// a throttled n parameter. Resolving it loads base.js to decode n (non-fatally),
// which is enough to exercise the source cache without a real signature cipher.
func directURLExtraction() *Extraction {
	return &Extraction{
		video:   &Video{ID: "vid123"},
		profile: makeProfile(profileAndroidVR), // no PO token needed
		session: newSession("US"),
		rawAudio: []rawFormat{{
			Itag:          140,
			URL:           "https://rr1.googlevideo.com/videoplayback?itag=140&n=THROTTLED&expire=2000000000&clen=100",
			ContentLength: "100",
		}},
		expiresAt: time.Unix(2000000000, 0).UTC(),
	}
}

const stubBaseJSPath = "/s/player/test123/player_ias.vflset/en_US/base.js"

// stubBaseJS is a minimal body that the resolver can extract a signature
// transform from, so it counts as genuine player JS worth persisting. (A bare
// comment would yield no transform and is intentionally not cached.)
const stubBaseJS = `function abc(a){a=a.split("");a.reverse();return a.join("")};`

func newStubClient(t *testing.T, cacheDir string, st *stubTransport) *Client {
	t.Helper()
	hc := httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: st}})
	return New(Config{HTTP: hc, CacheDir: cacheDir})
}

// TestNew_DiskCachePersistsBaseJS checks that the default resolver built by New
// writes base.js to the configured cache dir, and that a second client with a
// cold in-memory cache reads it back from disk instead of refetching.
func TestNew_DiskCachePersistsBaseJS(t *testing.T) {
	cacheDir := t.TempDir()
	embed := `<script src="` + stubBaseJSPath + `"></script>`

	// First client fetches base.js and should persist it.
	st1 := &stubTransport{baseJSPath: stubBaseJSPath, baseJSBody: stubBaseJS, embedScript: embed}
	c1 := newStubClient(t, cacheDir, st1)
	if _, err := c1.Resolve(context.Background(), directURLExtraction(), 0); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if st1.hits() != 1 {
		t.Fatalf("base.js fetched %d times on first client, want 1", st1.hits())
	}
	playerDir := filepath.Join(cacheDir, "players", "v"+strconv.Itoa(playerCacheSchema))
	entries, err := os.ReadDir(playerDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected a persisted base.js under %s, got entries=%v err=%v", playerDir, entries, err)
	}

	// Second client shares the cache dir but a cold memory cache, and its
	// transport 404s base.js. Resolution must still succeed from disk and never
	// hit the network for base.js.
	st2 := &stubTransport{baseJSPath: stubBaseJSPath, embedScript: embed, failBaseJS: true}
	c2 := newStubClient(t, cacheDir, st2)
	if _, err := c2.Resolve(context.Background(), directURLExtraction(), 0); err != nil {
		t.Fatalf("second resolve should succeed from disk cache: %v", err)
	}
	if st2.hits() != 0 {
		t.Fatalf("base.js fetched %d times on second client, want 0 (served from disk)", st2.hits())
	}
}

// TestNew_DiskCacheDisabled checks that DisableDiskCache leaves nothing on disk.
func TestNew_DiskCacheDisabled(t *testing.T) {
	cacheDir := t.TempDir()
	embed := `<script src="` + stubBaseJSPath + `"></script>`
	st := &stubTransport{baseJSPath: stubBaseJSPath, baseJSBody: stubBaseJS, embedScript: embed}
	hc := httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: st}})
	c := New(Config{HTTP: hc, CacheDir: cacheDir, DisableDiskCache: true})

	if _, err := c.Resolve(context.Background(), directURLExtraction(), 0); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "players")); !os.IsNotExist(err) {
		t.Fatalf("disk cache should be disabled, but players dir exists (err=%v)", err)
	}
}
