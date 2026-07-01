package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
)

// levelCaptureHandler records levels for matching log messages.
type levelCaptureHandler struct {
	mu     sync.Mutex
	match  string
	levels []slog.Level
}

func (h *levelCaptureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, h.match) {
		h.mu.Lock()
		h.levels = append(h.levels, r.Level)
		h.mu.Unlock()
	}
	return nil
}
func (h *levelCaptureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelCaptureHandler) WithGroup(string) slog.Handler      { return h }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureResp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

func newTestClient(rt http.RoundTripper) *Client {
	return New(Config{HTTP: httpx.New(httpx.Config{
		HTTPClient:   &http.Client{Transport: rt},
		MaxRetries:   1,
		MaxRetryWait: 50 * time.Millisecond,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   2 * time.Millisecond,
	})})
}

// newTestClientWith builds a test client with an explicit profile chain and
// PO-token provider, reusing the fast-retry transport config of newTestClient.
func newTestClientWith(rt http.RoundTripper, profiles []ClientProfile, provider potoken.Provider) *Client {
	return New(Config{
		HTTP: httpx.New(httpx.Config{
			HTTPClient:   &http.Client{Transport: rt},
			MaxRetries:   1,
			MaxRetryWait: 50 * time.Millisecond,
			BaseBackoff:  time.Millisecond,
			MaxBackoff:   2 * time.Millisecond,
		}),
		Profiles:        profiles,
		POTokenProvider: provider,
	})
}

// TestExtract_InjectsPlayerPOToken checks that Extract requests ScopePlayer before
// the /player POST and sends the returned token in the body.
func TestExtract_InjectsPlayerPOToken(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	fp := &fakeProvider{resp: potoken.Response{Token: "PLAYER-TOK"}}
	var playerBody []byte
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if resp, ok := discoveryResp(r); ok {
			return resp, nil // WEB loads base.js before the player request
		}
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			playerBody, _ = io.ReadAll(r.Body)
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), []ClientProfile{makeProfile(profileWeb)}, fp)

	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if fp.calls != 1 {
		t.Errorf("provider calls = %d, want 1", fp.calls)
	}
	if fp.gotReq.Scope != potoken.ScopePlayer {
		t.Errorf("provider scope = %v, want ScopePlayer", fp.gotReq.Scope)
	}
	if !strings.Contains(string(playerBody), `"serviceIntegrityDimensions":{"poToken":"PLAYER-TOK"}`) {
		t.Errorf("player request body missing player token: %s", playerBody)
	}
}

// TestExtract_RejectsHeaderOnlyPlayerToken covers a provider response that is
// useful for stream resolution but not for /player, where the body needs
// Response.Token.
func TestExtract_RejectsHeaderOnlyPlayerToken(t *testing.T) {
	fp := &fakeProvider{resp: potoken.Response{Headers: http.Header{"X-Foo": {"bar"}}}} // no Token
	var playerCalls int
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			playerCalls++
			return fixtureResp(http.StatusOK, nil), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil // fail the watch-page fallback
	}), []ClientProfile{makeProfile(profileWeb)}, fp)

	_, err := c.Extract(context.Background(), "testVideo01")
	if err == nil {
		t.Fatal("err = nil, want a failure when the only client has no usable player token")
	}
	if playerCalls != 0 {
		t.Errorf("/player POST count = %d, want 0 (an empty player token must not be sent)", playerCalls)
	}
}

func TestExtract_GenericBeatsNeedsPOToken(t *testing.T) {
	c := newTestClientWith(roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusNotFound, nil), nil
	}), []ClientProfile{makeProfile(profileWeb)}, nil)

	_, err := c.Extract(context.Background(), "testVideo01")
	if err == nil {
		t.Fatal("err = nil, want a failure")
	}
	if errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Errorf("err = %v, want the generic HTTP failure to outrank ErrNeedsPOToken", err)
	}
	if _, ok := errors.AsType[*waxerr.HTTPStatusError](err); !ok {
		t.Errorf("err = %v (%T), want a generic *waxerr.HTTPStatusError", err, err)
	}
}

func TestExtract_UnavailableBeatsNeedsPOToken(t *testing.T) {
	unavailable := readFixture(t, "player_unavailable.json") // status ERROR
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, unavailable), nil // ANDROID_VR: unavailable
		default:
			return fixtureResp(http.StatusNotFound, nil), nil // watch page fails generically
		}
	}), []ClientProfile{makeProfile(profileAndroidVR), makeProfile(profileWeb)}, nil)

	_, err := c.Extract(context.Background(), "testVideo01")
	if !errors.Is(err, waxerr.ErrVideoUnavailable) {
		t.Fatalf("err = %v, want ErrVideoUnavailable (must not be masked by WEB's needs-po-token)", err)
	}
	if errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Errorf("err = %v, must not be classified as needs-po-token", err)
	}
}

