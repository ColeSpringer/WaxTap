package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/potoken"
)

// TestRotateIdentity_ForcesFreshBootstrap covers the rotation mechanism: a
// reset drops the cached visitorData and the youtube.com cookies, so the next
// extraction re-fetches the homepage without the old identity and adopts a new
// one.
func TestRotateIdentity_ForcesFreshBootstrap(t *testing.T) {
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

	if !c.RotateIdentity(context.Background(), ext.IdentityGeneration(), "") {
		t.Fatal("RotateIdentity = false, want true for a guest client")
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
	if !c.RotateIdentity(context.Background(), ext.IdentityGeneration(), "") {
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

// TestRotateIdentity_ConcurrentCallsCollapse pins the reset semantics: calls
// carrying the same identity generation collapse into one wipe (all report
// success), while a call carrying the new generation performs its own reset.
// The latter must keep working: a freshly rotated identity can itself be
// capped, and suppressing its rotation would strand that download.
func TestRotateIdentity_ConcurrentCallsCollapse(t *testing.T) {
	inner, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar := &blockingJar{CookieJar: inner, gate: make(chan struct{})}
	c := New(Config{HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar}})})

	results := make(chan bool, 3)
	gen := c.resetSeq.Load()
	go func() { results <- c.RotateIdentity(context.Background(), gen, "") }() // will block inside the wipe
	deadline := time.Now().Add(5 * time.Second)
	for jar.wipes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first reset never reached the cookie wipe")
		}
		time.Sleep(time.Millisecond)
	}

	// These target the same identity while the first reset holds the lock
	// mid-wipe; they must collapse into it rather than wipe again.
	go func() { results <- c.RotateIdentity(context.Background(), gen, "") }()
	go func() { results <- c.RotateIdentity(context.Background(), gen, "") }()
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
	if !c.RotateIdentity(context.Background(), c.resetSeq.Load(), "") {
		t.Fatal("current-generation reset refused")
	}
	if got := jar.wipes.Load(); got != 2 {
		t.Errorf("cookie wipes after current-generation reset = %d, want 2", got)
	}
}

// TestRotateIdentity_JarlessRefuses pins that a jarless client reports no
// rotation: it has no durable guest identity to discard (every extraction
// already mints fresh synthetic visitorData), so claiming one was replaced
// would be false.
func TestRotateIdentity_JarlessRefuses(t *testing.T) {
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	if c.RotateIdentity(context.Background(), 0, "") {
		t.Fatal("RotateIdentity = true for a jarless client, want false")
	}
}

// TestRotateIdentity_WaitsForInFlightBootstrap pins the reset/bootstrap
// serialization: a reset must not interleave with an in-flight homepage
// bootstrap (it could strip the cookies the response just installed, or let
// the flight re-cache the identity the reset was discarding). The reset waits,
// then discards the bootstrap's result, so the next extraction re-fetches.
func TestRotateIdentity_WaitsForInFlightBootstrap(t *testing.T) {
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
	go func() { resetDone <- c.RotateIdentity(context.Background(), c.resetSeq.Load(), "") }()
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

// TestRotateIdentity_StaticSessionRefuses pins that a static session is never
// discarded: its caller supplied a fixed value and there is no provider to
// retire it with, so the identity must survive intact.
func TestRotateIdentity_StaticSessionRefuses(t *testing.T) {
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
	if c.RotateIdentity(context.Background(), 0, "") {
		t.Fatal("RotateIdentity = true, want false for a static session")
	}
	ytu := &url.URL{Scheme: "https", Host: "www.youtube.com"}
	var kept bool
	for _, ck := range jar.Cookies(ytu) {
		if ck.Name == "VISITOR_INFO1_LIVE" {
			kept = true
		}
	}
	if !kept {
		t.Error("adopted session cookies must survive a refused rotation")
	}
}

// stubSessionProvider hands out numbered guest sessions and records the
// invalidations it is asked for. failWith makes every invalidation fail, the
// way a rate-limited or unreachable minter would; cookieless and unversioned
// hand out visitorData-only and Generation-0 sessions.
type stubSessionProvider struct {
	mu          sync.Mutex
	served      int
	reports     []potoken.SessionInvalidation
	failWith    error
	cookieless  bool
	unversioned bool
}

func (p *stubSessionProvider) ProvideSession(context.Context) (potoken.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.served++
	sess := potoken.Session{
		VisitorData: fmt.Sprintf("ADOPTED_VD_%d", p.served),
		Generation:  uint64(p.served),
	}
	if p.unversioned {
		sess.Generation = 0
	}
	if !p.cookieless {
		sess.Cookies = []*http.Cookie{{Name: "VISITOR_INFO1_LIVE", Value: fmt.Sprintf("vi-%d", p.served), Domain: ".youtube.com", Path: "/"}}
	}
	return sess, nil
}

func (p *stubSessionProvider) InvalidateSession(_ context.Context, inv potoken.SessionInvalidation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failWith != nil {
		return p.failWith
	}
	p.reports = append(p.reports, inv)
	return nil
}

// plainSessionProvider adopts a session but cannot retire it.
type plainSessionProvider struct{}

func (plainSessionProvider) ProvideSession(context.Context) (potoken.Session, error) {
	return potoken.Session{VisitorData: "ADOPTED_VD", Generation: 1}, nil
}

// adoptingClient builds a Client that adopts from provider and serves /player
// from the OK fixture. Any other request fails the test: adoption must never
// bootstrap a guest identity.
func adoptingClient(t *testing.T, provider potoken.SessionProvider, jar http.CookieJar, lastPlayerBody *[]byte) *Client {
	t.Helper()
	ok := readFixture(t, "player_ok.json")
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			*lastPlayerBody, _ = io.ReadAll(r.Body)
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s (an adopted session must not bootstrap)", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	return New(Config{
		HTTP:            httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar, Transport: rt}}),
		SessionProvider: provider,
	})
}

