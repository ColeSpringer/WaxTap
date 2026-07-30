package youtube

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/potoken"
)

// TestResetGuestIdentity_ForcesFreshBootstrap covers the rotation mechanism: a
// reset drops the cached visitorData and the youtube.com cookies, so the next
// extraction re-fetches the homepage without the old identity and adopts a new
// one.
func TestResetGuestIdentity_ForcesFreshBootstrap(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	var homepageHits int
	var homepageCookies []string // Cookie header of each homepage request
	var lastPlayerBody []byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/":
			homepageHits++
			homepageCookies = append(homepageCookies, r.Header.Get("Cookie"))
			vd := "FIRST_VD"
			if homepageHits > 1 {
				vd = "SECOND_VD"
			}
			resp := fixtureResp(http.StatusOK, []byte(`<html><script>ytcfg.set({"VISITOR_DATA":"`+vd+`"});</script></html>`))
			resp.Header.Add("Set-Cookie", "VISITOR_INFO1_LIVE=vi-"+vd+"; Domain=.youtube.com; Path=/; Max-Age=31536000")
			return resp, nil
		case strings.Contains(r.URL.Path, "/player"):
			lastPlayerBody, _ = io.ReadAll(r.Body)
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	c := New(Config{HTTP: httpx.New(httpx.Config{
		HTTPClient:   &http.Client{Jar: jar, Transport: rt},
		MaxRetries:   1,
		MaxRetryWait: 50 * time.Millisecond,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   2 * time.Millisecond,
	})})

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract 1: %v", err)
	}
	if !bytes.Contains(lastPlayerBody, []byte("FIRST_VD")) {
		t.Fatalf("first player request should carry the first visitorData:\n%s", lastPlayerBody)
	}

	if !c.ResetGuestIdentity(ext.IdentityGeneration()) {
		t.Fatal("ResetGuestIdentity = false, want true for a guest client")
	}

	ytu := &url.URL{Scheme: "https", Host: "www.youtube.com"}
	var consent bool
	for _, ck := range jar.Cookies(ytu) {
		switch ck.Name {
		case "VISITOR_INFO1_LIVE":
			t.Errorf("VISITOR_INFO1_LIVE survived the reset (value %q)", ck.Value)
		case "CONSENT":
			consent = true
		}
	}
	if !consent {
		t.Error("CONSENT cookie missing after reset; it must be re-seeded")
	}

	ext2, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract 2: %v", err)
	}
	if homepageHits != 2 {
		t.Errorf("homepage fetched %d times, want 2 (reset must drop the visitor cache)", homepageHits)
	}
	if len(homepageCookies) == 2 && strings.Contains(homepageCookies[1], "VISITOR_INFO1_LIVE") {
		t.Errorf("second bootstrap carried the old identity cookie: %q", homepageCookies[1])
	}
	if !bytes.Contains(lastPlayerBody, []byte("SECOND_VD")) {
		t.Errorf("second player request should carry the re-bootstrapped visitorData:\n%s", lastPlayerBody)
	}

	// A reset keyed to the already-replaced identity must not wipe the new one:
	// a staggered 403 from an old URL arriving after a rotation is satisfied by
	// the rotation that already happened.
	if !c.ResetGuestIdentity(ext.IdentityGeneration()) {
		t.Fatal("stale-generation reset should report the old identity as gone")
	}
	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatalf("extract 3: %v", err)
	}
	if homepageHits != 2 {
		t.Errorf("homepage fetched %d times, want 2 (a stale-generation reset must not wipe the current identity)", homepageHits)
	}
	if ext2.IdentityGeneration() == ext.IdentityGeneration() {
		t.Errorf("identity generation did not advance across the reset: %d", ext2.IdentityGeneration())
	}
}

// blockingJar wraps a jar and blocks the first Cookies call until gate closes,
// so a test can hold one reset inside the cookie wipe while other resets queue.
type blockingJar struct {
	http.CookieJar
	gate  chan struct{}
	wipes atomic.Int32
}

func (j *blockingJar) Cookies(u *url.URL) []*http.Cookie {
	if j.wipes.Add(1) == 1 {
		<-j.gate
	}
	return j.CookieJar.Cookies(u)
}

