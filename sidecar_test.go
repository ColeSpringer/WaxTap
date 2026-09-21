package waxtap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/potoken"
)

func TestRedactURL(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://127.0.0.1:4416/player-context", "http://127.0.0.1:4416/player-context"},
		{"http://user:pass@host:4416/p?token=secret#frag", "http://host:4416/p"},
		{"https://example.com/path?a=b&c=d", "https://example.com/path"},
	}
	for _, tc := range cases {
		if got := redactURL(tc.in); got != tc.want {
			t.Errorf("redactURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Invalid URLs are replaced rather than echoed.
	if got := redactURL("http://h/%zz"); got != "<unparseable-url>" {
		t.Errorf("redactURL(bad) = %q, want <unparseable-url>", got)
	}
}

func TestBuildSidecarURL(t *testing.T) {
	cases := []struct {
		base, def, want string
	}{
		// A base with no path (or just "/") gets the default path appended.
		{"http://127.0.0.1:4417", "/get_pot", "http://127.0.0.1:4417/get_pot"},
		{"http://127.0.0.1:4417/", "/get_pot", "http://127.0.0.1:4417/get_pot"},
		// A full endpoint URL is preserved.
		{"http://host:4417/session", "/session", "http://host:4417/session"},
		{"http://host:4417/custom", "/get_pot", "http://host:4417/custom"},
		// Existing query parameters are retained.
		{"http://host:4417?key=K", "/get_pot", "http://host:4417/get_pot?key=K"},
		{"https://host/session?key=K", "/session", "https://host/session?key=K"},
	}
	for _, tc := range cases {
		got, err := buildSidecarURL(tc.base, tc.def)
		if err != nil {
			t.Errorf("buildSidecarURL(%q,%q) error: %v", tc.base, tc.def, err)
			continue
		}
		if got != tc.want {
			t.Errorf("buildSidecarURL(%q,%q) = %q, want %q", tc.base, tc.def, got, tc.want)
		}
	}
	// Non-HTTP and unparseable bases are rejected so a misconfiguration surfaces.
	for _, bad := range []string{"", "ftp://host/x", "not-a-url", "http://%zz"} {
		if _, err := buildSidecarURL(bad, "/get_pot"); err == nil {
			t.Errorf("buildSidecarURL(%q) = nil error, want a validation error", bad)
		}
	}
}

// TestNewSidecarProvidersRejectBadURL confirms the public constructors surface a
// URL validation error instead of returning an unusable provider.
func TestNewSidecarProvidersRejectBadURL(t *testing.T) {
	if _, err := NewSidecarPOTokenProvider("ftp://host/x"); err == nil {
		t.Error("NewSidecarPOTokenProvider(bad) = nil error, want a validation error")
	}
	if _, err := NewSidecarPlayerContextProvider("not-a-url"); err == nil {
		t.Error("NewSidecarPlayerContextProvider(bad) = nil error, want a validation error")
	}
	if _, err := NewSidecarSessionProvider(""); err == nil {
		t.Error("NewSidecarSessionProvider(empty) = nil error, want a validation error")
	}
}

func TestCapRunes(t *testing.T) {
	if got := capRunes("short", 10); got != "short" {
		t.Errorf("capRunes short = %q", got)
	}
	if got := capRunes("abcdef", 3); got != "abc…" {
		t.Errorf("capRunes truncate = %q, want abc…", got)
	}
	// Multibyte runes are not split.
	if got := capRunes("héllo", 2); got != "hé…" {
		t.Errorf("capRunes multibyte = %q, want hé…", got)
	}
}

func TestReadSidecarRefusal(t *testing.T) {
	refusal := func(status int, h http.Header, body string) sidecarRefusal {
		if h == nil {
			h = http.Header{}
		}
		return readSidecarRefusal(&http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))})
	}
	if got := refusal(500, nil, `{"error":"bad scope"}`); got.Reason != "bad scope" {
		t.Errorf("reason from error field = %q", got.Reason)
	}
	if got := refusal(500, nil, `{"message":"try later"}`); got.Reason != "try later" {
		t.Errorf("reason from message field = %q", got.Reason)
	}
	// Non-JSON and fieldless bodies must not be echoed.
	if got := refusal(500, nil, "<html>secret token</html>"); got != (sidecarRefusal{}) {
		t.Errorf("refusal from HTML = %+v, want empty (no leak)", got)
	}
	if got := refusal(500, nil, `{"other":"x"}`); got != (sidecarRefusal{}) {
		t.Errorf("refusal from fieldless JSON = %+v, want empty", got)
	}

	body := `{"error":"video unplayable: This video is private (playabilityStatus \"LOGIN_REQUIRED\")","code":"video-unavailable","details":"LOGIN_REQUIRED","retry_after_seconds":7}`
	r := refusal(422, http.Header{"Retry-After": {"25"}}, body)
	if r.Code != "video-unavailable" || r.Details != "LOGIN_REQUIRED" || !strings.Contains(r.Reason, "private") {
		t.Errorf("refusal = %+v", r)
	}
	if r.RetryAfter != 25*time.Second {
		t.Errorf("RetryAfter = %v, want the header to win over retry_after_seconds", r.RetryAfter)
	}
	if r := refusal(503, nil, `{"error":"no session","code":"no-session","retry_after_seconds":12}`); r.RetryAfter != 12*time.Second || r.Code != "no-session" {
		t.Errorf("refusal = %+v, want retry_after_seconds honoured", r)
	}

	// Reason and code are rune-capped so a hostile body cannot flood a message.
	long := strings.Repeat("x", 300)
	longCode := strings.Repeat("c", 100)
	r = refusal(500, nil, `{"error":"`+long+`","code":"`+longCode+`"}`)
	if []rune(r.Reason)[sidecarReasonRunes] != '…' || len([]rune(r.Reason)) != sidecarReasonRunes+1 {
		t.Errorf("reason len = %d runes, want %d plus an ellipsis", len([]rune(r.Reason)), sidecarReasonRunes)
	}
	if len([]rune(r.Code)) != sidecarCodeRunes+1 {
		t.Errorf("code len = %d runes, want %d plus an ellipsis", len([]rune(r.Code)), sidecarCodeRunes)
	}
}

func TestSidecarResponseErrorVerdict(t *testing.T) {
	sre := &SidecarResponseError{Label: "player-context server", Endpoint: "http://127.0.0.1:4416/player-context", StatusCode: 422,
		Code: SidecarCodeVideoUnavailable, Details: "LOGIN_REQUIRED", Reason: "video unplayable: This video is private"}
	if !errors.Is(sre, ErrVideoRestricted) {
		t.Errorf("private LOGIN_REQUIRED must unwrap to ErrVideoRestricted: %v", sre)
	}
	if !strings.Contains(sre.Error(), "HTTP 422 (video-unavailable)") {
		t.Errorf("Error() = %q, want the code beside the status", sre.Error())
	}
	generic := &SidecarResponseError{StatusCode: 422, Code: SidecarCodeVideoUnavailable}
	if !errors.Is(generic, ErrVideoUnavailable) {
		t.Error("a video-unavailable refusal without details is the generic verdict")
	}
	other := &SidecarResponseError{StatusCode: 502, Code: "player-context-failed"}
	if errors.Is(other, ErrVideoUnavailable) || other.Unwrap() != nil {
		t.Error("only video-unavailable carries a verdict")
	}
	// A verdict a provider raised from a 200 body has no status, but the code is
	// what explains its exit 3, so it has to reach the message there too.
	coded200 := &SidecarResponseError{Label: "player-context server", Endpoint: "http://127.0.0.1:4416/player-context",
		Code: SidecarCodeVideoUnavailable, Details: "ERROR", Reason: `playability_status "ERROR"`}
	if !strings.Contains(coded200.Error(), "(video-unavailable)") {
		t.Errorf("Error() = %q, want the code even without an HTTP status", coded200.Error())
	}
	if !errors.Is(coded200, ErrVideoUnavailable) {
		t.Errorf("a coded 200 verdict must still unwrap: %v", coded200)
	}
	// Cause is a provider's own error for a caller that knows the provider. It is
	// neither unwrapped nor printed, so it cannot reclassify the refusal or
	// redirect an errors.Is that reads it as the playability verdict.
	cause := &fs.PathError{Op: "read", Path: "/tmp/socket", Err: context.DeadlineExceeded}
	withCause := &SidecarResponseError{Label: "player-context server", Endpoint: "http://127.0.0.1:4416/player-context",
		StatusCode: 502, Code: "player-context-failed", Cause: cause}
	if errors.Is(withCause, context.DeadlineExceeded) {
		t.Error("Cause must stay invisible to errors.Is")
	}
	if _, ok := errors.AsType[*fs.PathError](withCause); ok {
		t.Error("errors.AsType must not reach through a Cause")
	}
	if withCause.Unwrap() != nil {
		t.Error("a non-verdict refusal unwraps to nothing, Cause or not")
	}
	if got := withCause.Error(); !strings.Contains(got, "HTTP 502 (player-context-failed)") || strings.Contains(got, "deadline") {
		t.Errorf("Error() = %q, want the refusal alone: Cause is never printed", got)
	}
}