func TestExtractExcluding_SkipsByStableIDDuplicateNames(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	dup := profileAndroidVR // token-free, so no provider is needed
	dup.Name = "DUP"
	profiles := []ClientProfile{makeProfile(dup), makeProfile(dup)}
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			return fixtureResp(http.StatusOK, ok), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	}), profiles, nil)

	ext, err := c.ExtractExcluding(context.Background(), "testVideo01", map[AttemptID]bool{profileAttempt(0): true})
	if err != nil {
		t.Fatal(err)
	}
	if ext.Attempt() != profileAttempt(1) {
		t.Errorf("attempt = %q, want profile:1 (skip by index, not by the shared name)", ext.Attempt())
	}
}

func TestExtractExcluding_WatchPageIsIndependentAttempt(t *testing.T) {
	login := readFixture(t, "player_login_required.json")
	html := readFixture(t, "watch_page.html")
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			return fixtureResp(http.StatusOK, watchPageWithBaseJS(html)), nil
		case strings.HasSuffix(r.URL.Path, "/base.js"):
			return fixtureResp(http.StatusOK, []byte(stsBaseJS)), nil
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, login), nil // the single profile is login-gated
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	})

	// Skipping the watch-page attempt leaves only the failing profile, so its
	// login-required verdict surfaces instead of the watch page rescuing it.
	c := newTestClientWith(transport, []ClientProfile{makeProfile(profileAndroidVR)}, nil)
	if _, err := c.ExtractExcluding(context.Background(), "testVideo01", map[AttemptID]bool{AttemptWatchPage: true}); !errors.Is(err, waxerr.ErrLoginRequired) {
		t.Fatalf("err = %v, want ErrLoginRequired (watch page skipped)", err)
	}

	// Skipping the profile leaves the watch page, which rescues extraction.
	c2 := newTestClientWith(transport, []ClientProfile{makeProfile(profileAndroidVR)}, nil)
	ext, err := c2.ExtractExcluding(context.Background(), "testVideo01", map[AttemptID]bool{profileAttempt(0): true})
	if err != nil {
		t.Fatalf("skip profile, watch page should rescue: %v", err)
	}
	if ext.Attempt() != AttemptWatchPage {
		t.Errorf("attempt = %q, want watch-page", ext.Attempt())
	}
	// The extraction records the forced client replaced by WEB.
	if got := ext.SubstitutedFrom(); got != "ANDROID_VR" {
		t.Errorf("SubstitutedFrom = %q, want ANDROID_VR", got)
	}
}

func TestExtractExcluding_DefaultChainWatchPageIsNotSubstitution(t *testing.T) {
	// A normal fallback through the default chain is not a client substitution.
	login := readFixture(t, "player_login_required.json")
	html := readFixture(t, "watch_page.html")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			return fixtureResp(http.StatusOK, watchPageWithBaseJS(html)), nil
		case strings.HasSuffix(r.URL.Path, "/base.js"):
			return fixtureResp(http.StatusOK, []byte(stsBaseJS)), nil
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, login), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("default chain watch-page rescue: %v", err)
	}
	if ext.Attempt() != AttemptWatchPage {
		t.Fatalf("attempt = %q, want watch-page", ext.Attempt())
	}
	if got := ext.SubstitutedFrom(); got != "" {
		t.Errorf("SubstitutedFrom = %q, want empty for the default chain", got)
	}
}

func TestExtractExcluding_AllSkippedReturnsChainExhausted(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			return fixtureResp(http.StatusOK, ok), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	}), []ClientProfile{makeProfile(profileAndroidVR)}, nil)

	skip := map[AttemptID]bool{profileAttempt(0): true, AttemptWatchPage: true}
	_, err := c.ExtractExcluding(context.Background(), "testVideo01", skip)
	if !errors.Is(err, waxerr.ErrChainExhausted) {
		t.Fatalf("err = %v, want ErrChainExhausted", err)
	}
	if errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Errorf("chain-exhausted must not be classified as an extraction failure: %v", err)
	}
}

func TestExtractAttempt_PinsToOneProfile(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var clients []string // X-Youtube-Client-Name on each /player POST
	profiles := []ClientProfile{makeProfile(profileAndroidVR), makeProfile(profileIOS)}
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			clients = append(clients, r.Header.Get("X-Youtube-Client-Name"))
			return fixtureResp(http.StatusOK, ok), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	}), profiles, nil)

	ext, err := c.ExtractAttempt(context.Background(), "testVideo01", profileAttempt(1)) // IOS
	if err != nil {
		t.Fatal(err)
	}
	if ext.Attempt() != profileAttempt(1) {
		t.Errorf("attempt = %q, want profile:1", ext.Attempt())
	}
	// IOS is InnerTube client 5; ANDROID_VR (28) must not have been contacted.
	if len(clients) != 1 || clients[0] != "5" {
		t.Errorf("client POSTs = %v, want exactly [\"5\"] (pinned to IOS, chain not re-run)", clients)
	}
}

