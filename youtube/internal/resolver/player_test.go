package resolver

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/potoken"
)

const testBaseJSPath = "/s/player/abcd1234ef/player_ias.vflset/en_US/base.js"

// doerFunc adapts a function to the HTTPDoer interface.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// fixtureServer serves the authored embed page and base.js, counting hits per
// path so caching can be asserted.
type fixtureServer struct {
	t      *testing.T
	mu     sync.Mutex
	hits   map[string]int
	embed  []byte
	baseJS []byte
}

func newFixtureServer(t *testing.T) *fixtureServer {
	t.Helper()
	embed, err := os.ReadFile("testdata/embed.html")
	if err != nil {
		t.Fatalf("read embed.html: %v", err)
	}
	base, err := os.ReadFile("testdata/base.js")
	if err != nil {
		t.Fatalf("read base.js: %v", err)
	}
	return &fixtureServer{t: t, hits: map[string]int{}, embed: embed, baseJS: base}
}

func (s *fixtureServer) doer() HTTPDoer {
	return doerFunc(func(r *http.Request) (*http.Response, error) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()

		var body []byte
		switch {
		case strings.HasPrefix(r.URL.Path, "/embed/"):
			body = s.embed
		case r.URL.Path == testBaseJSPath:
			body = s.baseJS
		default:
			s.t.Errorf("unexpected request: %s", r.URL)
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})
}

func (s *fixtureServer) hitCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// cipherURL builds a signatureCipher bundle wrapping streamURL with signature s.
func cipherURL(s, sp, streamURL string) string {
	v := url.Values{}
	v.Set("s", s)
	v.Set("sp", sp)
	v.Set("url", streamURL)
	return v.Encode()
}

func TestPlayerResolve_SignatureCipher(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Config{HTTP: srv.doer()})

	stream := "https://rr1.googlevideo.com/videoplayback?itag=140&n=12345&expire=2000000000&clen=3400000"
	got, err := p.Resolve(context.Background(),
		Context{VideoID: "vid123", Headers: http.Header{"User-Agent": {"UA/1"}}},
		Candidate{SignatureCipher: cipherURL("ABCDEFGH", "sig", stream)})
	if err != nil {
		t.Fatal(err)
	}

	u, _ := url.Parse(got.URL)
	q := u.Query()
	if q.Get("sig") != "GFEDH" {
		t.Errorf("sig = %q, want GFEDH (deciphered)", q.Get("sig"))
	}
	if q.Get("n") != "54321" {
		t.Errorf("n = %q, want 54321 (decoded)", q.Get("n"))
	}
	if !got.ExpiresAt.Equal(time.Unix(2000000000, 0).UTC()) {
		t.Errorf("ExpiresAt = %v, want unix 2000000000", got.ExpiresAt)
	}
	if got.ContentLength != 3400000 {
		t.Errorf("ContentLength = %d, want 3400000", got.ContentLength)
	}
	if got.Headers.Get("User-Agent") != "UA/1" {
		t.Errorf("UA header not propagated: %v", got.Headers)
	}
}

func TestPlayerResolve_DirectURL(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Config{HTTP: srv.doer()})

	direct := "https://rr1.googlevideo.com/videoplayback?itag=251&n=12345&expire=2000000000&clen=3500000"
	got, err := p.Resolve(context.Background(),
		Context{VideoID: "vid123"},
		Candidate{URL: direct})
	if err != nil {
		t.Fatal(err)
	}

	q, _ := url.ParseQuery(mustQuery(got.URL))
	if q.Get("n") != "54321" {
		t.Errorf("n = %q, want 54321 (decoded even for direct URLs)", q.Get("n"))
	}
	if q.Get("sig") != "" {
		t.Errorf("unexpected sig on a direct URL: %q", q.Get("sig"))
	}
	if got.ContentLength != 3500000 {
		t.Errorf("ContentLength = %d, want 3500000", got.ContentLength)
	}
}

// TestPlayerResolve_Caches checks that player discovery and base.js fetches are
// shared across resolutions.
func TestPlayerResolve_Caches(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Config{HTTP: srv.doer()})

	direct := "https://rr1.googlevideo.com/videoplayback?itag=251&n=12345"
	for i := 0; i < 3; i++ {
		if _, err := p.Resolve(context.Background(), Context{VideoID: "vid123"}, Candidate{URL: direct}); err != nil {
			t.Fatal(err)
		}
	}
	if n := srv.hitCount(testBaseJSPath); n != 1 {
		t.Errorf("base.js fetched %d times, want 1 (program cache)", n)
	}
	if n := srv.hitCount("/embed/vid123"); n != 1 {
		t.Errorf("embed page fetched %d times, want 1 (URL cache)", n)
	}
}