// TestResetGuestIdentity_ConcurrentCallsCollapse pins the reset semantics: calls
// carrying the same identity generation collapse into one wipe (all report
// success), while a call carrying the new generation performs its own reset.
// The latter must keep working: a freshly rotated identity can itself be
// capped, and suppressing its rotation would strand that download.
func TestResetGuestIdentity_ConcurrentCallsCollapse(t *testing.T) {
	inner, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar := &blockingJar{CookieJar: inner, gate: make(chan struct{})}
	c := New(Config{HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar}})})

	results := make(chan bool, 3)
	gen := c.resetSeq.Load()
	go func() { results <- c.ResetGuestIdentity(gen) }() // will block inside the wipe
	deadline := time.Now().Add(5 * time.Second)
	for jar.wipes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first reset never reached the cookie wipe")
		}
		time.Sleep(time.Millisecond)
	}

	// These target the same identity while the first reset holds the lock
	// mid-wipe; they must collapse into it rather than wipe again.
	go func() { results <- c.ResetGuestIdentity(gen) }()
	go func() { results <- c.ResetGuestIdentity(gen) }()
	time.Sleep(100 * time.Millisecond) // let both reach the reset lock
	close(jar.gate)

	for range 3 {
		if !<-results {
			t.Error("a collapsed reset must still report the identity as discarded")
		}
	}
	if got := jar.wipes.Load(); got != 1 {
		t.Errorf("cookie wipes = %d, want 1 (same-generation resets must collapse)", got)
	}

	// A reset keyed to the rotated-to identity is a new escape request.
	if !c.ResetGuestIdentity(c.resetSeq.Load()) {
		t.Fatal("current-generation reset refused")
	}
	if got := jar.wipes.Load(); got != 2 {
		t.Errorf("cookie wipes after current-generation reset = %d, want 2", got)
	}
}

// TestResetGuestIdentity_JarlessRefuses pins that a jarless client reports no
// rotation: it has no durable guest identity to discard (every extraction
// already mints fresh synthetic visitorData), so claiming one was replaced
// would be false.
func TestResetGuestIdentity_JarlessRefuses(t *testing.T) {
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	if c.ResetGuestIdentity(0) {
		t.Fatal("ResetGuestIdentity = true for a jarless client, want false")
	}
}

// TestResetGuestIdentity_WaitsForInFlightBootstrap pins the reset/bootstrap
// serialization: a reset must not interleave with an in-flight homepage
// bootstrap (it could strip the cookies the response just installed, or let
// the flight re-cache the identity the reset was discarding). The reset waits,
// then discards the bootstrap's result, so the next extraction re-fetches.
func TestResetGuestIdentity_WaitsForInFlightBootstrap(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	var homepageHits atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/":
			if homepageHits.Add(1) == 1 {
				<-gate // hold the first bootstrap in flight
			}
			return fixtureResp(http.StatusOK, []byte(`<html><script>ytcfg.set({"VISITOR_DATA":"VD"});</script></html>`)), nil
		case strings.Contains(r.URL.Path, "/player"):
			return fixtureResp(http.StatusOK, ok), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	c := New(Config{HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar, Transport: rt}})})

	extractDone := make(chan error, 1)
	go func() {
		_, err := c.Extract(context.Background(), "testVideo01")
		extractDone <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for homepageHits.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("bootstrap never reached the homepage")
		}
		time.Sleep(time.Millisecond)
	}

	resetDone := make(chan bool, 1)
	go func() { resetDone <- c.ResetGuestIdentity(c.resetSeq.Load()) }()
	select {
	case <-resetDone:
		t.Fatal("reset completed while the bootstrap it must wait for was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(gate)
	if !<-resetDone {
		t.Fatal("reset refused")
	}
	if err := <-extractDone; err != nil {
		t.Fatalf("extract during reset: %v", err)
	}

	// The waited-out reset discarded the flight's result, so a fresh extraction
	// bootstraps again.
	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if got := homepageHits.Load(); got != 2 {
		t.Errorf("homepage hits = %d, want 2 (the reset must discard the in-flight bootstrap's identity)", got)
	}
}

// TestResetGuestIdentity_AdoptedSessionRefuses pins that an adopted identity is
// never discarded: it is externally owned and PO-token content binding depends
// on it.
func TestResetGuestIdentity_AdoptedSessionRefuses(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{
		HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar}}),
		Session: &potoken.Session{
			VisitorData: "ADOPTED_VD",
			Cookies:     []*http.Cookie{{Name: "VISITOR_INFO1_LIVE", Value: "vi-adopted", Domain: ".youtube.com", Path: "/"}},
		},
	})
	if c.ResetGuestIdentity(0) {
		t.Fatal("ResetGuestIdentity = true, want false under adoption")
	}
	ytu := &url.URL{Scheme: "https", Host: "www.youtube.com"}
	var kept bool
	for _, ck := range jar.Cookies(ytu) {
		if ck.Name == "VISITOR_INFO1_LIVE" {
			kept = true
		}
	}
	if !kept {
		t.Error("adopted session cookies must survive a refused reset")
	}
}
