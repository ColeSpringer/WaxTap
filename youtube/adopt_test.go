package youtube

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/potoken"
)

// fakeSessionProvider is a stub potoken.SessionProvider that counts calls and can
// fail on the first call only (to exercise the cache-on-success-only contract).
type fakeSessionProvider struct {
	calls   int
	sess    potoken.Session
	err     error
	errOnce bool
}

func (f *fakeSessionProvider) ProvideSession(context.Context) (potoken.Session, error) {
	f.calls++
	if f.err != nil && (!f.errOnce || f.calls == 1) {
		return potoken.Session{}, f.err
	}
	return f.sess, nil
}

// adoptTestClient builds an ANDROID_VR single-profile client over a cookie-jarred
// transport, with the given adoption config. A jar is present so a non-adopting
// client would bootstrap; adoption must skip that.
func adoptTestClient(t *testing.T, rt http.RoundTripper, jar http.CookieJar, cfg Config) *Client {
	t.Helper()
	cfg.HTTP = httpx.New(httpx.Config{
		HTTPClient:   &http.Client{Jar: jar, Transport: rt},
		MaxRetries:   1,
		MaxRetryWait: 50 * time.Millisecond,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   2 * time.Millisecond,
	})
	cfg.Profiles = []ClientProfile{makeProfile(profileAndroidVR)}
	return New(cfg)
}

// TestExtract_AdoptsSessionVerbatimSkipsBootstrap proves a static adopted session
// is used byte-for-byte in the player request and that the homepage bootstrap is
// skipped even though a cookie jar is present, across repeated extractions.
func TestExtract_AdoptsSessionVerbatimSkipsBootstrap(t *testing.T) {
	const adopted = "CgtADOPTED-_value%3D%3D"
	ok := readFixture(t, "player_ok.json")
	jar, _ := cookiejar.New(nil)

	var playerBody []byte
	var visitorHeader string
	c := adoptTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/" || strings.Contains(r.URL.Host, "youtube.com") && r.Method == http.MethodGet {
			t.Errorf("homepage/bootstrap must not be fetched under adoption: %s", r.URL)
			return fixtureResp(http.StatusNotFound, nil), nil
		}
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			playerBody, _ = io.ReadAll(r.Body)
			visitorHeader = r.Header.Get("X-Goog-Visitor-Id")
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), jar, Config{Session: &potoken.Session{VisitorData: adopted}})

	for i := range 2 {
		if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
			t.Fatalf("extract %d: %v", i, err)
		}
	}
	if visitorHeader != adopted {
		t.Errorf("X-Goog-Visitor-Id = %q, want adopted %q (verbatim)", visitorHeader, adopted)
	}
	if !bytes.Contains(playerBody, []byte(adopted)) {
		t.Errorf("player body did not carry the adopted visitorData verbatim:\n%s", playerBody)
	}
}

// TestExtract_AdoptionFailureAborts verifies that a failed session resolution
// is fatal under adoption: extraction returns the error and never falls back to
// a synthetic visitorData (which would send the wrong content_binding to a token
// minter).
func TestExtract_AdoptionFailureAborts(t *testing.T) {
	provider := &fakeSessionProvider{err: errProvider}
	jar, _ := cookiejar.New(nil)
	c := adoptTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("no request should be made when adoption fails: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), jar, Config{SessionProvider: provider})

	_, err := c.Extract(context.Background(), "testVideo01")
	if err == nil {
		t.Fatal("expected adoption failure to abort Extract")
	}
	if !strings.Contains(err.Error(), "session provider failed") {
		t.Errorf("error = %v, want a session-provider failure", err)
	}
}

// TestExtract_AdoptionProviderResolvedOnce verifies the provider is resolved at
// most once across extractions, and that a transient first failure is retried
// (cache-on-success-only) rather than poisoning the Client.
func TestExtract_AdoptionProviderResolvedOnce(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	jar, _ := cookiejar.New(nil)
	provider := &fakeSessionProvider{
		sess:    potoken.Session{VisitorData: "CgtPROVIDED%3D%3D"},
		err:     errProvider,
		errOnce: true, // fail the first call only
	}
	c := adoptTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), jar, Config{SessionProvider: provider})

	if _, err := c.Extract(context.Background(), "testVideo01"); err == nil {
		t.Fatal("first extract should fail (provider errored)")
	}
	for i := range 3 {
		if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
			t.Fatalf("extract after recovery %d: %v", i, err)
		}
	}
	// One failed call + one successful call; the success is cached for the rest.
	if provider.calls != 2 {
		t.Errorf("ProvideSession calls = %d, want 2 (failure retried, success cached)", provider.calls)
	}
}

