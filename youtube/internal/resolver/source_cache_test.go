package resolver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// nonPlayerBody mimics an HTTP 200 that is not player JS. It should neither be
// persisted nor trusted if already present in the source cache.
const nonPlayerBody = `<!DOCTYPE html><html><body>Access denied. Verify you are human.</body></html>`

// fakeSource records cache use so tests can tell whether base.js came from cache
// or from the network.
type fakeSource struct {
	mu   sync.Mutex
	data map[string][]byte
	puts int
	gets int
}

func newFakeSource() *fakeSource { return &fakeSource{data: map[string][]byte{}} }

func (s *fakeSource) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	v, ok := s.data[key]
	return v, ok
}

func (s *fakeSource) Put(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.data[key] = append([]byte(nil), data...)
}

// TestSourceCache_PopulatedOnFetch checks that a network fetch of base.js writes
// the source through to the cache exactly once.
func TestSourceCache_PopulatedOnFetch(t *testing.T) {
	srv := newFixtureServer(t)
	sc := newFakeSource()
	p := New(Config{HTTP: srv.doer(), SourceCache: sc})

	stream := "https://rr1.googlevideo.com/videoplayback?itag=140&n=12345"
	if _, err := p.Resolve(context.Background(),
		Context{VideoID: "vid123"},
		Candidate{SignatureCipher: cipherURL("ABCDEFGH", "sig", stream)}); err != nil {
		t.Fatal(err)
	}

	if n := srv.hitCount(testBaseJSPath); n != 1 {
		t.Fatalf("base.js fetched %d times, want 1", n)
	}
	if sc.puts != 1 {
		t.Fatalf("source cache puts = %d, want 1", sc.puts)
	}
}

// TestSourceCache_AvoidsNetworkOnHit covers a cold Player using source cached by
// an earlier Player, the same path a later process takes after reading disk.
func TestSourceCache_AvoidsNetworkOnHit(t *testing.T) {
	// Seed the shared source cache via a first player that does fetch base.js.
	seedSrv := newFixtureServer(t)
	sc := newFakeSource()
	seed := New(Config{HTTP: seedSrv.doer(), SourceCache: sc})
	stream := "https://rr1.googlevideo.com/videoplayback?itag=140&n=12345"
	cand := Candidate{SignatureCipher: cipherURL("ABCDEFGH", "sig", stream)}
	if _, err := seed.Resolve(context.Background(), Context{VideoID: "vid123"}, cand); err != nil {
		t.Fatal(err)
	}

	// A fresh player shares the source cache but has no compiled programs. Its
	// HTTP doer fails if base.js is fetched, and PlayerURL skips embed discovery
	// so only the program path is exercised.
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == testBaseJSPath {
			t.Errorf("base.js fetched over network despite a warm source cache")
		}
		return notFoundResp(), nil
	})
	fresh := New(Config{HTTP: doer, SourceCache: sc})
	if _, err := fresh.Resolve(context.Background(),
		Context{VideoID: "vid123", PlayerURL: testBaseJSPath}, cand); err != nil {
		t.Fatalf("resolve from warm source cache: %v", err)
	}
}

// TestSourceCache_DoesNotPersistNonPlayerBody checks that an HTTP 200 body with
// no player transforms is treated as network data only, not durable cache
// content.
func TestSourceCache_DoesNotPersistNonPlayerBody(t *testing.T) {
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		// Discovery is watch-first with /embed as fallback; serve either.
		case r.URL.Path == "/watch", strings.HasPrefix(r.URL.Path, "/embed/"):
			return okResp(`<script src="` + testBaseJSPath + `"></script>`), nil
		case r.URL.Path == testBaseJSPath:
			return okResp(nonPlayerBody), nil
		}
		return notFoundResp(), nil
	})
	sc := newFakeSource()
	p := New(Config{HTTP: doer, SourceCache: sc})

	// Direct URLs tolerate n-decode failure, so this exercises the fetch path
	// without making resolution fail.
	if _, err := p.Resolve(context.Background(), Context{VideoID: "v"},
		Candidate{URL: "https://rr1.googlevideo.com/videoplayback?itag=251&n=KEEP"}); err != nil {
		t.Fatal(err)
	}
	if sc.puts != 0 {
		t.Fatalf("a non-player body was persisted (puts=%d, want 0)", sc.puts)
	}
}

// TestSourceCache_PoisonedEntryFallsBackToNetwork covers cache files written
// before validation existed: the resolver ignores the bad body, refetches, and
// replaces it.
func TestSourceCache_PoisonedEntryFallsBackToNetwork(t *testing.T) {
	srv := newFixtureServer(t)
	sc := newFakeSource()
	// Seed a bad entry for the player URL the fixture discovers from /watch.
	sc.data["https://www.youtube.com"+testBaseJSPath] = []byte(nonPlayerBody)

	p := New(Config{HTTP: srv.doer(), SourceCache: sc})
	stream := "https://rr1.googlevideo.com/videoplayback?itag=140&n=12345"
	got, err := p.Resolve(context.Background(), Context{VideoID: "vid123"},
		Candidate{SignatureCipher: cipherURL("ABCDEFGH", "sig", stream)})
	if err != nil {
		t.Fatalf("a poisoned cache entry should fall back to the network, got: %v", err)
	}
	q, _ := url.ParseQuery(mustQuery(got.URL))
	if q.Get("sig") != "GFEDH" {
		t.Errorf("sig = %q, want GFEDH (deciphered from the refetched player)", q.Get("sig"))
	}
	if n := srv.hitCount(testBaseJSPath); n != 1 {
		t.Errorf("base.js fetched %d times, want 1 (poisoned entry forced a refetch)", n)
	}
}