func TestSidecarRetryWait(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		want  time.Duration
		retry bool
	}{
		{"transport", &SidecarError{Err: errors.New("dial")}, sidecarTransientWait, true},
		{"408", &SidecarResponseError{StatusCode: 408}, sidecarTransientWait, true},
		{"500", &SidecarResponseError{StatusCode: 500}, sidecarTransientWait, true},
		{"502", &SidecarResponseError{StatusCode: 502}, sidecarTransientWait, true},
		{"503", &SidecarResponseError{StatusCode: 503}, sidecarTransientWait, true},
		{"504", &SidecarResponseError{StatusCode: 504}, sidecarTransientWait, true},
		// The set matches internal/httpx: a status that names something the
		// sidecar will not do is final there too.
		{"501", &SidecarResponseError{StatusCode: 501}, 0, false},
		{"505", &SidecarResponseError{StatusCode: 505}, 0, false},
		{"507", &SidecarResponseError{StatusCode: 507}, 0, false},
		{"bare 429", &SidecarResponseError{StatusCode: 429}, 0, false},
		{"429 with a stated wait", &SidecarResponseError{StatusCode: 429, RetryAfter: 3 * time.Second}, 3 * time.Second, true},
		{"401", &SidecarResponseError{StatusCode: 401}, 0, false},
		{"malformed 200", &SidecarResponseError{StatusCode: 0, Reason: "malformed JSON response"}, 0, false},
		{"verdict", &SidecarResponseError{StatusCode: 422, Code: SidecarCodeVideoUnavailable}, 0, false},
		{"wait past the cap", &SidecarResponseError{StatusCode: 502, RetryAfter: sidecarRetryMaxWait + time.Second}, 0, false},
		{"other error", errors.New("boom"), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, retry := SidecarRetryWait(tc.err)
			if retry != tc.retry || (retry && got != tc.want) {
				t.Errorf("SidecarRetryWait = %v, %v; want %v, %v", got, retry, tc.want, tc.retry)
			}
		})
	}
}

// TestPauseExportsRunTheOneRule pins that the exported pause policy is the
// policy itself and not a second copy of it: an in-process adapter (WaxSeal's
// provider) calls these, so each has to answer the way internal/httpx does.
// The rules are pinned there (TestPauseBlocked, TestKeepCause); what is pinned
// here is the export and the case that separates each function from the
// other, since a wrapper crossed over would pass a weaker test.
func TestPauseExportsRunTheOneRule(t *testing.T) {
	pending := errors.New("refusal")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := PauseBlocked(cancelled, time.Second, pending); !errors.Is(err, context.Canceled) {
		t.Errorf("PauseBlocked cancelled = %v, want the cancellation to outrank the pending error", err)
	}
	tight, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := PauseBlocked(tight, time.Second, pending); err != pending {
		t.Errorf("PauseBlocked with no headroom = %v, want the pending error", err)
	}
	if err := PauseBlocked(context.Background(), time.Hour, pending); err != nil {
		t.Errorf("PauseBlocked with no deadline = %v, want nil", err)
	}
	if err := PauseInterrupted(context.DeadlineExceeded, pending); err != pending {
		t.Errorf("PauseInterrupted deadline = %v, want the pending error it explains", err)
	}
	if err := PauseInterrupted(context.Canceled, pending); !errors.Is(err, context.Canceled) {
		t.Errorf("PauseInterrupted cancelled = %v, want the cancellation", err)
	}
}

func TestBgutilProviderPlayerScope(t *testing.T) {
	var gotBinding, gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_pot" {
			t.Errorf("path = %q, want /get_pot", r.URL.Path)
		}
		var req bgutilRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotBinding, gotScope = req.ContentBinding, req.Scope
		_ = json.NewEncoder(w).Encode(bgutilResponse{POToken: "TOKEN-P", ExpiresAt: "2026-06-09T07:25:25Z"})
	}))
	defer srv.Close()

	p, err := NewSidecarPOTokenProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.ProvidePOToken(context.Background(), potoken.Request{
		Scope:   potoken.ScopePlayer,
		VideoID: "vid123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBinding != "vid123" {
		t.Errorf("content_binding = %q, want vid123 (player scope binds to the video ID)", gotBinding)
	}
	if gotScope != "player" {
		t.Errorf("scope = %q, want player", gotScope)
	}
	if resp.Token != "TOKEN-P" {
		t.Errorf("token = %q, want TOKEN-P", resp.Token)
	}
	if want := time.Date(2026, 6, 9, 7, 25, 25, 0, time.UTC); !resp.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (RFC3339)", resp.ExpiresAt, want)
	}
}

// TestBgutilProviderSendsAPIKey verifies the X-API-Key header is sent only when a
// key is configured with WithSidecarAPIKey.
func TestBgutilProviderSendsAPIKey(t *testing.T) {
	for _, key := range []string{"secret-key", ""} {
		var gotKey string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotKey = r.Header.Get("X-API-Key")
			_ = json.NewEncoder(w).Encode(bgutilResponse{POToken: "T"})
		}))
		p, err := NewSidecarPOTokenProvider(srv.URL, WithSidecarAPIKey(key))
		if err != nil {
			srv.Close()
			t.Fatalf("key %q: %v", key, err)
		}
		_, err = p.ProvidePOToken(context.Background(),
			potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"})
		srv.Close()
		if err != nil {
			t.Fatalf("key %q: %v", key, err)
		}
		if gotKey != key {
			t.Errorf("X-API-Key = %q, want %q", gotKey, key)
		}
	}
}

func TestBgutilProviderGVSScopeAndEpochExpiry(t *testing.T) {
	var gotBinding, gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req bgutilRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotBinding, gotScope = req.ContentBinding, req.Scope
		_ = json.NewEncoder(w).Encode(bgutilResponse{POToken: "TOKEN-G", ExpiresAt: "1812345925"})
	}))
	defer srv.Close()

	p, err := NewSidecarPOTokenProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.ProvidePOToken(context.Background(), potoken.Request{
		Scope:       potoken.ScopeGVS,
		VisitorData: "VISITOR==",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotBinding != "VISITOR==" {
		t.Errorf("content_binding = %q, want the visitor data (GVS scope)", gotBinding)
	}
	if gotScope != "gvs" {
		t.Errorf("scope = %q, want gvs", gotScope)
	}
	if resp.Token != "TOKEN-G" {
		t.Errorf("token = %q, want TOKEN-G", resp.Token)
	}
	if want := time.Unix(1812345925, 0).UTC(); !resp.ExpiresAt.Equal(want) {
		t.Errorf("expiresAt = %v, want %v (epoch tolerated)", resp.ExpiresAt, want)
	}
}

// TestBgutilProviderServerError verifies a non-200 becomes a SidecarResponseError
// carrying the HTTP status, which the CLI classifier maps to an exit code.
func TestBgutilProviderServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no integrity token", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p, err := NewSidecarPOTokenProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, provErr := p.ProvidePOToken(context.Background(),
		potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"})
	if provErr == nil {
		t.Fatal("expected an error on a non-200 response")
	}
	sre, ok := errors.AsType[*SidecarResponseError](provErr)
	if !ok || sre.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a SidecarResponseError with StatusCode 503", provErr)
	}
}