// TestExtract_AdoptedCookiesSeededLoginDropped checks that guest cookies from an
// adopted session land in the jar, login cookies are dropped with a warning, and
// the cookies are seeded once.
func TestExtract_AdoptedCookiesSeededLoginDropped(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	jar, _ := cookiejar.New(nil)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	session := &potoken.Session{
		VisitorData: "CgtADOPTED%3D%3D",
		Cookies: []*http.Cookie{
			{Name: "PREF", Value: "guestpref", Domain: ".youtube.com", Path: "/"},
			{Name: "__Secure-3PSID", Value: "SECRET", Domain: ".youtube.com", Path: "/"}, // login cookie
		},
	}
	c := adoptTestClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), jar, Config{Session: session, Logger: logger})

	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatalf("extract: %v", err)
	}

	got := jar.Cookies(&url.URL{Scheme: "https", Host: "www.youtube.com"})
	var hasPREF, hasLogin bool
	for _, ck := range got {
		switch ck.Name {
		case "PREF":
			hasPREF = true
		case "__Secure-3PSID":
			hasLogin = true
		}
	}
	if !hasPREF {
		t.Error("guest cookie PREF was not seeded into the jar")
	}
	if hasLogin {
		t.Error("login cookie __Secure-3PSID must be dropped, not seeded")
	}
	if !strings.Contains(logBuf.String(), "__Secure-3PSID") {
		t.Errorf("expected a warning naming the dropped login cookie; logs:\n%s", logBuf.String())
	}
}

// TestExtract_AdoptedCookiesNoJarErrors verifies that supplying guest cookies
// without a cookie jar is a hard error (no silent drop), while visitorData-only
// adoption still works jarless.
func TestExtract_AdoptedCookiesNoJarErrors(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			return fixtureResp(http.StatusOK, ok), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	jarless := func(cfg Config) *Client {
		cfg.HTTP = httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: rt}})
		cfg.Profiles = []ClientProfile{makeProfile(profileAndroidVR)}
		return New(cfg)
	}

	// Cookies without a jar: error.
	withCookies := jarless(Config{Session: &potoken.Session{
		VisitorData: "CgtADOPTED%3D%3D",
		Cookies:     []*http.Cookie{{Name: "PREF", Value: "x", Domain: ".youtube.com", Path: "/"}},
	}})
	if _, err := withCookies.Extract(context.Background(), "testVideo01"); err == nil || !strings.Contains(err.Error(), "cookie jar") {
		t.Fatalf("expected a no-jar cookie error, got %v", err)
	}

	// visitorData-only adoption works jarless.
	vdOnly := jarless(Config{Session: &potoken.Session{VisitorData: "CgtADOPTED%3D%3D"}})
	if _, err := vdOnly.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatalf("visitorData-only adoption should work jarless: %v", err)
	}
}

// TestExtract_StaticEmptyVisitorDataAborts verifies that a static Session with
// an empty VisitorData is not silently adopted (which would break GVS
// content_binding) but aborts extraction with a clear error, matching the
// provider path.
func TestExtract_StaticEmptyVisitorDataAborts(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("no request expected when adoption has an empty visitorData: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	c := New(Config{
		HTTP:     httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: rt}}),
		Profiles: []ClientProfile{makeProfile(profileAndroidVR)},
		Session:  &potoken.Session{VisitorData: ""},
	})
	if _, err := c.Extract(context.Background(), "testVideo01"); err == nil || !strings.Contains(err.Error(), "empty visitorData") {
		t.Fatalf("err = %v, want an empty-visitorData abort", err)
	}
}

// TestExtract_ProviderCookiesNoJarCachesError verifies that a provider that
// returns cookies while the client has no jar fails once (a permanent config
// error) and is not re-invoked on every Extract; the cache-on-success rule must
// not retry a permanent misconfiguration.
func TestExtract_ProviderCookiesNoJarCachesError(t *testing.T) {
	provider := &fakeSessionProvider{sess: potoken.Session{
		VisitorData: "CgtADOPTED%3D%3D",
		Cookies:     []*http.Cookie{{Name: "PREF", Value: "x", Domain: ".youtube.com", Path: "/"}},
	}}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusNotFound, nil), nil
	})
	c := New(Config{
		HTTP:            httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: rt}}), // no jar
		Profiles:        []ClientProfile{makeProfile(profileAndroidVR)},
		SessionProvider: provider,
	})
	for i := range 3 {
		if _, err := c.Extract(context.Background(), "testVideo01"); err == nil || !strings.Contains(err.Error(), "cookie jar") {
			t.Fatalf("extract %d: err = %v, want a cookie-jar error", i, err)
		}
	}
	if provider.calls != 1 {
		t.Errorf("ProvideSession calls = %d, want 1 (permanent error cached, not re-fetched)", provider.calls)
	}
}