// stsBaseJS is the smallest valid player program needed for signature timestamp
// lookup. The cipher transform allows the resolver to cache the program.
const stsBaseJS = `var cfg={signatureTimestamp:19834};` +
	`var Xq={sp:function(a,b){a.splice(0,b)}};` +
	`function dcr(a){a=a.split("");Xq.sp(a,1);return a.join("")}` +
	`;s&&(s=dcr(decodeURIComponent(s)));`

// discoveryResp serves the watch/embed page and base.js used for signature
// timestamp lookup, for tests where only player discovery (no streamingData
// scrape) touches /watch. Watch-first discovery and the scrape fallback now both
// carry bpctr (consent bypass), so they share the /watch URL; a test that needs
// both serves a combined watch page instead (see watchPageWithBaseJS). It returns
// false for requests the caller should handle.
func discoveryResp(r *http.Request) (*http.Response, bool) {
	switch {
	case r.URL.Path == "/watch", strings.HasPrefix(r.URL.Path, "/embed/"):
		return fixtureResp(http.StatusOK, []byte(`<script src="/s/player/abcd1234ef/player_ias.vflset/en_US/base.js"></script>`)), true
	case strings.HasSuffix(r.URL.Path, "/base.js"):
		return fixtureResp(http.StatusOK, []byte(stsBaseJS)), true
	}
	return nil, false
}

// watchPageWithBaseJS returns the watch-page HTML with a base.js <script> tag
// appended. The real watch page carries both the player response (for the scrape
// fallback) and the base.js URL (for signature-timestamp discovery); since
// discovery gained bpctr, both consumers fetch the same /watch URL, so one body
// serves both.
func watchPageWithBaseJS(html []byte) []byte {
	tag := []byte(`<script src="/s/player/abcd1234ef/player_ias.vflset/en_US/base.js"></script>`)
	return append(append(append([]byte{}, html...), '\n'), tag...)
}

func TestExtract_WEBSendsSignatureTimestamp(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var playerBody []byte
	var watchVideoID string
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			watchVideoID = r.URL.Query().Get("v")
			return fixtureResp(http.StatusOK, []byte(`<script src="/s/player/abcd1234ef/player_ias.vflset/en_US/base.js"></script>`)), nil
		case strings.HasSuffix(r.URL.Path, "/base.js"):
			return fixtureResp(http.StatusOK, []byte(stsBaseJS)), nil
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			playerBody, _ = io.ReadAll(r.Body)
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), []ClientProfile{makeProfile(profileWeb)}, &fakeProvider{resp: potoken.Response{Token: "TOK"}})

	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(playerBody), `"signatureTimestamp":19834`) {
		t.Errorf("WEB /player body missing signatureTimestamp: %s", playerBody)
	}
	// Discovery must use the requested video; it lands in the watch page ?v=.
	if watchVideoID != "testVideo01" {
		t.Errorf("signature timestamp discovery ?v= = %q, want testVideo01", watchVideoID)
	}
}

func TestExtract_AndroidVROmitsSignatureTimestamp(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var playerBody []byte
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			playerBody, _ = io.ReadAll(r.Body)
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected signature timestamp lookup for ANDROID_VR: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(playerBody), "signatureTimestamp") {
		t.Errorf("ANDROID_VR body must omit signatureTimestamp: %s", playerBody)
	}
}

func TestExtract_FirstClientSucceeds(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var playerCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			playerCalls++
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Video().Title != "Test Song" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	if playerCalls != 1 {
		t.Errorf("playerCalls = %d, want 1 (first client wins)", playerCalls)
	}

	// The resolver input is kept by index because itag is not unique across
	// multi-track audio.
	if rf, ok := ext.rawFormatByIndex(0); !ok || rf.Itag != 251 || rf.URL == "" {
		t.Errorf("index 0 raw = %+v, ok=%v; want itag 251 with a direct URL", rf, ok)
	}
	if rf, ok := ext.rawFormatByIndex(1); !ok || rf.Itag != 140 || rf.SignatureCipher == "" {
		t.Errorf("index 1 raw = %+v, ok=%v; want itag 140 with a signatureCipher", rf, ok)
	}
}

