package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
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
		if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
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

// TestExtract_BootstrapFailureFallsBack verifies that a failed bootstrap still
// allows extraction to continue with synthetic visitorData.
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

	ext, err := c.Extract(context.Background(), "testVideo01")
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

// A bootstrap whose dial to the configured proxy fails ends the extraction
// there: the proxy is a fixed setting, so the chain's own dials would fail
// the same way after paying a second timeout. Only the proxy dial is fatal;
// TestExtract_BootstrapFailureFallsBack keeps a 500 best-effort.
func TestExtract_BootstrapProxyFailureIsFatal(t *testing.T) {
	var player atomic.Int32
	c := jarClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "www.youtube.com" && r.URL.Path == "/" {
			return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: &net.OpError{
				Op: "proxyconnect", Net: "tcp", Err: errors.New("dial tcp 127.0.0.1:1: i/o timeout"),
			}}
		}
		if strings.Contains(r.URL.Path, "/player") {
			player.Add(1)
		}
		return nil, errors.New("unexpected request: " + r.URL.String())
	}))
	_, err := c.Extract(context.Background(), "dummyVideo0")
	if !httpx.IsProxyConnect(err) {
		t.Fatalf("err = %v, want the proxy dial failure", err)
	}
	if !strings.Contains(err.Error(), "visitor bootstrap") {
		t.Errorf("err = %v, want it to name the bootstrap", err)
	}
	if n := player.Load(); n != 0 {
		t.Errorf("%d /player requests after the proxy failed, want none", n)
	}
}

// A channel URL resolves through InnerTube and falls back to a page scrape
// on most failures; each path bootstraps, and a failed bootstrap load is
// never cached, so without a guard a dead proxy would be dialled once per
// path. The proxy failure ends the resolve at the first.
func TestResolveChannelID_DeadProxyIsNotScraped(t *testing.T) {
	var homepage atomic.Int32
	c := jarClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "www.youtube.com" && r.URL.Path == "/" {
			homepage.Add(1)
			return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: &net.OpError{
				Op: "proxyconnect", Net: "tcp", Err: errors.New("dial tcp 127.0.0.1:1: i/o timeout"),
			}}
		}
		return nil, errors.New("unexpected request: " + r.URL.String())
	}))
	_, err := c.resolveChannelID(context.Background(), "https://www.youtube.com/@dummyChannel0")
	if !httpx.IsProxyConnect(err) {
		t.Fatalf("err = %v, want the proxy dial failure", err)
	}
	if !strings.Contains(err.Error(), "visitor bootstrap") {
		t.Errorf("err = %v, want it to name the bootstrap", err)
	}
	if n := homepage.Load(); n != 1 {
		t.Errorf("the homepage was dialled %d times, want once: the scrape must not bootstrap again", n)
	}
}

// TestExtract_NoJarSkipsBootstrap verifies that jarless clients do not attempt
// the page fetch and stay on the synthetic visitorData path.
func TestExtract_NoJarSkipsBootstrap(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request (bootstrap should be skipped without a jar): %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
}