// TestResolveToken_AdoptedContentBinding proves the GVS token's content_binding
// (the provider's Request.VisitorData) is the adopted visitorData, verbatim: the
// coherence the whole adoption feature exists to provide.
func TestResolveToken_AdoptedContentBinding(t *testing.T) {
	const adopted = "CgtADOPTED-_value%3D%3D"
	fp := &fakeProvider{resp: potoken.Response{Token: "GVS-TOK"}}
	c := New(Config{POTokenProvider: fp})

	sess := newSession("US")
	sess.adoptVisitorData(adopted)
	ext := &Extraction{video: &Video{ID: "vid123"}, profile: makeProfile(profileWeb), session: sess}

	if _, err := c.resolveToken(context.Background(), ext, nil); err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if fp.gotReq.Scope != potoken.ScopeGVS {
		t.Errorf("scope = %v, want GVS", fp.gotReq.Scope)
	}
	if fp.gotReq.VisitorData != adopted {
		t.Errorf("GVS content_binding visitorData = %q, want adopted %q verbatim", fp.gotReq.VisitorData, adopted)
	}
}

func TestSeedExternalCookies(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	seedExternalCookies(jar, []*http.Cookie{
		{Name: "A", Value: "1", Domain: ".youtube.com", Path: "/"},
		{Name: "B", Value: "2", Domain: "www.youtube.com", Path: "/"},
		{Name: "C", Value: "3", Domain: "googlevideo.com", Path: "/"},
		{Name: "D", Value: "4", Domain: "", Path: "/"}, // domain-less: skipped
	})
	yt := jar.Cookies(&url.URL{Scheme: "https", Host: "www.youtube.com"})
	if len(yt) != 2 { // A (.youtube.com) and B (www.youtube.com)
		t.Errorf("youtube.com cookies = %d, want 2 (got %v)", len(yt), yt)
	}
	gv := jar.Cookies(&url.URL{Scheme: "https", Host: "googlevideo.com"})
	if len(gv) != 1 {
		t.Errorf("googlevideo.com cookies = %d, want 1", len(gv))
	}
	for _, ck := range append(yt, gv...) {
		if ck.Name == "D" {
			t.Error("domain-less cookie D should have been skipped")
		}
	}
}

func TestSeedExternalCookies_NilJarNoPanic(t *testing.T) {
	// Must not panic with a nil jar (visitorData-only adoption needs no jar).
	seedExternalCookies(nil, []*http.Cookie{{Name: "A", Value: "1", Domain: ".youtube.com"}})
	seedExternalCookies(nil, nil)
}

func TestFilterLoginCookies(t *testing.T) {
	// Guest cookies (including WaxSeal's /session set) are kept; the full Google
	// auth-cookie family is dropped, including the __Secure-1P/3P and SIDCC/APISID
	// variants a flat denylist missed.
	keep := []string{"PREF", "VISITOR_INFO1_LIVE", "YSC", "GPS", "VISITOR_PRIVACY_METADATA",
		"__Secure-YNID", "__Secure-ROLLOUT_TOKEN", "CONSENT", "SOCS"}
	drop := []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "LOGIN_INFO",
		"__Secure-1PSID", "__Secure-3PSID", "__Secure-1PSIDTS", "__Secure-3PSIDTS",
		"__Secure-1PAPISID", "__Secure-3PAPISID", "SIDCC", "__Secure-1PSIDCC", "__Secure-3PSIDCC"}

	var in []*http.Cookie
	for _, n := range append(append([]string{}, keep...), drop...) {
		in = append(in, &http.Cookie{Name: n})
	}
	safe, dropped := filterLoginCookies(in)
	if len(safe) != len(keep) {
		t.Errorf("kept %d cookies, want %d: %v", len(safe), len(keep), safe)
	}
	if len(dropped) != len(drop) {
		t.Errorf("dropped %d cookies, want %d: %v", len(dropped), len(drop), dropped)
	}
	for _, ck := range safe {
		if isLoginCookie(ck.Name) {
			t.Errorf("login cookie %q leaked into the guest-safe set", ck.Name)
		}
	}
}

// errProvider is a sentinel used by the adoption failure tests.
var errProvider = errTest("provider boom")

type errTest string

func (e errTest) Error() string { return string(e) }
