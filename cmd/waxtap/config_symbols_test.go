package main

import (
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
)

// TestTranslateConfigSymbols covers each Go option field name that may appear in
// a waxtap configuration error, including conflicts pre-validated by config.go.
func TestTranslateConfigSymbols(t *testing.T) {
	goSymbols := []string{
		"ProfileOverridePath", "ChromeMajor", "PerHostQPS", "Cooldown", "Options.",
		"VisitorData", "POTokenProvider", "PlayerContextProvider",
	}
	cases := []struct {
		in        string
		wantFlags []string
	}{
		{"invalid ChromeMajor 1000: must be 0", []string{"--chrome-major"}},
		{"ChromeMajor and ProfileOverridePath are mutually exclusive", []string{"--chrome-major", "--profile-override"}},
		{"Client and ProfileOverridePath are mutually exclusive", []string{"--client and", "--profile-override"}},
		{"invalid Cooldown -1s: must be >= 0", []string{"--cooldown"}},
		{"invalid PerHostQPS -1: must be a finite value >= 0", []string{"--qps"}},
		{`set Options.Client (e.g. "web") or a single-client ProfileOverridePath`, []string{"--client", "--profile-override"}},
		{"PlayerContextProvider requires a POTokenProvider", []string{"--player-context-url", "--potoken-url"}},
		{"adopted Session requires a non-empty VisitorData", []string{"--visitor-data"}},
	}
	for _, tc := range cases {
		got := translateConfigSymbols(tc.in)
		for _, f := range tc.wantFlags {
			if !strings.Contains(got, f) {
				t.Errorf("translate(%q) = %q, want flag %q", tc.in, got, f)
			}
		}
		for _, sym := range goSymbols {
			if strings.Contains(got, sym) {
				t.Errorf("translate(%q) = %q, still contains %q", tc.in, got, sym)
			}
		}
	}
}

// TestConfigSymbolTranslation verifies that reachable waxtap.New configuration
// errors use CLI flag names when rendered.
func TestConfigSymbolTranslation(t *testing.T) {
	goSymbols := []string{"ProfileOverridePath", "ChromeMajor", "PerHostQPS", "Options.", "Cooldown"}

	cases := []struct {
		name      string
		opts      waxtap.Options
		wantFlags []string
	}{
		{
			name:      "chrome-major range",
			opts:      waxtap.Options{ChromeMajor: 1000},
			wantFlags: []string{"--chrome-major"},
		},
		{
			name:      "chrome-major vs profile-override",
			opts:      waxtap.Options{ChromeMajor: 80, ProfileOverridePath: "/tmp/x.json"},
			wantFlags: []string{"--chrome-major", "--profile-override"},
		},
		{
			name:      "client vs profile-override",
			opts:      waxtap.Options{Client: "web", ProfileOverridePath: "/tmp/x.json"},
			wantFlags: []string{"--client", "--profile-override"},
		},
		{
			name:      "cooldown range",
			opts:      waxtap.Options{Politeness: waxtap.Politeness{Cooldown: -1}},
			wantFlags: []string{"--cooldown"},
		},
		{
			name:      "qps range",
			opts:      waxtap.Options{Politeness: waxtap.Politeness{PerHostQPS: -1}},
			wantFlags: []string{"--qps"},
		},
		{
			name:      "session needs uniform client chain",
			opts:      waxtap.Options{Session: &waxtap.POTokenSession{VisitorData: "abc"}},
			wantFlags: []string{"--client", "--profile-override"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := waxtap.New(tc.opts)
			if err == nil {
				t.Fatalf("New(%+v) = nil error, want a config conflict", tc.opts)
			}
			// Translating the displayed string must not disturb the exit-2 mapping.
			if got := exitCodeFor(err); got != 2 {
				t.Errorf("exitCodeFor = %d, want 2 (invalid-config)", got)
			}
			msg := friendlyError(err)
			for _, flag := range tc.wantFlags {
				if !strings.Contains(msg, flag) {
					t.Errorf("message %q does not name %q", msg, flag)
				}
			}
			for _, sym := range goSymbols {
				if strings.Contains(msg, sym) {
					t.Errorf("message %q still contains Go symbol %q", msg, sym)
				}
			}
		})
	}
}
