package main

import (
	"strings"
	"testing"
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