// TestBgutilProviderMalformedJSONReason confirms a 200 with an undecodable body
// surfaces the decode detail (a structured json error, not raw body bytes) so a
// custom sidecar integration is debuggable, and classifies as an invalid 200
// (StatusCode 0).
func TestBgutilProviderMalformedJSONReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()

	p, err := NewSidecarPOTokenProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, provErr := p.ProvidePOToken(context.Background(),
		potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"})
	sre, ok := errors.AsType[*SidecarResponseError](provErr)
	if !ok {
		t.Fatalf("err = %v, want a SidecarResponseError", provErr)
	}
	if sre.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (a 200 with invalid content)", sre.StatusCode)
	}
	if !strings.HasPrefix(sre.Reason, "malformed JSON response:") {
		t.Errorf("Reason = %q, want the malformed-JSON prefix with the decode detail", sre.Reason)
	}
	// The decode error itself rides along in Cause, where a caller that wants the
	// offset or the offending field can read it without parsing the Reason text.
	if _, ok := errors.AsType[*json.SyntaxError](sre.Cause); !ok {
		t.Errorf("Cause = %#v, want the *json.SyntaxError the decode returned", sre.Cause)
	}
	// Error() is unchanged by the addition: the decode detail reaches it through
	// Reason, as it always did, and Cause adds nothing to the text.
	bare := &SidecarResponseError{Label: sre.Label, Endpoint: sre.Endpoint, StatusCode: sre.StatusCode, Reason: sre.Reason}
	if sre.Error() != bare.Error() {
		t.Errorf("Error() = %q, want the same line as without a Cause (%q)", sre.Error(), bare.Error())
	}
}

func TestBgutilProviderBindingErrorsBeforeRequest(t *testing.T) {
	// These must fail in contentBinding, before any HTTP call, so the unroutable
	// address is never contacted.
	p, err := NewSidecarPOTokenProvider("http://127.0.0.1:0/get_pot")
	if err != nil {
		t.Fatal(err)
	}
	cases := []potoken.Request{
		{Scope: potoken.ScopePlayer},                  // no video ID
		{Scope: potoken.ScopeGVS},                     // no visitor data
		{Scope: potoken.ScopeSubtitles, VideoID: "v"}, // unsupported scope
	}
	for _, req := range cases {
		if _, err := p.ProvidePOToken(context.Background(), req); err == nil {
			t.Errorf("scope %s: expected an error before any request", req.Scope)
		}
	}
}

// TestSidecarRefusalSurvivesALargeEnvelope: WaxSeal clamps each text field of
// its error envelope at 4 KiB, and a player-context refusal can carry a browser
// trace in both error and details. Once JSON-escaped that is past 8 KiB, and a
// read that stops there loses the code and the details with the truncated
// document; the limit has to cover the envelope.
func TestSidecarRefusalSurvivesALargeEnvelope(t *testing.T) {
	recordSidecarSleeps(t)
	trace := strings.Repeat("at frame\n", 512) // 4.5 KiB raw, more escaped
	body, err := json.Marshal(map[string]any{"error": trace, "code": "player-context-failed", "details": trace})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := scriptedSidecar(t, sidecarReply{status: http.StatusBadGateway, body: string(body)})
	p, err := NewSidecarPlayerContextProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ProvidePlayerContext(context.Background(), "dummyVideo0")
	sre, ok := errors.AsType[*SidecarResponseError](err)
	if !ok || sre.Code != "player-context-failed" || sre.Details == "" {
		t.Fatalf("err = %v, want the refusal's code and details read from the large envelope", err)
	}
}

// TestSidecarRefusalPastTheLimitSaysSo: a decode fills nothing when the document
// is cut, so an envelope past the limit loses the code with it. The refusal has
// to say that rather than report a bare status, since a reason the reader can
// act on is the difference between a contract mismatch and a mystery 502.
func TestSidecarRefusalPastTheLimitSaysSo(t *testing.T) {
	recordSidecarSleeps(t)
	huge, err := json.Marshal(map[string]any{
		"error": strings.Repeat("y", sidecarErrorBodyLimit+1), "code": "player-context-failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := scriptedSidecar(t, sidecarReply{status: http.StatusBadGateway, body: string(huge)})
	p, err := NewSidecarPlayerContextProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ProvidePlayerContext(context.Background(), "dummyVideo0")
	sre, ok := errors.AsType[*SidecarResponseError](err)
	if !ok || !strings.Contains(sre.Reason, "past the") {
		t.Fatalf("err = %v, want the refusal to name the body it could not read", err)
	}
}

// TestSidecarClientDoesNotFollowRedirects confirms the dedicated client pins
// credentials to the endpoint: a 3xx becomes a SidecarResponseError instead of
// being chased to another host.
func TestSidecarClientDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_ = json.NewEncoder(w).Encode(bgutilResponse{POToken: "LEAKED"})
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/get_pot", http.StatusFound)
	}))
	defer srv.Close()

	p, err := NewSidecarPOTokenProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, provErr := p.ProvidePOToken(context.Background(),
		potoken.Request{Scope: potoken.ScopePlayer, VideoID: "v"})
	if provErr == nil {
		t.Fatal("expected an error: a redirect must not be followed to another host")
	}
	sre, ok := errors.AsType[*SidecarResponseError](provErr)
	if !ok || sre.StatusCode != http.StatusFound {
		t.Fatalf("err = %v, want a SidecarResponseError with StatusCode 302", provErr)
	}
	if !strings.HasPrefix(sre.Reason, "redirected to ") {
		t.Errorf("reason = %q, want where the redirect pointed; a 3xx carries no envelope", sre.Reason)
	}
	if n := len([]rune(sre.Reason)); n > sidecarReasonRunes+1 {
		t.Errorf("reason ran to %d runes; a Location is capped like every other reason", n)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target contacted %d times; credentials must stay bound to the endpoint", n)
	}
}

const validPlayerContextJSON = `{
  "playability_status": "OK",
  "player_url": "https://www.youtube.com/s/player/444511ca/player_es6.vflset/en_US/base.js",
  "server_abr_streaming_url": "https://rr3.googlevideo.com/videoplayback?n=SCRAMBLED&sabr=1",
  "video_playback_ustreamer_config": "dXN0cmVhbWVy",
  "visitor_data": "CgtWSVNJVE9S",
  "user_agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
  "client_version": "2.20260606.02.00",
  "title": "Big Buck Bunny",
  "author": "Blender",
  "length_seconds": 634,
  "channel_id": "UCdummy",
  "description": "desc",
  "thumbnails": [
    {"url":"https://i.ytimg.com/vi/dummyVideo0/default.jpg","width":168,"height":94},
    {"url":"https://i.ytimg.com/vi/dummyVideo0/mqdefault.jpg","width":336,"height":188},
    {"url":"https://i.ytimg.com/vi/dummyVideo0/maxresdefault.jpg","width":1280,"height":720}
  ],
  "is_live_content": true,
  "is_live_now": false,
  "is_upcoming": false,
  "publish_date": "2015-04-10T00:00:00-07:00",
  "audio_formats": [
    {"itag":251,"lmt":"1719185012384481","xtags":"","mime_type":"audio/webm; codecs=\"opus\"","bitrate":143452,"audio_channels":2,"audio_sample_rate":48000,"content_length":9700000,"approx_duration_ms":634624}
  ],
  "session_generation": 7
}`

func newPlayerContextServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/player-context" {
			t.Errorf("path = %q, want /player-context", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestPlayerContextProviderDecode(t *testing.T) {
	srv := newPlayerContextServer(t, http.StatusOK, validPlayerContextJSON)
	defer srv.Close()

	p, err := NewSidecarPlayerContextProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := p.ProvidePlayerContext(context.Background(), "aqz-KE-bpKQ")
	if err != nil {
		t.Fatalf("ProvidePlayerContext: %v", err)
	}
	if pc.ServerAbrURL == "" || pc.VisitorData != "CgtWSVNJVE9S" || pc.ClientVersion != "2.20260606.02.00" {
		t.Errorf("decoded context = %+v", pc)
	}
	// The browser identity travels with the context, so the WEB requests under it
	// present the browser the URL was minted on rather than WaxTap's own Chrome.
	if !strings.Contains(pc.UserAgent, "Chrome/141.0.0.0") {
		t.Errorf("user_agent = %q, want the attesting browser's navigator.userAgent", pc.UserAgent)
	}
	if pc.PlayerURL != "https://www.youtube.com/s/player/444511ca/player_es6.vflset/en_US/base.js" {
		t.Errorf("player_url = %q", pc.PlayerURL)
	}
	if pc.Title != "Big Buck Bunny" || pc.LengthSeconds != 634 {
		t.Errorf("metadata = title %q length %d", pc.Title, pc.LengthSeconds)
	}
	if len(pc.AudioFormats) != 1 {
		t.Fatalf("audio formats = %d, want 1", len(pc.AudioFormats))
	}
	f := pc.AudioFormats[0]
	if f.Itag != 251 || f.LMT != "1719185012384481" || f.ContentLength != 9700000 || f.AudioSampleRate != 48000 {
		t.Errorf("format = %+v", f)
	}
	if pc.Generation != 7 {
		t.Errorf("Generation = %d, want 7 (session_generation)", pc.Generation)
	}
	if pc.ChannelID != "UCdummy" || pc.Description != "desc" {
		t.Errorf("channel/description = %q/%q", pc.ChannelID, pc.Description)
	}
	if len(pc.Thumbnails) != 3 || pc.Thumbnails[0].Width != 168 {
		t.Errorf("thumbnails = %+v, want the wire order kept on the contract type", pc.Thumbnails)
	}
	if !pc.IsLiveContent || pc.IsLiveNow || pc.IsUpcoming {
		t.Errorf("live flags = %v/%v/%v, want a finished broadcast", pc.IsLiveContent, pc.IsLiveNow, pc.IsUpcoming)
	}
	if pc.PublishDate != "2015-04-10T00:00:00-07:00" {
		t.Errorf("publishDate = %q, want the raw microformat string", pc.PublishDate)
	}

	t.Run("older provider omits the metadata keys", func(t *testing.T) {
		bare := `{"playability_status":"OK","server_abr_streaming_url":"u","video_playback_ustreamer_config":"c","visitor_data":"v","audio_formats":[{"itag":251}]}`
		srv := newPlayerContextServer(t, http.StatusOK, bare)
		defer srv.Close()
		p, err := NewSidecarPlayerContextProvider(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		pc, err := p.ProvidePlayerContext(context.Background(), "dummyVideo0")
		if err != nil {
			t.Fatal(err)
		}
		if pc.ChannelID != "" || pc.Description != "" || pc.PublishDate != "" {
			t.Errorf("absent keys = %q/%q/%q, want empty", pc.ChannelID, pc.Description, pc.PublishDate)
		}
		if pc.Thumbnails != nil {
			t.Errorf("thumbnails = %+v, want nil when the body carried none", pc.Thumbnails)
		}
		if pc.IsLiveContent || pc.IsLiveNow || pc.IsUpcoming {
			t.Error("absent live flags must be false")
		}
	})
}

func TestPlayerContextProviderErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"non-200", http.StatusInternalServerError, "boom", "returned"},
		{"status not OK", http.StatusOK, `{"playability_status":"ERROR: bot check"}`, "playability_status"},
		{"video-unavailable 422", http.StatusUnprocessableEntity, `{"error":"video unplayable: This video is private","code":"video-unavailable","details":"LOGIN_REQUIRED"}`, "video-unavailable"},
		{"missing url", http.StatusOK, `{"playability_status":"OK","visitor_data":"v","audio_formats":[{"itag":251}]}`, "missing"},
		{"missing visitor", http.StatusOK, `{"playability_status":"OK","server_abr_streaming_url":"u","audio_formats":[{"itag":251}]}`, "missing"},
		{"no formats", http.StatusOK, `{"playability_status":"OK","server_abr_streaming_url":"u","visitor_data":"v"}`, "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newPlayerContextServer(t, tc.status, tc.body)
			defer srv.Close()
			p, err := NewSidecarPlayerContextProvider(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, provErr := p.ProvidePlayerContext(context.Background(), "v")
			if provErr == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(provErr.Error(), tc.want) {
				t.Errorf("err = %q, want substring %q", provErr.Error(), tc.want)
			}
			switch tc.name {
			case "video-unavailable 422":
				if !errors.Is(provErr, ErrVideoRestricted) {
					t.Errorf("err = %v, want the relayed playability verdict", provErr)
				}
				sre, ok := errors.AsType[*SidecarResponseError](provErr)
				if !ok || sre.Code != SidecarCodeVideoUnavailable {
					t.Errorf("err = %v, want a SidecarResponseError carrying the code", provErr)
				}
			case "status not OK":
				// A 200 whose playability status is not OK is the same verdict.
				if !errors.Is(provErr, ErrVideoUnavailable) {
					t.Errorf("err = %v, want ErrVideoUnavailable", provErr)
				}
			}
		})
	}
}

func TestParseNetscapeCookies(t *testing.T) {
	// A real-world-ish file: header comment, a blank line, a normal cookie, an
	// #HttpOnly_ cookie, a six-field empty-value cookie (trailing tab dropped by
	// some exporters), and a too-short line that must be skipped.
	const content = "# Netscape HTTP Cookie File\n" +
		"\n" +
		".youtube.com\tTRUE\t/\tFALSE\t1799999999\tPREF\tval123\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tVISITOR_INFO1_LIVE\tabc\n" +
		"# a comment line\n" +
		".youtube.com\tTRUE\t/\tFALSE\t0\tEMPTYVAL\n" +
		"too\tfew\tfields\n"
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cookies, err := ParseNetscapeCookies(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 3 {
		t.Fatalf("got %d cookies, want 3 (comment/under-six-field lines skipped, six-field empty-value kept): %+v", len(cookies), cookies)
	}

	pref := cookies[0]
	if pref.Name != "PREF" || pref.Value != "val123" || pref.Domain != ".youtube.com" || pref.Path != "/" {
		t.Errorf("PREF cookie = %+v", pref)
	}
	if pref.Secure {
		t.Error("PREF secure flag should be FALSE")
	}
	if pref.Expires.IsZero() {
		t.Error("PREF should have a non-zero expiry (unix 1799999999)")
	}

	vis := cookies[1]
	if vis.Name != "VISITOR_INFO1_LIVE" || !vis.HttpOnly {
		t.Errorf("#HttpOnly_ cookie not parsed as HttpOnly: %+v", vis)
	}
	if vis.Domain != ".youtube.com" {
		t.Errorf("#HttpOnly_ domain = %q, want .youtube.com (prefix stripped)", vis.Domain)
	}
	if !vis.Secure {
		t.Error("VISITOR_INFO1_LIVE secure flag should be TRUE")
	}
	if !vis.Expires.IsZero() {
		t.Error("expiry 0 should be a session cookie (zero time)")
	}

	// A six-field line (empty value, trailing tab dropped) parses with an empty value.
	empty := cookies[2]
	if empty.Name != "EMPTYVAL" || empty.Value != "" {
		t.Errorf("six-field cookie = %+v, want name EMPTYVAL with an empty value", empty)
	}
	if empty.Domain != ".youtube.com" || empty.Path != "/" {
		t.Errorf("six-field cookie = %+v, want domain/path still parsed", empty)
	}

	// An expiry that does not parse fails the file, naming the line: reading
	// it as "no expiry" makes it a session cookie, so a credential the export
	// had already retired would be sent for the rest of the run with nothing
	// to say so. The leniency above (headers, comments, short lines) is for
	// lines that are not cookies at all, which is a different thing.
	// A blank column is a session cookie, the way 0 is, and a fractional
	// timestamp still names a moment. Only a value with no reading fails.
	for _, tc := range []struct {
		name, field string
		wantZero    bool
	}{
		{"blank", "", true},
		{"whitespace", " ", true},
		{"zero", "0", true},
		{"seconds", "1799999999", false},
		{"fractional", "1799999999.5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "cookies.txt")
			line := ".youtube.com\tTRUE\t/\tFALSE\t" + tc.field + "\tPREF\tv\n"
			if werr := os.WriteFile(p, []byte(line), 0o600); werr != nil {
				t.Fatal(werr)
			}
			c, cerr := ParseNetscapeCookies(p)
			if cerr != nil {
				t.Fatalf("expiry %q failed the file: %v", tc.field, cerr)
			}
			if len(c) != 1 {
				t.Fatalf("got %d cookies, want 1", len(c))
			}
			if c[0].Expires.IsZero() != tc.wantZero {
				t.Errorf("expiry %q -> %v, want zero = %v", tc.field, c[0].Expires, tc.wantZero)
			}
		})
	}

	badExpiry := filepath.Join(t.TempDir(), "cookies.txt")
	if werr := os.WriteFile(badExpiry, []byte("# header\n.youtube.com\tTRUE\t/\tFALSE\tnot-a-number\tPREF\tv\n"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	if _, eerr := ParseNetscapeCookies(badExpiry); eerr == nil {
		t.Error("an expiry that does not parse was accepted")
	} else if !strings.Contains(eerr.Error(), "expiry") || !strings.Contains(eerr.Error(), "line 2") {
		t.Errorf("err = %v, want it to name the expiry and line 2", eerr)
	}

	// The secure flag is the opposite case: it decides only whether a cookie
	// may travel over plain HTTP, and every jar-backed request WaxTap makes is
	// HTTPS, so an odd spelling reads as FALSE rather than failing an export
	// that otherwise works.
	odd := filepath.Join(t.TempDir(), "cookies.txt")
	body := ".youtube.com\tTRUE\t/\tMAYBE\t0\tPREF\tv\n" +
		".youtube.com\tTRUE\t/\ttrue\t0\tSID\tw\n"
	if werr := os.WriteFile(odd, []byte(body), 0o600); werr != nil {
		t.Fatal(werr)
	}
	loose, lerr := ParseNetscapeCookies(odd)
	if lerr != nil {
		t.Fatalf("an odd secure flag failed the file: %v", lerr)
	}
	if len(loose) != 2 {
		t.Fatalf("got %d cookies, want 2", len(loose))
	}
	if loose[0].Secure {
		t.Error("an unreadable secure flag should read as FALSE")
	}
	if !loose[1].Secure {
		t.Error(`a lowercase "true" is TRUE; the column is case-insensitive`)
	}

	// An empty file is still a valid one, as the documented leniency says.
	blank := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(blank, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ParseNetscapeCookies(blank); err != nil || len(got) != 0 {
		t.Errorf("empty file = %v, %v, want no cookies and no error", got, err)
	}
}

func TestParseSessionExpiry(t *testing.T) {
	if got := parseSessionExpiry([]byte(`1799999999`)); got.IsZero() {
		t.Error("unix-seconds expiry should parse to a non-zero time")
	}
	if got := parseSessionExpiry([]byte(`"2030-01-02T03:04:05Z"`)); got.IsZero() {
		t.Error("RFC3339 expiry should parse to a non-zero time")
	}
	for _, raw := range []string{``, `0`, `null`, `"garbage"`} {
		if got := parseSessionExpiry([]byte(raw)); !got.IsZero() {
			t.Errorf("parseSessionExpiry(%q) = %v, want zero (session cookie)", raw, got)
		}
	}
}

// TestHTTPSessionProvider covers the reference minter's snake_case /session shape
// (visitor_data, http_only, extra keys) plus a session cookie with no expires.
func TestHTTPSessionProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"visitor_data": "CgtBROWSER%3D%3D",
			"user_agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/149.0.0.0 Safari/537.36",
			"client_version": "2.20260901.00.00",
			"cookie_header": "ignored=1",
			"cookies": [
				{"name":"PREF","value":"p","domain":".youtube.com","path":"/","secure":true,"http_only":false,"expires":1799999999},
				{"name":"YSC","value":"y","domain":".youtube.com","path":"/","secure":true,"http_only":true}
			]
		}`))
	}))
	defer srv.Close()

	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := p.ProvideSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sess.VisitorData != "CgtBROWSER%3D%3D" {
		t.Errorf("visitorData = %q, want the verbatim literal", sess.VisitorData)
	}
	if len(sess.Cookies) != 2 {
		t.Fatalf("cookies = %d, want 2", len(sess.Cookies))
	}
	if sess.Cookies[0].Name != "PREF" || !sess.Cookies[0].Secure || sess.Cookies[0].Expires.IsZero() {
		t.Errorf("PREF cookie = %+v", sess.Cookies[0])
	}
	if sess.Cookies[1].Name != "YSC" || !sess.Cookies[1].HttpOnly {
		t.Errorf("YSC http_only not parsed: %+v", sess.Cookies[1])
	}
	if sess.UserAgent != "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/149.0.0.0 Safari/537.36" || sess.ClientVersion != "2.20260901.00.00" {
		t.Errorf("browser identity = %q / %q, want the attesting browser's", sess.UserAgent, sess.ClientVersion)
	}
}

// TestHTTPSessionProviderCamelCase confirms the documented camelCase variant is
// still accepted (visitorData/httpOnly), so the contract does not break on casing.
func TestHTTPSessionProviderCamelCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"visitorData":"CgtX%3D%3D","userAgent":"Mozilla/5.0 camel","clientVersion":"2.20260901.00.00","cookies":[{"name":"PREF","value":"p","domain":".youtube.com","httpOnly":true}]}`))
	}))
	defer srv.Close()
	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := p.ProvideSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sess.VisitorData != "CgtX%3D%3D" || len(sess.Cookies) != 1 || !sess.Cookies[0].HttpOnly {
		t.Errorf("camelCase not accepted: vd=%q cookies=%+v", sess.VisitorData, sess.Cookies)
	}
	if sess.UserAgent != "Mozilla/5.0 camel" || sess.ClientVersion != "2.20260901.00.00" {
		t.Errorf("camelCase identity not accepted: %q / %q", sess.UserAgent, sess.ClientVersion)
	}
}

