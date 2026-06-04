package main

import "testing"

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
