package resolver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2/potoken"
	"github.com/colespringer/waxtap/v2/waxerr"
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
	base, err := os.ReadFile("testdata/player_synth.js")
	if err != nil {
		t.Fatalf("read player_synth.js: %v", err)
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
		// Discovery tries /watch first and /embed as fallback; both carry the same
		// base.js <script> tag, so serve the embed fixture for either.
		case r.URL.Path == "/watch", strings.HasPrefix(r.URL.Path, "/embed/"):
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
	if q.Get("sig") != "HGFEDCBA" {
		t.Errorf("sig = %q, want HGFEDCBA (deciphered)", q.Get("sig"))
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
	if n := srv.hitCount("/watch"); n != 1 {
		t.Errorf("watch page fetched %d times, want 1 (URL cache)", n)
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
// page discovery entirely.
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
	if n := srv.hitCount("/watch"); n != 0 {
		t.Errorf("watch page fetched %d times, want 0 (player URL was supplied)", n)
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
	var watchVideoID string
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			watchVideoID = r.URL.Query().Get("v")
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
	// The video id is carried in the watch page's ?v= query, not the path.
	if watchVideoID != "testVideo01" {
		t.Errorf("discovery watch ?v= = %q, want testVideo01", watchVideoID)
	}
}

func TestPlayerDescrambleN(t *testing.T) {
	srv := newFixtureServer(t)
	p := New(Config{HTTP: srv.doer()})

	raw := "https://r1---example.googlevideo.com/videoplayback?itag=251&n=12345&expire=2000000000"
	got, err := p.DescrambleN(context.Background(), Context{VideoID: "vid123"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := url.ParseQuery(mustQuery(got))
	if q.Get("n") != "54321" {
		t.Errorf("n = %q, want 54321 (descrambled)", q.Get("n"))
	}
	if q.Get("itag") != "251" || q.Get("expire") != "2000000000" {
		t.Errorf("descrambled URL dropped parameters: %s", got)
	}
	// Discovery must use the supplied video id; it lands in the watch page ?v=.
	if got := srv.hitCount("/watch"); got != 1 {
		t.Errorf("discovery watch hits = %d, want 1", got)
	}
}

func TestPlayerDescrambleN_NoN(t *testing.T) {
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request for a URL with no n: %s", r.URL)
		return notFoundResp(), nil
	})
	p := New(Config{HTTP: doer})

	raw := "https://r1---example.googlevideo.com/videoplayback?itag=140&expire=2000000000"
	got, err := p.DescrambleN(context.Background(), Context{VideoID: "vid123"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Errorf("DescrambleN(%q) = %q, want it unchanged", raw, got)
	}
}

func TestPlayerDescrambleN_PlayerUnavailable(t *testing.T) {
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		return notFoundResp(), nil // discovery fails
	})
	p := New(Config{HTTP: doer})

	if _, err := p.DescrambleN(context.Background(), Context{VideoID: "vid123"}, "https://r1.googlevideo.com/videoplayback?n=12345"); err == nil {
		t.Fatal("DescrambleN must return an error when the player is unavailable")
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
		case r.URL.Path == "/watch", strings.HasPrefix(r.URL.Path, "/embed/"):
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

// TestPlayerResolve_DiscoveryEmbedFallback covers the embed-page fallback when
// the watch page does not carry a base.js URL.
func TestPlayerResolve_DiscoveryEmbedFallback(t *testing.T) {
	base, err := os.ReadFile("testdata/player_synth.js")
	if err != nil {
		t.Fatal(err)
	}
	var embedHits int
	doer := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			return okResp(`<html>no player here</html>`), nil
		case strings.HasPrefix(r.URL.Path, "/embed/"):
			embedHits++
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
	if embedHits != 1 {
		t.Errorf("embed page hits = %d, want 1 (watch lacked base.js)", embedHits)
	}
}

// bodyDoer serves a fixed body of the given size on a 200 so the size guard in
// get can be exercised without a fixture file.
func bodyDoer(n int) doerFunc {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(bytes.NewReader(make([]byte, n))),
			Header:     make(http.Header),
		}, nil
	}
}

// TestPlayerGet_TruncationGuard confirms an over-cap player response is rejected
// with a clear error rather than silently truncated and handed to the cipher
// solver as corrupt JavaScript; a body exactly at the cap still reads back whole.
func TestPlayerGet_TruncationGuard(t *testing.T) {
	const rawURL = "https://www.youtube.com/s/player/x/base.js"

	p := New(Config{HTTP: bodyDoer(maxPlayerBytes + 16)})
	_, err := p.get(context.Background(), rawURL)
	if err == nil {
		t.Fatal("get on an over-cap body = nil error, want a truncation error")
	}
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Errorf("err = %v, want it to wrap ErrExtractionFailed", err)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("err = %v, want it to mention truncation", err)
	}

	// The cap is inclusive: a body of exactly maxPlayerBytes is not truncated.
	atCap := New(Config{HTTP: bodyDoer(maxPlayerBytes)})
	buf, err := atCap.get(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("get on an at-cap body: %v", err)
	}
	if len(buf) != maxPlayerBytes {
		t.Errorf("len(buf) = %d, want %d (whole body, not truncated)", len(buf), maxPlayerBytes)
	}
}