func TestExtract_FallsBackAcrossClients(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	login := readFixture(t, "player_login_required.json")
	var names []string
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if resp, ok := discoveryResp(r); ok {
			return resp, nil // WEB-family clients load base.js before the player request
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/player") {
			t.Errorf("unexpected request: %s", r.URL)
			return fixtureResp(http.StatusNotFound, nil), nil
		}
		name := r.Header.Get("X-Youtube-Client-Name")
		names = append(names, name)
		if name == "28" { // ANDROID_VR is age-gated; IOS delivers
			return fixtureResp(http.StatusOK, login), nil
		}
		return fixtureResp(http.StatusOK, ok), nil
	}))

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Video().Title != "Test Song" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	// Without a PO token, the WEB-family clients (WEB=1, WEB_EMBEDDED=56)
	// short-circuit at the token fetch and make no /player call, so only
	// ANDROID_VR and IOS do.
	// (TestExtract_PlayabilityErrorTriesAllClients covers the token-present chain.)
	if want := []string{"28", "5"}; !slicesEqual(names, want) {
		t.Errorf("client order = %v, want %v", names, want)
	}
}

func TestExtract_PlayabilityErrorTriesAllClients(t *testing.T) {
	un := readFixture(t, "player_unavailable.json") // status ERROR
	var playerCalls int
	// The WEB profile needs a player-scope token before its /player POST, so
	// configure a provider. Discovery serves a real base.js so the sts client
	// resolves a non-zero timestamp: the ERROR is genuine across every client,
	// not a missing-sts artifact, so it stays classified as ErrVideoUnavailable.
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if resp, ok := discoveryResp(r); ok {
			return resp, nil
		}
		if strings.Contains(r.URL.Path, "/player") {
			playerCalls++
		}
		return fixtureResp(http.StatusOK, un), nil
	}), nil, &fakeProvider{resp: potoken.Response{Token: "TOK"}})

	_, err := c.Extract(context.Background(), "testVideo01")
	if !errors.Is(err, waxerr.ErrVideoUnavailable) {
		t.Fatalf("err = %v, want ErrVideoUnavailable", err)
	}
	// A generic ERROR is no longer terminal: every client in the chain is tried
	// before extraction gives up.
	if want := len(DefaultProfiles()); playerCalls != want {
		t.Errorf("player calls = %d, want %d (all clients tried past ERROR)", playerCalls, want)
	}
}

// TestExtract_MissingTimestampAttributed verifies that when a signature-timestamp
// profile resolves sts=0 and YouTube answers UNPLAYABLE, the terminal error names
// the missing timestamp (ErrExtractionFailed) rather than classifying the video as
// ErrVideoUnavailable.
func TestExtract_MissingTimestampAttributed(t *testing.T) {
	unplayable := readFixture(t, "player_unplayable.json") // status UNPLAYABLE
	// A valid player program with no signatureTimestamp literal: discovery
	// succeeds but the lookup resolves sts=0.
	const noSTS = `var Xq={sp:function(a,b){a.splice(0,b)}};` +
		`function dcr(a){a=a.split("");Xq.sp(a,1);return a.join("")}` +
		`;s&&(s=dcr(decodeURIComponent(s)));`
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			return fixtureResp(http.StatusOK, []byte(`<script src="/s/player/abcd1234ef/player_ias.vflset/en_US/base.js"></script>`)), nil
		case strings.HasSuffix(r.URL.Path, "/base.js"):
			return fixtureResp(http.StatusOK, []byte(noSTS)), nil
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, unplayable), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), []ClientProfile{makeProfile(profileWeb)}, &fakeProvider{resp: potoken.Response{Token: "TOK"}})

	_, err := c.Extract(context.Background(), "testVideo01")
	if !errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Fatalf("err = %v, want ErrExtractionFailed (missing sts attributed)", err)
	}
	if errors.Is(err, waxerr.ErrVideoUnavailable) {
		t.Errorf("err = %v, must not classify as ErrVideoUnavailable (the masking is removed)", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "signature timestamp") || !strings.Contains(msg, "sts=0") {
		t.Errorf("err = %q, want it to name the WEB signature timestamp cause", err)
	}
	if ee, ok := errors.AsType[*waxerr.ExtractionError](err); !ok || ee.Stage != "signature-timestamp" {
		t.Errorf("err = %#v, want *waxerr.ExtractionError at stage %q", err, "signature-timestamp")
	}
}

