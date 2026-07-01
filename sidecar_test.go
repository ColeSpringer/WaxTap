package waxtap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/potoken"
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

func TestSidecarReason(t *testing.T) {
	if got := sidecarReason(strings.NewReader(`{"error":"bad scope"}`)); got != "bad scope" {
		t.Errorf("reason from error field = %q", got)
	}
	if got := sidecarReason(strings.NewReader(`{"message":"try later"}`)); got != "try later" {
		t.Errorf("reason from message field = %q", got)
	}
	// Non-JSON and fieldless bodies must not be echoed.
	if got := sidecarReason(strings.NewReader("<html>secret token</html>")); got != "" {
		t.Errorf("reason from HTML = %q, want empty (no leak)", got)
	}
	if got := sidecarReason(strings.NewReader(`{"other":"x"}`)); got != "" {
		t.Errorf("reason from fieldless JSON = %q, want empty", got)
	}
}

func TestBgutilProviderPlayerScope(t *testing.T) {
	var gotBinding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_pot" {
			t.Errorf("path = %q, want /get_pot", r.URL.Path)
		}
		var req bgutilRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		gotBinding = req.ContentBinding
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
	var gotBinding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req bgutilRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotBinding = req.ContentBinding
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
  "client_version": "2.20260606.02.00",
  "title": "Big Buck Bunny",
  "author": "Blender",
  "length_seconds": 634,
  "audio_formats": [
    {"itag":251,"lmt":"1719185012384481","xtags":"","mime_type":"audio/webm; codecs=\"opus\"","bitrate":143452,"audio_channels":2,"audio_sample_rate":48000,"content_length":9700000,"approx_duration_ms":634624}
  ]
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
			"user_agent": "Mozilla/5.0 ignored",
			"client_version": "2.x",
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
}

// TestHTTPSessionProviderCamelCase confirms the documented camelCase variant is
// still accepted (visitorData/httpOnly), so the contract does not break on casing.
func TestHTTPSessionProviderCamelCase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"visitorData":"CgtX%3D%3D","cookies":[{"name":"PREF","value":"p","domain":".youtube.com","httpOnly":true}]}`))
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
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := NewSidecarSessionProvider(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ProvideSession(ctx); err == nil {
		t.Fatal("expected failure after retries")
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2 (one retry)", hits)
	}
}
