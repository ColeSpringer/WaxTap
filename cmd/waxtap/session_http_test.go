package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseNetscapeCookies(t *testing.T) {
	// A real-world-ish file: header comment, a blank line, a normal cookie, an
	// #HttpOnly_ cookie, and a malformed short line that must be skipped.
	const content = "# Netscape HTTP Cookie File\n" +
		"\n" +
		".youtube.com\tTRUE\t/\tFALSE\t1799999999\tPREF\tval123\n" +
		"#HttpOnly_.youtube.com\tTRUE\t/\tTRUE\t0\tVISITOR_INFO1_LIVE\tabc\n" +
		"# a comment line\n" +
		"too\tfew\tfields\n"
	path := filepath.Join(t.TempDir(), "cookies.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cookies, err := parseNetscapeCookies(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cookies) != 2 {
		t.Fatalf("got %d cookies, want 2 (malformed/comment lines skipped): %+v", len(cookies), cookies)
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

	sess, err := newHTTPSessionProvider(srv.URL).ProvideSession(context.Background())
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
	sess, err := newHTTPSessionProvider(srv.URL).ProvideSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sess.VisitorData != "CgtX%3D%3D" || len(sess.Cookies) != 1 || !sess.Cookies[0].HttpOnly {
		t.Errorf("camelCase not accepted: vd=%q cookies=%+v", sess.VisitorData, sess.Cookies)
	}
}

func TestHTTPSessionProvider_EmptyVisitorDataErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"visitorData":"","cookies":[]}`))
	}))
	defer srv.Close()
	if _, err := newHTTPSessionProvider(srv.URL).ProvideSession(context.Background()); err == nil {
		t.Fatal("expected an error for an empty visitorData")
	}
}

func TestHTTPSessionProvider_RetriesOnceThenFails(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := newHTTPSessionProvider(srv.URL).ProvideSession(ctx); err == nil {
		t.Fatal("expected failure after retries")
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2 (one retry)", hits)
	}
}