// TestExtract_WebEmbeddedEmbedRestrictionNotMaskedBySTS verifies that an embed
// restriction outranks a signature-timestamp failure from the same attempt.
func TestExtract_WebEmbeddedEmbedRestrictionNotMaskedBySTS(t *testing.T) {
	errResp := readFixture(t, "player_unavailable.json") // status ERROR
	const noSTS = `var Xq={sp:function(a,b){a.splice(0,b)}};` +
		`function dcr(a){a=a.split("");Xq.sp(a,1);return a.join("")}` +
		`;s&&(s=dcr(decodeURIComponent(s)));`
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			return fixtureResp(http.StatusOK, []byte(`<script src="/s/player/abcd1234ef/player_ias.vflset/en_US/base.js"></script>`)), nil
		case strings.HasSuffix(r.URL.Path, "/base.js"):
			return fixtureResp(http.StatusOK, []byte(noSTS)), nil
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, errResp), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}), []ClientProfile{makeProfile(profileWebEmbedded)}, &fakeProvider{resp: potoken.Response{Token: "TOK"}})

	_, err := c.Extract(context.Background(), "testVideo01")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The embed restriction must not be masked by the signature-timestamp failure.
	pe, ok := errors.AsType[*waxerr.PlayabilityError](err)
	if !ok || !pe.Embed {
		t.Errorf("err = %#v, want a *PlayabilityError with Embed=true", err)
	}
	// Do not add an embeddability claim to YouTube's reason.
	if strings.Contains(err.Error(), "embeddable") {
		t.Errorf("err = %q, should not fold in a 'may not be embeddable' claim", err)
	}
	if errors.Is(err, waxerr.ErrExtractionFailed) {
		t.Errorf("err = %v, must not be reclassified to ErrExtractionFailed (sts masking)", err)
	}
}

// TestExtract_SubstitutionLogsAtDebug verifies that a forced-client fallback to
// the watch page logs at Debug, not Warn. The public warning path reports the
// substitution, so the CLI should not print an extra slog line.
func TestExtract_SubstitutionLogsAtDebug(t *testing.T) {
	html := readFixture(t, "watch_page.html")
	errResp := readFixture(t, "player_unavailable.json") // status ERROR
	h := &levelCaptureHandler{match: "forced client failed; trying watch-page WEB fallback"}
	c := New(Config{
		Logger:   slog.New(h),
		Profiles: []ClientProfile{makeProfile(profileAndroidVR)}, // forced, non-playlist
		HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case r.URL.Path == "/watch":
				return fixtureResp(http.StatusOK, watchPageWithBaseJS(html)), nil
			case strings.HasSuffix(r.URL.Path, "/base.js"):
				return fixtureResp(http.StatusOK, []byte(stsBaseJS)), nil
			case strings.HasSuffix(r.URL.Path, "/v1/player"):
				return fixtureResp(http.StatusOK, errResp), nil // forced client fails -> watch-page fallback
			}
			return fixtureResp(http.StatusNotFound, nil), nil
		})}}),
	})
	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if len(h.levels) != 1 || h.levels[0] != slog.LevelDebug {
		t.Errorf("levels = %v, want exactly one Debug", h.levels)
	}
}

func TestExtract_WatchPageFallback(t *testing.T) {
	login := readFixture(t, "player_login_required.json")
	html := readFixture(t, "watch_page.html")
	var watchCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			watchCalls++
			// Discovery (base.js) and the scrape fallback (player response) both
			// fetch /watch now, so one combined body serves both.
			return fixtureResp(http.StatusOK, watchPageWithBaseJS(html)), nil
		case strings.HasSuffix(r.URL.Path, "/base.js"):
			return fixtureResp(http.StatusOK, []byte(stsBaseJS)), nil
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, login), nil // every client age-gated
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Video().Title != "From Watch Page" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	// Without a PO token the WEB-family clients short-circuit at the token fetch,
	// before signature-timestamp discovery, so the only /watch fetch is the
	// streamingData scrape fallback (which carries base.js inline).
	if watchCalls != 1 {
		t.Errorf("watchCalls = %d, want 1 (scrape fallback only)", watchCalls)
	}
}

func TestIsWebEmbedded(t *testing.T) {
	if !isWebEmbedded(profileWebEmbedded) {
		t.Error("profileWebEmbedded should be recognized as web_embedded")
	}
	if isWebEmbedded(profileWeb) || isWebEmbedded(profileAndroidVR) {
		t.Error("WEB and ANDROID_VR are not web_embedded")
	}
}

func TestAnnotateEmbedError(t *testing.T) {
	// The marker must preserve the original reason and classification.
	base := &waxerr.PlayabilityError{Status: "ERROR", Reason: "Video unavailable", Sentinel: waxerr.ErrVideoUnavailable}
	got := annotateEmbedError(base)
	pe, ok := errors.AsType[*waxerr.PlayabilityError](got)
	if !ok {
		t.Fatalf("annotateEmbedError returned %T, want *PlayabilityError", got)
	}
	if !pe.Embed {
		t.Error("Embed = false, want true")
	}
	if pe.Reason != "Video unavailable" {
		t.Errorf("reason = %q, want YouTube's verbatim reason preserved", pe.Reason)
	}
	if pe.Status != "ERROR" || !errors.Is(got, waxerr.ErrVideoUnavailable) {
		t.Errorf("status/sentinel changed: status=%q unavailable=%v", pe.Status, errors.Is(got, waxerr.ErrVideoUnavailable))
	}
	unpl := &waxerr.PlayabilityError{Status: "UNPLAYABLE", Reason: "nope", Sentinel: waxerr.ErrVideoUnavailable}
	if annotateEmbedError(unpl) != error(unpl) {
		t.Error("a non-ERROR playability failure should be returned unchanged")
	}
}