// TestRotateIdentity_AdoptedRotatesThroughProvider is the WaxSeal-mediated
// escape: a capped adopted session is reported to its provider, its cookies are
// dropped, and the next extraction adopts the replacement.
func TestRotateIdentity_AdoptedRotatesThroughProvider(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var lastPlayerBody []byte
	p := &stubSessionProvider{}
	c := adoptingClient(t, p, jar, &lastPlayerBody)

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract 1: %v", err)
	}
	if !bytes.Contains(lastPlayerBody, []byte("ADOPTED_VD_1")) {
		t.Fatalf("first player request should carry the first adopted visitorData:\n%s", lastPlayerBody)
	}

	if !c.RotateIdentity(context.Background(), ext.IdentityGeneration(), "testVideo01") {
		t.Fatal("RotateIdentity = false, want true for an invalidating provider")
	}
	if len(p.reports) != 1 {
		t.Fatalf("invalidations = %d, want 1", len(p.reports))
	}
	if got := p.reports[0]; got.Generation != 1 || got.VideoID != "testVideo01" || got.Reason != invalidationReasonDeliveryCap {
		t.Errorf("invalidation = %+v, want generation 1 for testVideo01 with reason %q", got, invalidationReasonDeliveryCap)
	}
	for _, ck := range jar.Cookies(&url.URL{Scheme: "https", Host: "www.youtube.com"}) {
		if ck.Name == "VISITOR_INFO1_LIVE" && ck.Value == "vi-1" {
			t.Error("the retired session's cookie survived the rotation")
		}
	}

	ext2, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract 2: %v", err)
	}
	if p.served != 2 {
		t.Errorf("sessions served = %d, want 2 (the rotation must drop the cached adoption)", p.served)
	}
	if !bytes.Contains(lastPlayerBody, []byte("ADOPTED_VD_2")) {
		t.Errorf("second player request should carry the replacement visitorData:\n%s", lastPlayerBody)
	}
	if ext2.IdentityGeneration() == ext.IdentityGeneration() {
		t.Errorf("identity generation did not advance across the rotation: %d", ext2.IdentityGeneration())
	}
}

// TestRotateIdentity_AdoptedRefusesWithoutInvalidator pins that a provider with
// no way to retire what it handed out reports no rotation, leaving the caller
// on the plain re-resolve rather than re-adopting the same capped session.
func TestRotateIdentity_AdoptedRefusesWithoutInvalidator(t *testing.T) {
	c := New(Config{
		HTTP:            httpx.New(httpx.Config{HTTPClient: &http.Client{}}),
		SessionProvider: plainSessionProvider{},
	})
	if c.RotateIdentity(context.Background(), 0, "") {
		t.Fatal("RotateIdentity = true, want false for a provider that cannot invalidate")
	}
}

// TestRotateIdentity_AdoptedKeepsSessionWhenReportFails pins that a rejected
// invalidation changes nothing: the session the provider still owns stays
// adopted, cookies and all.
func TestRotateIdentity_AdoptedKeepsSessionWhenReportFails(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var lastPlayerBody []byte
	p := &stubSessionProvider{failWith: errors.New("recycling is rate-limited")}
	c := adoptingClient(t, p, jar, &lastPlayerBody)

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract 1: %v", err)
	}
	if c.RotateIdentity(context.Background(), ext.IdentityGeneration(), "testVideo01") {
		t.Fatal("RotateIdentity = true, want false when the provider rejects the report")
	}

	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatalf("extract 2: %v", err)
	}
	if p.served != 1 {
		t.Errorf("sessions served = %d, want 1 (a failed report must not drop the adoption)", p.served)
	}
	if !bytes.Contains(lastPlayerBody, []byte("ADOPTED_VD_1")) {
		t.Errorf("the adopted session must survive a failed report:\n%s", lastPlayerBody)
	}
	var kept bool
	for _, ck := range jar.Cookies(&url.URL{Scheme: "https", Host: "www.youtube.com"}) {
		if ck.Name == "VISITOR_INFO1_LIVE" && ck.Value == "vi-1" {
			kept = true
		}
	}
	if !kept {
		t.Error("adopted cookies must survive a failed report")
	}
}

