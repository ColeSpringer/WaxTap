package main

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2"
)

// TestResultJSONOutputFormatNumbers checks that a transcoded YouTube result emits
// real output numbers (no longer zero) through the --json DTO.
func TestResultJSONOutputFormatNumbers(t *testing.T) {
	res := &waxtap.Result{
		SourceKind:   waxtap.SourceYouTube,
		Transcoded:   true,
		SourceFormat: waxtap.Format{Codec: "opus", Extension: "webm"},
		OutputFormat: waxtap.Format{Codec: "flac", Extension: "flac", SampleRate: 48000, Channels: 2, Bitrate: 700000, ContentLength: 5_000_000, Duration: 2 * time.Second},
	}
	of, ok := resultToJSON(res).OutputFormat.(formatJSON)
	if !ok {
		t.Fatalf("outputFormat type = %T, want formatJSON", resultToJSON(res).OutputFormat)
	}
	if of.SampleRate != 48000 || of.Channels != 2 || of.Bitrate != 700000 || of.ContentLength != 5_000_000 {
		t.Errorf("outputFormat numbers = %+v, want the non-zero values", of)
	}
	if of.DurationSeconds != 2 {
		t.Errorf("outputFormat durationSeconds = %v, want 2", of.DurationSeconds)
	}
}

// TestRenderLoudnessSubGateNote checks that a non-finite output loudness (a clip
// too short to gate) is flagged so the normalize result is not read as verified.
func TestRenderLoudnessSubGateNote(t *testing.T) {
	cases := []struct {
		name   string
		output float64
		want   bool
	}{
		{"NaN output", math.NaN(), true},
		{"negative inf output", math.Inf(-1), true},
		{"finite output", -14.0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
			renderLoudness(env, &waxtap.LoudnessResult{
				Input:  &waxtap.LoudnessInfo{IntegratedLUFS: -20},
				Output: &waxtap.LoudnessInfo{IntegratedLUFS: tc.output},
				Target: -14,
			})
			if got := strings.Contains(out.String(), "too short to gate"); got != tc.want {
				t.Errorf("note present = %v, want %v; output:\n%s", got, tc.want, out.String())
			}
		})
	}
}