func TestForcedNonWebSingle(t *testing.T) {
	single := func(p ClientProfile) *Client {
		return newTestClientWith(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return fixtureResp(http.StatusNotFound, nil), nil
		}), []ClientProfile{makeProfile(p)}, nil)
	}
	if !single(profileAndroidVR).forcedNonWebSingle() {
		t.Error("a single forced ANDROID_VR chain should be a non-WEB substitution candidate")
	}
	if single(profileWeb).forcedNonWebSingle() {
		t.Error("a single forced WEB chain is not a substitution (watch page is WEB)")
	}
	def := newTestClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	if def.forcedNonWebSingle() {
		t.Error("the default chain must not be treated as a forced single client")
	}
}

func TestExtract_ForcedClientSubstitutionNamesWEB(t *testing.T) {
	// The terminal error names both attempts and preserves the underlying verdict.
	login := readFixture(t, "player_login_required.json")
	c := newTestClientWith(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/watch":
			return fixtureResp(http.StatusNotFound, nil), nil // watch-page fallback fails
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, login), nil
		}
		return fixtureResp(http.StatusNotFound, nil), nil
	}), []ClientProfile{makeProfile(profileAndroidVR)}, nil)

	_, err := c.Extract(context.Background(), "testVideo01")
	if err == nil {
		t.Fatal("expected an error when the forced client and watch-page both fail")
	}
	if !strings.Contains(err.Error(), "ANDROID_VR") {
		t.Errorf("error %q should name the forced client ANDROID_VR", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "watch-page") {
		t.Errorf("error %q should name the WEB watch-page fallback", err)
	}
	if !errors.Is(err, waxerr.ErrLoginRequired) {
		t.Errorf("err = %v, want it to still match ErrLoginRequired through the wrap", err)
	}
}

func TestExtract_RateLimitShortCircuits(t *testing.T) {
	var calls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		resp := fixtureResp(http.StatusTooManyRequests, nil)
		resp.Header.Set("Retry-After", "3600")
		return resp, nil
	}))

	_, err := c.Extract(context.Background(), "testVideo01")
	if !errors.Is(err, waxerr.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (rate limit short-circuits chain + fallback)", calls)
	}
}

func TestExtract_DefaultLocale(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var body []byte
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		return fixtureResp(http.StatusOK, ok), nil
	}))
	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"hl":"en"`)) || !bytes.Contains(body, []byte(`"gl":"US"`)) {
		t.Errorf("default locale not en/US in request: %s", body)
	}
}

func TestExtract_ConfiguredLocale(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var body []byte
	var acceptLang string
	c := New(Config{
		HL: "de",
		GL: "DE",
		HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ = io.ReadAll(r.Body)
			acceptLang = r.Header.Get("Accept-Language")
			return fixtureResp(http.StatusOK, ok), nil
		})}}),
	})
	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"hl":"de"`)) || !bytes.Contains(body, []byte(`"gl":"DE"`)) {
		t.Errorf("configured locale not in request body: %s", body)
	}
	if !strings.HasPrefix(acceptLang, "de") {
		t.Errorf("Accept-Language = %q, want it to lead with de", acceptLang)
	}
}

func TestEnumerate(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	cont := readFixture(t, "playlist_continuation.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
			return fixtureResp(http.StatusOK, cont), nil
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Title != "My Playlist" {
		t.Errorf("title = %q", pl.Title)
	}
	if pl.Author != "Owner Name" {
		t.Errorf("author = %q", pl.Author)
	}
	if len(pl.Errors) != 0 {
		t.Errorf("errors = %v", pl.Errors)
	}

	wantIDs := []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc"}
	if len(pl.Entries) != len(wantIDs) {
		t.Fatalf("entries = %d, want %d", len(pl.Entries), len(wantIDs))
	}
	for i, e := range pl.Entries {
		if e.VideoID != wantIDs[i] {
			t.Errorf("entry[%d].VideoID = %q, want %q", i, e.VideoID, wantIDs[i])
		}
		if e.Index != i {
			t.Errorf("entry[%d].Index = %d, want %d", i, e.Index, i)
		}
	}
	if pl.Entries[0].Title != "Song A" || pl.Entries[0].Author != "Artist A" {
		t.Errorf("entry0 = %+v", pl.Entries[0])
	}
	if pl.Entries[2].Duration != 300*time.Second {
		t.Errorf("entry2 duration = %v, want 5m", pl.Entries[2].Duration)
	}
}

func TestEnumerate_NegativeMaxItemsIsInvalidConfig(t *testing.T) {
	c := newTestClient(roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		t.Fatal("Enumerate must reject a negative cap before any request")
		return nil, nil
	}))
	_, err := c.Enumerate(context.Background(), "PLtest", -1, nil)
	if !errors.Is(err, waxerr.ErrInvalidConfig) {
		t.Errorf("negative maxItems err = %v, want ErrInvalidConfig", err)
	}
}