func TestPlayerResolve_AppliesToken(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Config{HTTP: srv.doer()})

	tokenExp := time.Unix(1900000000, 0).UTC()
	direct := "https://rr1.googlevideo.com/videoplayback?itag=251&n=12345"
	got, err := p.Resolve(context.Background(),
		Context{
			VideoID: "vid123",
			Headers: http.Header{"User-Agent": {"UA/1"}},
			Token: &Token{
				Scope:   potoken.ScopeGVS,
				Value:   "POTOKEN123",
				Headers: http.Header{"X-Goog-Foo": {"bar"}},
				Expires: tokenExp,
			},
		},
		Candidate{URL: direct})
	if err != nil {
		t.Fatal(err)
	}

	q, _ := url.ParseQuery(mustQuery(got.URL))
	if q.Get("pot") != "POTOKEN123" {
		t.Errorf("pot = %q, want POTOKEN123", q.Get("pot"))
	}
	if got.Headers.Get("X-Goog-Foo") != "bar" {
		t.Errorf("token header not applied: %v", got.Headers)
	}
	// The URL carries no expire, so the token's expiry governs validity.
	if !got.ExpiresAt.Equal(tokenExp) {
		t.Errorf("ExpiresAt = %v, want token expiry %v", got.ExpiresAt, tokenExp)
	}
}

// TestPlayerResolve_PlayerURLOverride uses a caller-supplied player URL, skipping
// embed-page discovery entirely.
func TestPlayerResolve_PlayerURLOverride(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Config{HTTP: srv.doer()})

	direct := "https://rr1.googlevideo.com/videoplayback?itag=251&n=12345"
	_, err := p.Resolve(context.Background(),
		Context{VideoID: "vid123", PlayerURL: testBaseJSPath},
		Candidate{URL: direct})
	if err != nil {
		t.Fatal(err)
	}
	if n := srv.hitCount("/embed/vid123"); n != 0 {
		t.Errorf("embed fetched %d times, want 0 (player URL was supplied)", n)
	}
	if n := srv.hitCount(testBaseJSPath); n != 1 {
		t.Errorf("base.js fetched %d times, want 1", n)
	}
}

func TestPlayerSignatureTimestamp(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Config{HTTP: srv.doer()})

	sts, err := p.SignatureTimestamp(context.Background(), Context{PlayerURL: testBaseJSPath})
	if err != nil {
		t.Fatal(err)
	}
	if sts != 19834 {
		t.Errorf("signature timestamp = %d, want 19834", sts)
	}
}

func TestPlayerSignatureTimestamp_NoPattern(t *testing.T) {
	noSTS := `var Xq={sp:function(a,b){a.splice(0,b)}};` +
		`function dcr(a){a=a.split("");Xq.sp(a,1);return a.join("")}` +
		`;s&&(s=dcr(decodeURIComponent(s)));`
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == testBaseJSPath {
			return okResp(noSTS), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return notFoundResp(), nil
	})
	p := New(Config{HTTP: doer})

	sts, err := p.SignatureTimestamp(context.Background(), Context{PlayerURL: testBaseJSPath})
	if err != nil {
		t.Fatalf("unexpected error for missing signature timestamp: %v", err)
	}
	if sts != 0 {
		t.Errorf("signature timestamp = %d, want 0 (no pattern)", sts)
	}
}

func TestPlayerSignatureTimestamp_DiscoversFromVideoID(t *testing.T) {
	var embedPath string
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/embed/"):
			embedPath = r.URL.Path
			return okResp(`<script src="` + testBaseJSPath + `"></script>`), nil
		case r.URL.Path == testBaseJSPath:
			return okResp(`var cfg={signatureTimestamp:19834};` +
				`var Xq={sp:function(a,b){a.splice(0,b)}};` +
				`function dcr(a){a=a.split("");Xq.sp(a,1);return a.join("")}` +
				`;s&&(s=dcr(decodeURIComponent(s)));`), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return notFoundResp(), nil
	})
	p := New(Config{HTTP: doer})

	sts, err := p.SignatureTimestamp(context.Background(), Context{VideoID: "testVideo01"})
	if err != nil {
		t.Fatal(err)
	}
	if sts != 19834 {
		t.Errorf("signature timestamp = %d, want 19834", sts)
	}
	if embedPath != "/embed/testVideo01" {
		t.Errorf("discovery embed path = %q, want /embed/testVideo01", embedPath)
	}
}

func mustQuery(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.RawQuery
}

func notFoundResp() *http.Response {
	return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: http.NoBody, Header: make(http.Header)}
}

