package youtube

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/internal/httpx"
)

func TestJSONUnescape(t *testing.T) {
	// A unicode-escaped "=" must decode; a plain value is returned unchanged.
	if got := jsonUnescape(`Cgs=`); got != "Cgs=" {
		t.Errorf("jsonUnescape = %q, want Cgs=", got)
	}
	if got := jsonUnescape(`CgtSb2JQaWtl`); got != "CgtSb2JQaWtl" {
		t.Errorf("jsonUnescape mangled a plain value: %q", got)
	}
}

func TestVisitorDataRegex(t *testing.T) {
	cases := []string{
		`ytcfg.set({"VISITOR_DATA":"REAL_VD_123","X":1});`,
		`{"client":{"clientName":"WEB","visitorData":"REAL_VD_123"}}`,
	}
	for _, body := range cases {
		m := visitorDataRe.FindStringSubmatch(body)
		if m == nil || m[1] != "REAL_VD_123" {
			t.Errorf("visitorData not extracted from %q: %v", body, m)
		}
	}
}

// jarClient builds a Client with a cookie jar so tests exercise the bootstrap
// path that persists YouTube's guest cookies.
func jarClient(t *testing.T, rt http.RoundTripper) *Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{HTTP: httpx.New(httpx.Config{
		HTTPClient:   &http.Client{Jar: jar, Transport: rt},
		MaxRetries:   1,
		MaxRetryWait: 50 * time.Millisecond,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   2 * time.Millisecond,
	})})
}

// TestExtract_BootstrapsRealVisitorData covers the full bootstrap path: one page
// fetch, visitorData in the player request, and cache reuse.
func TestExtract_BootstrapsRealVisitorData(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	homepage := []byte(`<html><script>ytcfg.set({"VISITOR_DATA":"REAL_VD_123"});</script></html>`)

	var homepageHits int
	var lastPlayerBody []byte
	c := jarClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/":
			homepageHits++
			return fixtureResp(http.StatusOK, homepage), nil
		case strings.Contains(r.URL.Path, "/player"):
			lastPlayerBody, _ = io.ReadAll(r.Body)
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	for i := 0; i < 2; i++ {
		if _, err := c.Extract(context.Background(), "dQw4w9WgXcQ"); err != nil {
			t.Fatalf("extract %d: %v", i, err)
		}
	}

	if homepageHits != 1 {
		t.Errorf("homepage fetched %d times, want 1 (bootstrap is cached)", homepageHits)
	}
	if !bytes.Contains(lastPlayerBody, []byte("REAL_VD_123")) {
		t.Errorf("player request did not carry the bootstrapped visitorData:\n%s", lastPlayerBody)
	}
}

// TestExtract_BootstrapFailureFallsBack ensures a failed bootstrap still allows
// extraction to continue with synthetic visitorData.
func TestExtract_BootstrapFailureFallsBack(t *testing.T) {
	ok := readFixture(t, "player_ok.json")

	var homepageHits, playerHits int
	c := jarClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/":
			homepageHits++
			return fixtureResp(http.StatusInternalServerError, nil), nil // bootstrap fails
		case strings.Contains(r.URL.Path, "/player"):
			playerHits++
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	ext, err := c.Extract(context.Background(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatalf("extraction should survive a failed bootstrap: %v", err)
	}
	if ext.Video().Title != "Test Song" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	if homepageHits == 0 || playerHits == 0 {
		t.Errorf("expected both a bootstrap attempt and a player call (homepage=%d player=%d)", homepageHits, playerHits)
	}
}

// TestExtract_NoJarSkipsBootstrap ensures jarless clients do not attempt the page
// fetch and stay on the synthetic visitorData path.
func TestExtract_NoJarSkipsBootstrap(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request (bootstrap should be skipped without a jar): %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	if _, err := c.Extract(context.Background(), "dQw4w9WgXcQ"); err != nil {
		t.Fatal(err)
	}
}