func TestEnumerate_BadRequestIsInvalidPlaylistID(t *testing.T) {
	// A 400 from the browse endpoint means YouTube rejected the playlist ID; it
	// must surface as ErrInvalidPlaylistID, not a raw HTTP status.
	c := newTestClient(roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusBadRequest, []byte(`{"error":{"code":400,"message":"Invalid value"}}`)), nil
	}))
	_, err := c.Enumerate(context.Background(), "PLbroken", 0, nil)
	if !errors.Is(err, waxerr.ErrInvalidPlaylistID) {
		t.Fatalf("err = %v, want ErrInvalidPlaylistID", err)
	}
}

func TestEnumerate_NotFoundIsPlaylistUnavailable(t *testing.T) {
	// A 404 from the browse endpoint means the playlist is deleted or nonexistent.
	// It should surface as ErrPlaylistUnavailable, not as a raw HTTP status that
	// leaks the internal browse URL.
	c := newTestClient(roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusNotFound, []byte(`{"error":{"code":404,"message":"Not Found"}}`)), nil
	}))
	_, err := c.Enumerate(context.Background(), "PLmissing", 0, nil)
	if !errors.Is(err, waxerr.ErrPlaylistUnavailable) {
		t.Fatalf("err = %v, want ErrPlaylistUnavailable", err)
	}
}