// TestRotateIdentity_AdoptedRotationDropsServerSetCookies pins the wipe against
// cookies the retired session accumulated on its own: a visitorData-only
// adoption seeds nothing, but youtube.com Set-Cookies identity anchors on
// player responses during use, and those must not survive into the replacement
// session. CONSENT is re-seeded like the guest wipe does.
func TestRotateIdentity_AdoptedRotationDropsServerSetCookies(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			resp := fixtureResp(http.StatusOK, ok)
			resp.Header.Add("Set-Cookie", "VISITOR_INFO1_LIVE=vi-server; Domain=.youtube.com; Path=/; Max-Age=31536000")
			return resp, nil
		}
		t.Errorf("unexpected request: %s (an adopted session must not bootstrap)", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	p := &stubSessionProvider{cookieless: true}
	c := New(Config{
		HTTP:            httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar, Transport: rt}}),
		SessionProvider: p,
	})

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	ytu := &url.URL{Scheme: "https", Host: "www.youtube.com"}
	var stored bool
	for _, ck := range jar.Cookies(ytu) {
		if ck.Name == "VISITOR_INFO1_LIVE" {
			stored = true
		}
	}
	if !stored {
		t.Fatal("test premise broken: the server's Set-Cookie never reached the jar")
	}

	if !c.RotateIdentity(context.Background(), ext.IdentityGeneration(), "") {
		t.Fatal("RotateIdentity = false, want true")
	}
	var consent bool
	for _, ck := range jar.Cookies(ytu) {
		switch ck.Name {
		case "VISITOR_INFO1_LIVE":
			t.Errorf("VISITOR_INFO1_LIVE survived the rotation (value %q)", ck.Value)
		case "CONSENT":
			consent = true
		}
	}
	if !consent {
		t.Error("CONSENT cookie missing after rotation; it must be re-seeded")
	}
}

// TestRotateIdentity_AdoptedUnversionedRefuses pins that an invalidating
// provider whose sessions carry no generation refuses without a provider
// roundtrip: there is nothing to name in a report.
func TestRotateIdentity_AdoptedUnversionedRefuses(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var lastPlayerBody []byte
	p := &stubSessionProvider{unversioned: true}
	c := adoptingClient(t, p, jar, &lastPlayerBody)

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if c.RotateIdentity(context.Background(), ext.IdentityGeneration(), "") {
		t.Fatal("RotateIdentity = true, want false for an unversioned session")
	}
	if len(p.reports) != 0 {
		t.Errorf("invalidations = %d, want 0 (nothing to name in a report)", len(p.reports))
	}
}

// TestRotateIdentity_MidExtractionRotationKeepsStamp pins the stamp source: an
// extraction is tagged with the generation its session resolved under, not the
// counter's value when the extraction finishes. A rotation landing between the
// two must not let the extraction's later 403 retire the innocent replacement.
func TestRotateIdentity_MidExtractionRotationKeepsStamp(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	entered := make(chan struct{})
	var playerHits atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			if playerHits.Add(1) == 1 {
				close(entered)
				<-gate // hold the extraction mid-flight
			}
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	p := &stubSessionProvider{}
	c := New(Config{
		HTTP:            httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar, Transport: rt}}),
		SessionProvider: p,
	})

	type extResult struct {
		ext *Extraction
		err error
	}
	done := make(chan extResult, 1)
	go func() {
		ext, err := c.Extract(context.Background(), "testVideo01")
		done <- extResult{ext, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("extraction never reached /player")
	}

	// Rotate the session the in-flight extraction resolved under.
	if !c.RotateIdentity(context.Background(), c.resetSeq.Load(), "") {
		t.Fatal("mid-extraction rotation refused")
	}
	close(gate)
	res := <-done
	if res.err != nil {
		t.Fatalf("extract: %v", res.err)
	}

	if got := res.ext.IdentityGeneration(); got != 0 {
		t.Errorf("IdentityGeneration = %d, want 0 (the generation the session resolved under)", got)
	}
	// Its 403s must be satisfied by the rotation that already happened, not
	// spend the replacement session.
	if !c.RotateIdentity(context.Background(), res.ext.IdentityGeneration(), "") {
		t.Fatal("stale-generation rotation should report the old identity as gone")
	}
	if len(p.reports) != 1 {
		t.Errorf("invalidations = %d, want 1 (the stale rotation must not retire the replacement)", len(p.reports))
	}
}