// TestHTTPSessionProviderNoIdentity pins the optional half: a document without
// the identity keys leaves both fields empty, so WaxTap keeps its own.
func TestHTTPSessionProviderNoIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"visitor_data":"CgtX%3D%3D"}`))
	}))
	defer srv.Close()
	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := p.ProvideSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sess.UserAgent != "" || sess.ClientVersion != "" {
		t.Errorf("identity = %q / %q, want both empty", sess.UserAgent, sess.ClientVersion)
	}
}

func TestHTTPSessionProviderEmptyVisitorDataErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"visitorData":"","cookies":[]}`))
	}))
	defer srv.Close()
	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProvideSession(context.Background()); err == nil {
		t.Fatal("expected an error for an empty visitorData")
	}
}

func TestHTTPSessionProviderRetriesOnceThenFails(t *testing.T) {
	waits := recordSidecarSleeps(t)
	srv, hitCount := scriptedSidecar(t, sidecarReply{status: http.StatusInternalServerError, body: "boom"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProvideSession(ctx); err == nil {
		t.Fatal("expected failure after retries")
	}
	if hitCount.Load() != 2 {
		t.Errorf("server hits = %d, want 2 (one retry)", hitCount.Load())
	}
	if len(*waits) != 1 || (*waits)[0] != sidecarTransientWait {
		t.Errorf("waits = %v, want one %v poke", *waits, sidecarTransientWait)
	}
}

// TestHTTPSessionProviderInvalidateSession covers the delivery-cap escape for an
// adopted session: the generation from /session is what gets reported, and the
// report goes to the /report sibling of the configured session endpoint.
func TestHTTPSessionProviderInvalidateSession(t *testing.T) {
	var reportPath string
	var reportBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/report" {
			reportPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&reportBody)
			_, _ = w.Write([]byte(`{"accepted":true,"retired":true,"generation":7}`))
			return
		}
		_, _ = w.Write([]byte(`{"visitor_data":"CgtX%3D%3D","session_generation":7,"cookies":[]}`))
	}))
	defer srv.Close()

	// Configure the full session endpoint, not the base, to pin the sibling
	// derivation rather than a lucky default-path append.
	p, err := NewSidecarSessionProvider(srv.URL + "/session")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := p.ProvideSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sess.Generation != 7 {
		t.Fatalf("Generation = %d, want 7", sess.Generation)
	}

	inv, ok := p.(potoken.SessionInvalidator)
	if !ok {
		t.Fatal("the sidecar session provider must implement potoken.SessionInvalidator")
	}
	if err := inv.InvalidateSession(context.Background(), potoken.SessionInvalidation{
		Generation: sess.Generation,
		VideoID:    "dummyVideo0",
		Reason:     "delivery-cap",
	}); err != nil {
		t.Fatalf("InvalidateSession: %v", err)
	}
	if reportPath != "/report" {
		t.Errorf("report path = %q, want /report", reportPath)
	}
	if got, _ := reportBody["session_generation"].(float64); got != 7 {
		t.Errorf("session_generation = %v, want 7", reportBody["session_generation"])
	}
	if reportBody["video_id"] != "dummyVideo0" || reportBody["reason"] != "delivery-cap" {
		t.Errorf("report body = %v, want the video and reason forwarded", reportBody)
	}
}