func TestEnumerate_ForbiddenIsNotPlaylistUnavailable(t *testing.T) {
	// A browse 403 is an anti-bot or attestation block, not a permanently
	// unavailable playlist. Private playlists return HTTP 200 with an in-body
	// alert. Keep 403 outside ErrPlaylistUnavailable so callers do not treat a
	// reachable playlist as permanently gone.
	c := newTestClient(roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"Forbidden"}}`)), nil
	}))
	_, err := c.Enumerate(context.Background(), "PLblocked", 0, nil)
	if errors.Is(err, waxerr.ErrPlaylistUnavailable) {
		t.Fatalf("err = %v, want a 403 NOT mapped to ErrPlaylistUnavailable", err)
	}
	hse, ok := errors.AsType[*waxerr.HTTPStatusError](err)
	if !ok || hse.StatusCode != http.StatusForbidden {
		t.Fatalf("err = %v, want the underlying HTTP 403 preserved", err)
	}
}

func TestEnumerate_MaxItemsAtPageBoundary(t *testing.T) {
	// maxItems equals the page's entry count: a clean page boundary, so the
	// next-page token is a valid resume point.
	browse := readFixture(t, "playlist_browse.json") // 2 entries + token
	var calls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(pl.Entries))
	}
	if pl.Continuation != "CONT_TOKEN_1" {
		t.Errorf("continuation = %q, want CONT_TOKEN_1 (page boundary is resumable)", pl.Continuation)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (stopped at limit, no continuation fetch)", calls)
	}
}

func TestEnumerate_MaxItemsMidPageNoResume(t *testing.T) {
	// maxItems falls in the middle of the first page. There is no page-granular
	// resume point for the unreturned entries.
	browse := readFixture(t, "playlist_browse.json") // 2 entries + token
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(pl.Entries))
	}
	if pl.Continuation != "" {
		t.Errorf("continuation = %q, want empty (mid-page cutoff must not skip entries)", pl.Continuation)
	}
}

func TestEnumerate_LegacyContinuationShape(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	legacy := readFixture(t, "playlist_continuation_legacy.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
			return fixtureResp(http.StatusOK, legacy), nil // continuationContents shape
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 2 from the initial page + 1 from the legacy continuation page.
	wantIDs := []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ddddddddddd"}
	if len(pl.Entries) != len(wantIDs) {
		t.Fatalf("entries = %d, want %d", len(pl.Entries), len(wantIDs))
	}
	for i, e := range pl.Entries {
		if e.VideoID != wantIDs[i] {
			t.Errorf("entry[%d].VideoID = %q, want %q", i, e.VideoID, wantIDs[i])
		}
	}
}

// TestEnumerate_LockupShape pages through the 2025 lockup layout end to end:
// a lockup initial page, a lockup continuation (view-model marker), then a
// legacy continuation page, mirroring how YouTube mixes shapes across pages.
func TestEnumerate_LockupShape(t *testing.T) {
	browse := readFixture(t, "playlist_browse_lockup.json")
	lockupCont := readFixture(t, "playlist_continuation_lockup.json")
	legacyCont := readFixture(t, "playlist_continuation.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("LOCKUP_CONT_1")):
			return fixtureResp(http.StatusOK, lockupCont), nil
		case bytes.Contains(body, []byte("LOCKUP_CONT_2")):
			return fixtureResp(http.StatusOK, legacyCont), nil
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Title != "Lockup Playlist" {
		t.Errorf("title = %q", pl.Title)
	}
	if len(pl.Errors) != 0 {
		t.Errorf("errors = %v", pl.Errors)
	}
	wantIDs := []string{"dummyVideo0", "dummyVideo1", "dummyVideo2", "ccccccccccc"}
	if len(pl.Entries) != len(wantIDs) {
		t.Fatalf("entries = %d, want %d", len(pl.Entries), len(wantIDs))
	}
	for i, e := range pl.Entries {
		if e.VideoID != wantIDs[i] {
			t.Errorf("entry[%d].VideoID = %q, want %q", i, e.VideoID, wantIDs[i])
		}
	}
	if e := pl.Entries[0]; e.Author != "Artist A" || e.Duration != 3*time.Minute {
		t.Errorf("entry0 = %+v, want lockup author/badge duration", e)
	}
}

// TestEnumerate_OnPageProgress verifies that onPage reports the running entry
// count once per playlist page.
func TestEnumerate_OnPageProgress(t *testing.T) {
	browse := readFixture(t, "playlist_browse_lockup.json")
	lockupCont := readFixture(t, "playlist_continuation_lockup.json")
	legacyCont := readFixture(t, "playlist_continuation.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("LOCKUP_CONT_1")):
			return fixtureResp(http.StatusOK, lockupCont), nil
		case bytes.Contains(body, []byte("LOCKUP_CONT_2")):
			return fixtureResp(http.StatusOK, legacyCont), nil
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))

	var counts []int
	pl, err := c.Enumerate(context.Background(), "PLtest", 0, func(n int) {
		counts = append(counts, n)
	})
	if err != nil {
		t.Fatal(err)
	}
	// One callback per page (initial + two continuations).
	if len(counts) < 2 {
		t.Fatalf("onPage called %d time(s), want one per page (>= 2)", len(counts))
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] {
			t.Errorf("running counts must not decrease across pages: %v", counts)
		}
	}
	if counts[len(counts)-1] <= counts[0] {
		t.Errorf("running count did not advance across pages: %v", counts)
	}
	if last := counts[len(counts)-1]; last != len(pl.Entries) {
		t.Errorf("final onPage count = %d, want %d (total entries)", last, len(pl.Entries))
	}
}

// TestEnumerate_RetriesUnrecognizedInitialPage covers a browse response whose
// JSON parses cleanly but does not expose a recognizable playlist shape. The
// client should retry once before failing the whole enumeration.
func TestEnumerate_RetriesUnrecognizedInitialPage(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	var calls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			// Parses as JSON, no error alert, but no recognizable container.
			return fixtureResp(http.StatusOK, []byte(`{"contents":{}}`)), nil
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(pl.Entries))
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (one retry after the unrecognized page)", calls)
	}
}

// TestEnumerate_DoesNotRetryRateLimit pins that the initial-browse retry loop
// leaves a 429 alone: an immediate same-session retry is counterproductive.
func TestEnumerate_DoesNotRetryRateLimit(t *testing.T) {
	var calls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return fixtureResp(http.StatusTooManyRequests, nil), nil
	}))

	_, err := c.Enumerate(context.Background(), "PLtest", 0, nil)
	if !errors.Is(err, waxerr.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	// httpx itself makes MaxRetries+1 = 2 attempts; Enumerate must add none.
	if calls != 2 {
		t.Errorf("transport calls = %d, want 2 (httpx-internal only, no Enumerate-level 429 retry)", calls)
	}
}

func TestEnumerate_HonorsConfiguredProfile(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	cont := readFixture(t, "playlist_continuation.json")

	// A caller can replace the WEB client version/key. Enumerate should use the
	// configured playlist-capable profile.
	custom := makeProfile(ClientProfile{
		Name: "WEB", InnerTubeName: "WEB", InnerTubeID: 1,
		Version: "9.99", APIKey: "KEYX", SupportsPlaylists: true,
	})
	var versions []string
	c := New(Config{
		Profiles: []ClientProfile{custom},
		HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			versions = append(versions, r.Header.Get("X-Youtube-Client-Version"))
			if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
				return fixtureResp(http.StatusOK, cont), nil
			}
			return fixtureResp(http.StatusOK, browse), nil
		})}}),
	})

	pl, err := c.Enumerate(context.Background(), "PLtest", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(pl.Entries))
	}
	if len(versions) == 0 {
		t.Fatal("no browse requests recorded")
	}
	for _, v := range versions {
		if v != "9.99" {
			t.Errorf("browse used client version %q, want 9.99 (configured profile honored)", v)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
