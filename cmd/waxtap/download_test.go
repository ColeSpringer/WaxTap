package main

import (
	"errors"
	"testing"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

func TestResolveValidatesProcessSpec(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
	}{
		{"negative bitrate", map[string]string{"transcode": "mp3", "bitrate": "-1"}},
		{"out-of-range loudness target", map[string]string{"transcode": "flac", "normalize": "true", "loudness-target": "50"}},
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

// TestBuildProcessSpecNormalizeRequiresEncode covers flag combinations before
// any download starts.
func TestBuildProcessSpecNormalizeRequiresEncode(t *testing.T) {
	cases := []struct {
		name      string
		df        *downloadFlags
		wantError bool
	}{
		{"normalize without transcode", &downloadFlags{normalize: true, loudTarget: -14}, true},
		{"normalize + copy", &downloadFlags{normalize: true, transcode: "copy", loudTarget: -14}, true},
		{"normalize + flac", &downloadFlags{normalize: true, transcode: "flac", loudTarget: -14}, false},
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