// TestHTTPSessionProviderInvalidateSessionRateLimited pins that a sidecar asking
// for backoff fails the invalidation: rotating anyway would re-adopt the same
// capped session.
func TestHTTPSessionProviderInvalidateSessionRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"accepted":false,"generation":7,"retry_after_seconds":42}`))
	}))
	defer srv.Close()
	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = p.(potoken.SessionInvalidator).InvalidateSession(context.Background(), potoken.SessionInvalidation{Generation: 7})
	if err == nil {
		t.Fatal("expected an error when the report is rate-limited")
	}
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("error should carry the retry delay: %v", err)
	}
}

// TestHTTPSessionProviderInvalidateSessionUnversioned pins that a sidecar which
// does not version its sessions cannot be reported to: /report requires a
// generation, so the call fails locally instead of sending a rejected request.
func TestHTTPSessionProviderInvalidateSessionUnversioned(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.(potoken.SessionInvalidator).InvalidateSession(context.Background(), potoken.SessionInvalidation{}); err == nil {
		t.Fatal("expected an error when the session carries no generation")
	}
	if hits != 0 {
		t.Errorf("report requests = %d, want 0", hits)
	}
}

// TestPlayerContextProviderInvalidateSession pins that the player-context
// provider reports capped sessions to the /report sibling of its endpoint,
// sharing the session provider's report contract.
func TestPlayerContextProviderInvalidateSession(t *testing.T) {
	var reportBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/report" {
			t.Errorf("path = %q, want /report", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&reportBody)
		_, _ = w.Write([]byte(`{"accepted":true,"retired":true,"generation":7}`))
	}))
	defer srv.Close()

	// Configure the full endpoint, not the base, to pin the sibling derivation.
	p, err := NewSidecarPlayerContextProvider(srv.URL + "/player-context")
	if err != nil {
		t.Fatal(err)
	}
	inv, ok := p.(potoken.SessionInvalidator)
	if !ok {
		t.Fatal("the sidecar player-context provider must implement potoken.SessionInvalidator")
	}
	if err := inv.InvalidateSession(context.Background(), potoken.SessionInvalidation{
		Generation: 7,
		VideoID:    "dummyVideo0",
		Reason:     "stream-capped",
	}); err != nil {
		t.Fatalf("InvalidateSession: %v", err)
	}
	if got, _ := reportBody["session_generation"].(float64); got != 7 {
		t.Errorf("session_generation = %v, want 7", reportBody["session_generation"])
	}
	if reportBody["video_id"] != "dummyVideo0" || reportBody["reason"] != "stream-capped" {
		t.Errorf("report body = %v, want the video and reason forwarded", reportBody)
	}
}

func TestSiblingSidecarURL(t *testing.T) {
	cases := []struct {
		endpoint, name, want string
	}{
		{"http://h:4417/session", "report", "http://h:4417/report"},
		{"http://h/api/session", "report", "http://h/api/report"},
		// A trailing slash must not shift the sibling down a level.
		{"http://h/api/session/", "report", "http://h/api/report"},
		{"http://h/session?key=K", "report", "http://h/report?key=K"},
	}
	for _, tc := range cases {
		got, err := siblingSidecarURL(tc.endpoint, tc.name)
		if err != nil {
			t.Errorf("siblingSidecarURL(%q,%q) error: %v", tc.endpoint, tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("siblingSidecarURL(%q,%q) = %q, want %q", tc.endpoint, tc.name, got, tc.want)
		}
	}
}

// redactURLsIn guards the messages assembled from wrapped causes, where a signed
// stream URL can arrive inside anything. It runs where those strings are built,
// so the result is safe in a playlist run's NDJSON as well as on the terminal.
func TestRedactURLsIn(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"no urls here":           "no urls here",
		"hostless: https://?x=1": "hostless: <url>",
		`Get "https://rr3---sn-x.googlevideo.com/videoplayback?expire=1&sig=SECRET": EOF`: `Get "https://rr3---sn-x.googlevideo.com": EOF`,
		"re-resolve after refresh: http://127.0.0.1:4417/get_pot?key=hunter2":             "re-resolve after refresh: http://127.0.0.1:4417",
		"two: https://a.example/x and https://b.example/y.":                               "two: https://a.example and https://b.example.",
	}
	for in, want := range cases {
		if got := redactURLsIn(in); got != want {
			t.Errorf("redactURLsIn(%q) = %q, want %q", in, got, want)
		}
	}
	if got := redactURLsIn("pot=X only in https://host/p?pot=X"); strings.Contains(got, "?pot=X") {
		t.Errorf("query string survived: %q", got)
	}
}

// recordSidecarSleeps replaces sidecarSleep for the test and returns the waits
// sidecarCall asked for, without sleeping.
func recordSidecarSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := sidecarSleep
	sidecarSleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sidecarSleep = prev })
	return &waits
}

// sidecarReply is one scripted answer from a fake sidecar.
type sidecarReply struct {
	status     int
	retryAfter string // Retry-After header, when the sidecar states one
	body       string
}

// scriptedSidecar answers each request with the next reply, repeating the last
// one once the script runs out, and counts the requests it served.
func scriptedSidecar(t *testing.T, replies ...sidecarReply) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r := replies[min(int(hits.Add(1))-1, len(replies)-1)]
		if r.retryAfter != "" {
			w.Header().Set("Retry-After", r.retryAfter)
		}
		if r.status != 0 && r.status != http.StatusOK {
			w.WriteHeader(r.status)
		}
		_, _ = io.WriteString(w, r.body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestSidecarCall covers the whole retry policy against a scripted sidecar: how
// many requests a refusal earns, and what it waits first.
func TestSidecarCall(t *testing.T) {
	const okContext = validPlayerContextJSON
	coolDown := sidecarReply{status: http.StatusBadGateway, body: `{"error":"proof cool-down","code":"player-context-failed"}`}

	cases := []struct {
		name string
		// deadline bounds the call; zero means none.
		deadline time.Duration
		replies  []sidecarReply
		wantHits int64
		wantWait []time.Duration
		// check inspects the final error; nil means the call must succeed.
		check func(t *testing.T, err error)
	}{
		{
			name:     "a stated wait is honoured",
			replies:  []sidecarReply{withRetryAfter(coolDown, "25"), {body: okContext}},
			wantHits: 2,
			wantWait: []time.Duration{25 * time.Second},
		},
		{
			name: "a stated wait is read from the body when no header carries one",
			replies: []sidecarReply{
				{status: http.StatusServiceUnavailable, body: `{"error":"no session","code":"no-session","retry_after_seconds":12}`},
				{body: okContext},
			},
			wantHits: 2,
			wantWait: []time.Duration{12 * time.Second},
		},
		{
			name:     "a wait past the cap is reported rather than slept through",
			replies:  []sidecarReply{withRetryAfter(coolDown, "120")},
			wantHits: 1,
			check: func(t *testing.T, err error) {
				sre, ok := errors.AsType[*SidecarResponseError](err)
				if !ok || sre.RetryAfter != 120*time.Second {
					t.Errorf("err = %v, want the stated 120s reported on the refusal", err)
				}
			},
		},
		{
			name: "a wait that cannot fit the deadline is not slept through",
			// 2s plus the one-second retry headroom does not fit a 3s budget.
			deadline: 3 * time.Second,
			replies:  []sidecarReply{withRetryAfter(coolDown, "2")},
			wantHits: 1,
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*SidecarResponseError](err); !ok {
					t.Errorf("err = %v, want the refusal rather than a deadline error", err)
				}
			},
		},
		{
			name:     "a server error earns the quick retry",
			replies:  []sidecarReply{{status: http.StatusInternalServerError, body: "boom"}},
			wantHits: 2,
			wantWait: []time.Duration{sidecarTransientWait},
			check: func(t *testing.T, err error) {
				sre, ok := errors.AsType[*SidecarResponseError](err)
				if !ok || sre.StatusCode != 500 {
					t.Errorf("err = %v, want the second attempt's 500", err)
				}
			},
		},
		{
			// A bare 429 says back off, which is what the CLI's exit 5 tells the
			// user; a 500 ms poke would contradict it.
			name:     "a bare 429 earns none",
			replies:  []sidecarReply{{status: http.StatusTooManyRequests, body: "slow down"}},
			wantHits: 1,
			check:    wantRefusal,
		},
		{
			name:     "a 429 that states a wait earns one",
			replies:  []sidecarReply{{status: http.StatusTooManyRequests, retryAfter: "3", body: "slow down"}, {body: okContext}},
			wantHits: 2,
			wantWait: []time.Duration{3 * time.Second},
		},
		{
			name:     "another client refusal earns none",
			replies:  []sidecarReply{{status: http.StatusUnauthorized, body: "unauthorized"}},
			wantHits: 1,
			check:    wantRefusal,
		},
		{
			name: "a playability verdict earns none",
			replies: []sidecarReply{{status: http.StatusUnprocessableEntity,
				body: `{"error":"video unplayable: Video unavailable","code":"video-unavailable","details":"ERROR"}`}},
			wantHits: 1,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrVideoUnavailable) {
					t.Errorf("err = %v, want the verdict", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			waits := recordSidecarSleeps(t)
			srv, hits := scriptedSidecar(t, tc.replies...)
			p, err := NewSidecarPlayerContextProvider(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.deadline)
				defer cancel()
			}
			_, callErr := p.ProvidePlayerContext(ctx, "dummyVideo0")
			if tc.check == nil {
				if callErr != nil {
					t.Fatalf("call should succeed: %v", callErr)
				}
			} else {
				if callErr == nil {
					t.Fatal("expected a refusal")
				}
				tc.check(t, callErr)
			}
			if got := hits.Load(); got != tc.wantHits {
				t.Errorf("requests = %d, want %d", got, tc.wantHits)
			}
			if !slices.Equal(*waits, tc.wantWait) {
				t.Errorf("waits = %v, want %v", *waits, tc.wantWait)
			}
		})
	}
}

func withRetryAfter(r sidecarReply, v string) sidecarReply {
	r.retryAfter = v
	return r
}

func wantRefusal(t *testing.T, err error) {
	t.Helper()
	if _, ok := errors.AsType[*SidecarResponseError](err); !ok {
		t.Errorf("err = %v, want a SidecarResponseError", err)
	}
}

// TestSidecarCallCancelOutranksRefusal pins PauseBlocked's precedence through
// sidecarCall: a cancelled caller gets the cancellation, not the retryable
// transport failure the cancellation itself produced.
func TestSidecarCallCancelOutranksRefusal(t *testing.T) {
	waits := recordSidecarSleeps(t)
	srv, _ := scriptedSidecar(t, sidecarReply{body: validPlayerContextJSON})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := NewSidecarPlayerContextProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProvidePlayerContext(ctx, "dummyVideo0"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation to outrank the pending failure", err)
	}
	if len(*waits) != 0 {
		t.Errorf("waits = %v, want none", *waits)
	}
}

func TestSidecarCallTransportRetry(t *testing.T) {
	waits := recordSidecarSleeps(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	p, err := NewSidecarPlayerContextProvider(url)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.ProvidePlayerContext(context.Background(), "dummyVideo0")
	if _, ok := errors.AsType[*SidecarError](err); !ok {
		t.Fatalf("err = %v, want a transport failure", err)
	}
	if len(*waits) != 1 || (*waits)[0] != sidecarTransientWait {
		t.Errorf("waits = %v, want one %v wait before the second dial", *waits, sidecarTransientWait)
	}
}

func TestBgutilProviderRetriesOnce(t *testing.T) {
	waits := recordSidecarSleeps(t)
	srv, hits := scriptedSidecar(t,
		sidecarReply{status: http.StatusBadGateway, body: "cool-down"},
		sidecarReply{body: `{"poToken":"tok"}`},
	)
	p, err := NewSidecarPOTokenProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.ProvidePOToken(context.Background(), potoken.Request{Scope: potoken.ScopePlayer, VideoID: "dummyVideo0"})
	if err != nil || got.Token != "tok" {
		t.Fatalf("ProvidePOToken = %+v, %v; want the token on the retry", got, err)
	}
	if hits.Load() != 2 || len(*waits) != 1 {
		t.Errorf("hits = %d, waits = %v; want 2 hits after one wait", hits.Load(), *waits)
	}
}

func TestSidecarClientTimeouts(t *testing.T) {
	tok, err := NewSidecarPOTokenProvider("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := tok.(*bgutilProvider).http.Timeout; got != defaultSidecarTimeout {
		t.Errorf("token client timeout = %v, want %v", got, defaultSidecarTimeout)
	}
	pc, err := NewSidecarPlayerContextProvider("http://127.0.0.1:1", WithSidecarTimeout(90*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	p := pc.(*playerContextProvider)
	if p.http.Timeout != 90*time.Second || p.reporter.http.Timeout != 90*time.Second {
		t.Errorf("option must reach the context client and its reporter: %v, %v", p.http.Timeout, p.reporter.http.Timeout)
	}
	sp, err := NewSidecarSessionProvider("http://127.0.0.1:1", WithSidecarTimeout(-1))
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.(*httpSessionProvider).http.Timeout; got != defaultSidecarTimeout {
		t.Errorf("non-positive option must select the default, got %v", got)
	}
}

// TestPingSidecarWire pins the request a ping sends: one GET on the /ping
// sibling of the configured endpoint, ?strict=true beside whatever query the
// endpoint carried, the key in the header, and JSON asked for.
func TestPingSidecarWire(t *testing.T) {
	var got struct{ method, path, query, key, accept string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.query = r.Method, r.URL.Path, r.URL.RawQuery
		got.key, got.accept = r.Header.Get("X-API-Key"), r.Header.Get("Accept")
		_, _ = io.WriteString(w, `{"ok":true,"probe":"tenant","reason":"ok"}`)
	}))
	defer srv.Close()

	h, err := PingSidecar(context.Background(), srv.URL+"/api/session/?key=K&odd%2Fone", WithSidecarAPIKey("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if !h.OK || h.Probe != "tenant" || h.Reason != "ok" {
		t.Errorf("health = %+v, want the live tenant answer", h)
	}
	if got.method != http.MethodGet || got.path != "/api/ping" {
		t.Errorf("request = %s %s, want GET /api/ping (the sibling of the configured endpoint)", got.method, got.path)
	}
	// strict leads, so it is the value WaxSeal reads, and the configured query
	// follows byte for byte, not re-encoded.
	if want := "strict=true&key=K&odd%2Fone"; got.query != want {
		t.Errorf("query = %q, want %q", got.query, want)
	}
	if got.key != "secret" || got.accept != "application/json" {
		t.Errorf("headers: X-API-Key = %q, Accept = %q", got.key, got.accept)
	}

	// A base URL pings at its root, and no key sends no header.
	got.key = "set"
	if _, err := PingSidecar(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if got.path != "/ping" || got.key != "" {
		t.Errorf("base URL: path = %q, X-API-Key = %q; want /ping and no header", got.path, got.key)
	}
}

// TestPingSidecar covers every answer a daemon can give a strict ping, and that
// each one is asked for exactly once: a probe is a question, not a request a
// retry could make succeed.
func TestPingSidecar(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string // a Retry-After header, when one is sent
		body       string
		want       SidecarHealth
		// wantErr is nil for a healthy answer; otherwise the refusal's status,
		// code, wait, and the start of its reason. A wantErr with status 0 is
		// a malformed 200.
		wantErr *SidecarResponseError
	}{
		{
			name: "live tenant session", status: 200,
			body: `{"ok":true,"probe":"tenant","reason":"ok","attest":"streaming","generation":7}`,
			want: SidecarHealth{OK: true, Probe: "tenant", Reason: "ok"},
		},
		{
			name: "daemon scope, keyless on a keyed daemon", status: 200,
			body: `{"ok":true,"probe":"daemon","reason":"ok","browser_relaunched":false,"keyed":true}`,
			want: SidecarHealth{OK: true, Probe: "daemon", Reason: "ok", Keyed: true},
		},
		{
			// A benign window stays healthy under strict, as the daemon's own
			// status says, and the relaunch it reports is carried.
			name: "no-session after a relaunch", status: 200,
			body: `{"ok":false,"probe":"tenant","reason":"no-session","error":"no attested session","browser_relaunched":true}`,
			want: SidecarHealth{Probe: "tenant", Reason: "no-session", Error: "no attested session", BrowserRelaunched: true},
		},
		{
			name: "busy", status: 200,
			body: `{"ok":false,"probe":"tenant","reason":"busy","error":"the page is held by a request"}`,
			want: SidecarHealth{Probe: "tenant", Reason: "busy", Error: "the page is held by a request"},
		},
		{
			// The loss the daemon confirmed: 503 under strict, with its account.
			name: "probe-failed", status: 503,
			body:    `{"ok":false,"probe":"tenant","reason":"probe-failed","error":"the shared browser missed two probes and was torn down and relaunched","browser_relaunched":true}`,
			want:    SidecarHealth{Probe: "tenant", Reason: "probe-failed", Error: "the shared browser missed two probes and was torn down and relaunched", BrowserRelaunched: true},
			wantErr: &SidecarResponseError{StatusCode: 503, Code: "probe-failed", Reason: "the shared browser missed two probes"},
		},
		{
			// A wait a proxy states on the loss travels with the refusal like
			// any other.
			name: "probe-failed with a stated wait", status: 503, retryAfter: "30",
			body:    `{"ok":false,"probe":"daemon","reason":"probe-failed","error":"no browser answers"}`,
			want:    SidecarHealth{Probe: "daemon", Reason: "probe-failed", Error: "no browser answers"},
			wantErr: &SidecarResponseError{StatusCode: 503, Code: "probe-failed", Reason: "no browser answers", RetryAfter: 30 * time.Second},
		},
		{
			// Older daemons omit the reason word; the body is still a health
			// body, told by its ok field, and ok:true is healthy.
			name: "health body without a reason word", status: 200,
			body: `{"ok":true,"probe":"tenant"}`,
			want: SidecarHealth{OK: true, Probe: "tenant"},
		},
		{
			// One JSON value is read, as every other sidecar answer is; what
			// follows it is not the daemon's answer.
			name: "health body with trailing bytes", status: 200,
			body: `{"ok":true,"probe":"tenant","reason":"ok"}` + "\ntrailing",
			want: SidecarHealth{OK: true, Probe: "tenant", Reason: "ok"},
		},
		{
			// A health body is a success body and gets the success bound, so
			// one padded past the refusal limit still decodes.
			name: "oversized health body", status: 200,
			body: `{"ok":true,"probe":"tenant","reason":"ok","pad":"` + strings.Repeat("x", 70000) + `"}`,
			want: SidecarHealth{OK: true, Probe: "tenant", Reason: "ok"},
		},
		{
			// A foreign envelope that happens to carry a reason key has no ok
			// field, so it is read as the refusal it is, not as a verdict.
			name: "a 503 envelope with a reason key", status: 503,
			body:    `{"reason":"upstream down","code":"bad-gateway","error":"gateway down"}`,
			wantErr: &SidecarResponseError{StatusCode: 503, Code: "bad-gateway", Reason: "gateway down"},
		},
		{
			name: "a 200 that is empty", status: 200,
			body:    ``,
			wantErr: &SidecarResponseError{Reason: "malformed JSON response"},
		},
		{
			// A daemon that predates ?strict answers a confirmed loss with 200;
			// its reason is the verdict the 503 would carry today.
			name: "probe-failed from a pre-strict daemon", status: 200,
			body:    `{"ok":false,"probe":"tenant","reason":"probe-failed","error":"session retired"}`,
			want:    SidecarHealth{Probe: "tenant", Reason: "probe-failed", Error: "session retired"},
			wantErr: &SidecarResponseError{StatusCode: 200, Code: "probe-failed", Reason: "session retired"},
		},
		{
			// A 503 without a health body is an ordinary refusal: a proxy's, or
			// a daemon's envelope. Its code and text are read the usual way.
			name: "a 503 that is not a health body", status: 503,
			body:    `{"error":"upstream down","code":"bad-gateway"}`,
			wantErr: &SidecarResponseError{StatusCode: 503, Code: "bad-gateway", Reason: "upstream down"},
		},
		{
			name: "a 503 that is not JSON", status: 503,
			body:    `<html>gateway timeout</html>`,
			wantErr: &SidecarResponseError{StatusCode: 503},
		},
		{
			name: "wrong key", status: 401,
			body:    `{"error":"invalid or missing API key","code":"unauthorized"}`,
			wantErr: &SidecarResponseError{StatusCode: 401, Code: "unauthorized", Reason: "invalid or missing API key"},
		},
		{
			name: "no /ping", status: 404,
			body:    `{"error":"not found","code":"not-found"}`,
			wantErr: &SidecarResponseError{StatusCode: 404, Code: "not-found", Reason: "not found"},
		},
		{
			// bgutil answers /ping with its uptime and version: it answered, and
			// that is all the ping can say about a daemon with no health body.
			name: "a daemon that is not WaxSeal", status: 200,
			body: `{"server_uptime":12.5,"version":"1.1.0"}`,
			want: SidecarHealth{},
		},
		{
			name: "a 200 that is not JSON", status: 200,
			body:    `OK`,
			wantErr: &SidecarResponseError{Reason: "malformed JSON response"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := scriptedSidecar(t, sidecarReply{status: tc.status, retryAfter: tc.retryAfter, body: tc.body})
			h, err := PingSidecar(context.Background(), srv.URL+"/session")
			if h != tc.want {
				t.Errorf("health = %+v, want %+v", h, tc.want)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("the daemon was asked %d times, want once: a probe earns no retry", n)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			sre, ok := errors.AsType[*SidecarResponseError](err)
			if !ok {
				t.Fatalf("err = %v (%T), want a *SidecarResponseError", err, err)
			}
			if sre.StatusCode != tc.wantErr.StatusCode || sre.Code != tc.wantErr.Code || sre.RetryAfter != tc.wantErr.RetryAfter || !strings.HasPrefix(sre.Reason, tc.wantErr.Reason) {
				t.Errorf("refusal = %+v, want status %d, code %q, wait %v, reason starting %q", sre, tc.wantErr.StatusCode, tc.wantErr.Code, tc.wantErr.RetryAfter, tc.wantErr.Reason)
			}
			if !strings.Contains(sre.Endpoint, "/ping") || sre.Label != "sidecar health endpoint" {
				t.Errorf("refusal names %q at %q, want the health endpoint", sre.Label, sre.Endpoint)
			}
		})
	}
}

// The failed ping's message reads like every other refusal, with the reason
// where a code goes, so a reader of the doctor line sees the verdict before the
// daemon's account of it.
func TestPingSidecarErrorText(t *testing.T) {
	srv, _ := scriptedSidecar(t, sidecarReply{status: 503, body: `{"ok":false,"probe":"tenant","reason":"probe-failed","error":"no browser answers and none could be launched: exit status 1"}`})
	_, err := PingSidecar(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("want the confirmed loss as an error")
	}
	want := "sidecar health endpoint at " + srv.URL + "/ping returned HTTP 503 (probe-failed): no browser answers and none could be launched: exit status 1"
	if got := err.Error(); got != want {
		t.Errorf("err = %q\nwant %q", got, want)
	}
}

// TestPingSidecarTimeout pins the ping's bound. Given no timeout, it is
// allowed WaxSeal's documented probe worst case rather than the 60 s the
// other endpoints get, since the answer that runs long is the verdict it
// exists to fetch; a timeout that is given is the bound, shorter or longer,
// as it is on every other sidecar request; and the caller's context always
// decides how long it will wait.
func TestPingSidecarTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, `{"ok":true,"probe":"tenant","reason":"ok"}`)
	}))
	defer srv.Close()

	h, err := PingSidecar(context.Background(), srv.URL)
	if err != nil || !h.OK {
		t.Fatalf("ping = %+v, %v; want the answer under the default bound", h, err)
	}
	_, err = PingSidecar(context.Background(), srv.URL, WithSidecarTimeout(20*time.Millisecond))
	if _, ok := errors.AsType[*SidecarError](err); !ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the given 20 ms timeout to cut the ping as a connection failure", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := PingSidecar(ctx, srv.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's deadline to cut the ping", err)
	}
}

// A 200 whose body dies mid-read came from a daemon that answered, so it is
// the unusable response sidecarJSON reports for the same thing, not a
// connection failure.
func TestPingSidecarBodyCutMidRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		_, _ = io.WriteString(w, `{"ok":true,"probe":`)
	}))
	defer srv.Close()
	_, err := PingSidecar(context.Background(), srv.URL)
	sre, ok := errors.AsType[*SidecarResponseError](err)
	if !ok || sre.StatusCode != 0 || !strings.HasPrefix(sre.Reason, "malformed JSON response") || sre.Cause == nil {
		t.Fatalf("err = %v (%T), want the malformed-200 refusal carrying the read error", err, err)
	}
}

func TestPingSidecarUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()
	_, err := PingSidecar(context.Background(), addr)
	se, ok := errors.AsType[*SidecarError](err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *SidecarError for a closed port", err, err)
	}
	// The message names the health endpoint, redacted to its path: the strict
	// query is not a secret, but every endpoint is printed the same way.
	if want := "sidecar health endpoint unreachable at " + addr + "/ping: "; !strings.HasPrefix(se.Error(), want) {
		t.Errorf("err = %q, want it to start %q", se.Error(), want)
	}
}

// A ping follows no redirect either: the key must stay bound to the endpoint
// it was configured for.
func TestPingSidecarDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"probe":"tenant","reason":"ok"}`)
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/ping", http.StatusFound)
	}))
	defer srv.Close()

	_, err := PingSidecar(context.Background(), srv.URL, WithSidecarAPIKey("secret"))
	sre, ok := errors.AsType[*SidecarResponseError](err)
	if !ok || sre.StatusCode != http.StatusFound || !strings.HasPrefix(sre.Reason, "redirected to ") {
		t.Fatalf("err = %v, want a 302 refusal naming where it pointed", err)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("redirect target contacted %d times; the key must stay bound to the endpoint", n)
	}
}

func TestPingSidecarRejectsBadURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://127.0.0.1:4416", "127.0.0.1:4416", "http://"} {
		if _, err := PingSidecar(context.Background(), bad); err == nil {
			t.Errorf("PingSidecar(%q) accepted the URL", bad)
		}
	}
}
