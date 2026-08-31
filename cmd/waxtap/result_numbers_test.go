package main

import (
	"bytes"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
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

// TestRenderLoudnessUnmeasurableNote checks that an unusable loudness figure is
// followed by the reason the library gave for it, on whichever side it applies
// to, so an "n/a" is never left to be read as a verified normalization.
func TestRenderLoudnessUnmeasurableNote(t *testing.T) {
	const silence = "the audio is digital silence"
	for _, tc := range []struct {
		name     string
		input    float64
		output   float64
		warnings []waxtap.Warning
		want     string // substring the note must carry, "" for no note at all
		reject   string // substring the note must not carry
	}{
		{
			name:     "silent output echoes its cause",
			input:    -20,
			output:   math.Inf(-1),
			warnings: []waxtap.Warning{{Code: waxtap.WarnLoudnessUnmeasurable, Detail: "output integrated loudness could not be measured: " + silence}},
			want:     silence,
			reject:   "too short",
		},
		{
			name:   "unexplained output falls back without guessing",
			input:  -20,
			output: math.NaN(),
			want:   "output integrated loudness could not be measured",
			reject: silence,
		},
		{
			name:   "finite output says nothing",
			input:  -20,
			output: -14.0,
			reject: "could not be measured",
		},
		{
			name:     "silent input is explained under its own line",
			input:    math.Inf(-1),
			output:   -14.0,
			warnings: []waxtap.Warning{{Code: waxtap.WarnLoudnessUnmeasurable, Detail: "input integrated loudness could not be measured: " + silence}},
			want:     "input integrated loudness could not be measured: " + silence,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
			renderLoudness(env, &waxtap.Result{
				Warnings: tc.warnings,
				Loudness: &waxtap.LoudnessResult{
					Input:  &waxtap.LoudnessInfo{IntegratedLUFS: tc.input},
					Output: &waxtap.LoudnessInfo{IntegratedLUFS: tc.output},
					Target: -14,
				},
			})
			got := out.String()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("output missing %q:\n%s", tc.want, got)
			}
			if tc.reject != "" && strings.Contains(got, tc.reject) {
				t.Errorf("output must not carry %q:\n%s", tc.reject, got)
			}
		})
	}
}