// TestPlayerResolve_DirectURLSurvivesPlayerFailure checks that a direct URL stays
// usable when player discovery is unavailable. base.js is only needed to decode
// n, so failure leaves the original n value in place.
func TestPlayerResolve_DirectURLSurvivesPlayerFailure(t *testing.T) {
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/embed/"), r.URL.Path == "/watch":
			return notFoundResp(), nil // discovery fails
		case r.URL.Path == testBaseJSPath:
			t.Errorf("base.js must not be fetched once discovery has failed")
			return okResp(""), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return notFoundResp(), nil
	})
	p := New(Config{HTTP: doer})

	direct := "https://rr1.googlevideo.com/videoplayback?itag=251&n=KEEPME&expire=2000000000&clen=999"
	got, err := p.Resolve(context.Background(), Context{VideoID: "v"}, Candidate{URL: direct})
	if err != nil {
		t.Fatalf("direct URL must remain usable when the player is unavailable: %v", err)
	}
	q, _ := url.ParseQuery(mustQuery(got.URL))
	if q.Get("n") != "KEEPME" {
		t.Errorf("n = %q, want KEEPME (original retained when player unavailable)", q.Get("n"))
	}
	if got.ContentLength != 999 {
		t.Errorf("ContentLength = %d, want 999", got.ContentLength)
	}
}

// TestPlayerResolve_SignatureCipherRequiresPlayer checks that ciphered candidates
// still fail without base.js.
func TestPlayerResolve_SignatureCipherRequiresPlayer(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return notFoundResp(), nil // all discovery fails
	})
	p := New(Config{HTTP: doer})

	_, err := p.Resolve(context.Background(), Context{VideoID: "v"},
		Candidate{SignatureCipher: cipherURL("ABCDEFGH", "sig", "https://rr1.googlevideo.com/videoplayback?itag=140")})
	if err == nil {
		t.Fatal("ciphered candidate must fail when the player JS is unavailable")
	}
}

func TestStreamExpiry(t *testing.T) {
	urlExp := time.Unix(2000000000, 0).UTC()
	withExpire := url.Values{"expire": {"2000000000"}}
	earlier := urlExp.Add(-time.Hour)
	later := urlExp.Add(time.Hour)

	cases := []struct {
		name string
		q    url.Values
		tok  *Token
		want time.Time
	}{
		{"no token", withExpire, nil, urlExp},
		{"earlier token caps", withExpire, &Token{Expires: earlier}, earlier},
		{"later token does not extend", withExpire, &Token{Expires: later}, urlExp},
		{"no url expiry falls to token", url.Values{}, &Token{Expires: earlier}, earlier},
		{"zero token expiry ignored", withExpire, &Token{}, urlExp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamExpiry(tc.q, "", tc.tok); !got.Equal(tc.want) {
				t.Errorf("streamExpiry = %v, want %v", got, tc.want)
			}
		})
	}
}

func okResp(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// TestPlayerResolve_NDecodeNonFatal checks that missing n-transform support keeps
// resolution usable with the original, throttled n value.
func TestPlayerResolve_NDecodeNonFatal(t *testing.T) {
	noN := `var Xq={sp:function(a,b){a.splice(0,b)}};` +
		`function dcr(a){a=a.split("");Xq.sp(a,1);return a.join("")}` +
		`;s&&(s=dcr(decodeURIComponent(s)));`
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/embed/"):
			return okResp(`<script src="` + testBaseJSPath + `"></script>`), nil
		case r.URL.Path == testBaseJSPath:
			return okResp(noN), nil
		default:
			t.Errorf("unexpected request: %s", r.URL)
			return okResp(""), nil
		}
	})
	p := New(Config{HTTP: doer})

	got, err := p.Resolve(context.Background(), Context{VideoID: "v"},
		Candidate{URL: "https://rr1.googlevideo.com/videoplayback?itag=251&n=KEEPME"})
	if err != nil {
		t.Fatalf("n-decode failure must be non-fatal, got: %v", err)
	}
	q, _ := url.ParseQuery(mustQuery(got.URL))
	if q.Get("n") != "KEEPME" {
		t.Errorf("n = %q, want KEEPME (decode failed, keep original throttled value)", q.Get("n"))
	}
}

// TestPlayerResolve_DiscoveryWatchFallback covers the watch-page fallback when
// the embed page does not carry a base.js URL.
func TestPlayerResolve_DiscoveryWatchFallback(t *testing.T) {
	base, err := os.ReadFile("testdata/base.js")
	if err != nil {
		t.Fatal(err)
	}
	var watchHits int
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/embed/"):
			return okResp(`<html>no player here</html>`), nil
		case r.URL.Path == "/watch":
			watchHits++
			return okResp(`<script src="` + testBaseJSPath + `"></script>`), nil
		case r.URL.Path == testBaseJSPath:
			return okResp(string(base)), nil
		default:
			t.Errorf("unexpected request: %s", r.URL)
			return okResp(""), nil
		}
	})
	p := New(Config{HTTP: doer})

	if _, err := p.Resolve(context.Background(), Context{VideoID: "v"},
		Candidate{URL: "https://rr1.googlevideo.com/videoplayback?itag=251&n=12345"}); err != nil {
		t.Fatal(err)
	}
	if watchHits != 1 {
		t.Errorf("watch page hits = %d, want 1 (embed lacked base.js)", watchHits)
	}
}
