package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/spf13/cobra"
)

// TestResolveValidatesOutputTemplate checks that malformed templates fail during
// flag resolution, before download or playlist work starts.
func TestResolveValidatesOutputTemplate(t *testing.T) {
	df := &downloadFlags{}
	cmd := &cobra.Command{Use: "download"}
	bindDownloadFlags(cmd, df)
	mustSet(t, cmd, "output-template", "{artist}")
	err := df.resolve(cmd, testResolveEnv())
	if !isUsageError(err) || !strings.Contains(err.Error(), "output-template") {
		t.Fatalf("resolve err = %v, want an --output-template usage error", err)
	}
}

func TestResolveValidatesProcessSpec(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
	}{
		{"negative bitrate", map[string]string{"format": "mp3", "bitrate": "-1"}},
		{"out-of-range loudness target", map[string]string{"format": "flac", "normalize": "true", "loudness-target": "50"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			df := &downloadFlags{}
			cmd := &cobra.Command{Use: "download"}
			bindDownloadFlags(cmd, df)
			for name, val := range tt.set {
				mustSet(t, cmd, name, val)
			}
			if err := df.resolve(cmd, testResolveEnv()); !errors.Is(err, waxtap.ErrIncompatibleSpec) {
				t.Fatalf("resolve err = %v, want ErrIncompatibleSpec", err)
			}
		})
	}
}

// TestBuildCutSpecValidatesCutModeWithoutRanges ensures invalid input fails
// before extraction, even when no cut ranges are configured.
func TestBuildCutSpecValidatesCutModeWithoutRanges(t *testing.T) {
	df := &downloadFlags{cutMode: "bogus"} // no ranges, no SponsorBlock
	if _, err := df.buildCutSpec(); err == nil {
		t.Fatal("buildCutSpec err = nil, want a cut-mode validation error before the no-cut early return")
	}
	// No cut work still yields a nil spec.
	if cs, err := (&downloadFlags{}).buildCutSpec(); err != nil || cs != nil {
		t.Errorf("buildCutSpec(empty) = (%v, %v), want (nil, nil)", cs, err)
	}
}

func TestBuildCutSpecValidatesSponsorErrorWithoutRanges(t *testing.T) {
	df := &downloadFlags{cutMode: "smart", sbOnError: "bogus"}
	_, err := df.buildCutSpec()
	if err == nil {
		t.Fatal("buildCutSpec err = nil, want a --sponsorblock-on-error validation error before the no-cut early return")
	}
	if !isUsageError(err) {
		t.Errorf("err = %#v, want a usageError (exit 2)", err)
	}
}

// TestMeasureSpecCarriesTarget guards the JSON contract: a measure-only run
// reports the configured loudness target instead of zero.
func TestMeasureSpecCarriesTarget(t *testing.T) {
	df := &downloadFlags{measure: true, loudTarget: -16}
	ls, err := df.buildLoudnessSpec()
	if err != nil {
		t.Fatal(err)
	}
	if ls == nil || ls.Mode != waxtap.LoudnessMeasureOnly {
		t.Fatalf("buildLoudnessSpec = %+v, want a measure-only spec", ls)
	}
	if ls.Target != -16 {
		t.Errorf("measure-only Target = %v, want -16 (carried into --json output)", ls.Target)
	}
}

// TestBuildProcessSpecNormalizeRequiresEncode covers flag combinations before
// any download starts.
func TestBuildProcessSpecNormalizeRequiresEncode(t *testing.T) {
	cases := []struct {
		name      string
		df        *downloadFlags
		wantError bool
	}{
		{"normalize without format", &downloadFlags{normalize: true, loudTarget: -14}, true},
		{"normalize + copy", &downloadFlags{normalize: true, format: "copy", loudTarget: -14}, true},
		{"normalize + flac", &downloadFlags{normalize: true, format: "flac", loudTarget: -14}, false},
		{"measure only", &downloadFlags{measure: true}, false},
		{"keep source", &downloadFlags{}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.df.buildProcessSpec()
			if tt.wantError && err == nil {
				t.Error("expected an error")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
