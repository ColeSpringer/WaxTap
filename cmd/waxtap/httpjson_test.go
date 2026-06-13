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
