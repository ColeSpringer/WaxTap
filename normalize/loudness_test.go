package normalize

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/waxerr"
)

// A loudnorm analysis stderr sample: ffmpeg banner/info noise (including a decoy
// brace pair) followed by the real JSON block, exactly as the filter prints it.
const loudnormStderr = `ffmpeg version 7.1.1 Copyright (c) the FFmpeg developers
  configuration: --prefix=/usr --enable-gpl
[lavfi @ 0x5500] decoy line {not: real json} ignore me
[Parsed_loudnorm_0 @ 0x556000]
{
	"input_i" : "-15.71",
	"input_tp" : "-1.49",
	"input_lra" : "5.90",
	"input_thresh" : "-26.06",
	"output_i" : "-23.01",
	"output_tp" : "-2.00",
	"output_lra" : "5.40",
	"output_thresh" : "-33.65",
	"normalization_type" : "dynamic",
	"target_offset" : "-0.99"
}
`

func TestParseLoudness(t *testing.T) {
	l, err := parseLoudness([]byte(loudnormStderr))
	if err != nil {
		t.Fatalf("parseLoudness: %v", err)
	}
	want := Loudness{
		IntegratedLUFS: -15.71,
		TruePeakDBTP:   -1.49,
		LRA:            5.90,
		Threshold:      -26.06,
		Offset:         -0.99,
	}
	if l != want {
		t.Errorf("parseLoudness =\n  %+v\nwant\n  %+v", l, want)
	}
}

func TestParseLoudnessNoJSON(t *testing.T) {
	if _, err := parseLoudness([]byte("just info, no json here")); err == nil {
		t.Fatal("parseLoudness(no json) = nil error, want error")
	}
}

func TestLastJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"plain", `{"a":1}`, `{"a":1}`, false},
		{"trailing-noise", "noise\n{\"a\":1}\nmore", `{"a":1}`, false},
		{"picks-last", `{"first":1} text {"second":2}`, `{"second":2}`, false},
		{"no-open", "}only close", "", true},
		{"no-brace", "nothing here", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := lastJSONObject([]byte(c.in))
			if c.err {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestApplyFilter(t *testing.T) {
	m := Loudness{IntegratedLUFS: -15.71, TruePeakDBTP: -1.49, LRA: 5.90, Threshold: -26.06, Offset: -0.99}
	got := ApplyFilter(-14, m)

	if !strings.HasPrefix(got, "loudnorm=") {
		t.Fatalf("filter %q does not start with loudnorm=", got)
	}
	for _, want := range []string{
		"I=-14.00",
		"TP=-1.00", // defaultTruePeak
		"measured_I=-15.71",
		"measured_TP=-1.49",
		"measured_LRA=5.90",
		"measured_thresh=-26.06",
		"offset=-0.99",
		"linear=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("filter %q missing %q", got, want)
		}
	}
}

func TestApplyFilterNonFinite(t *testing.T) {
	// A silent track measures input_i=-inf / target_offset=inf; ApplyFilter must
	// fall back to a valid pass-through, not an out-of-range loudnorm that ffmpeg
	// rejects ("Value -inf for parameter 'measured_I' out of range").
	m := Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1), LRA: 0, Threshold: -70, Offset: math.Inf(1)}
	if got := ApplyFilter(-14, m); got != "anull" {
		t.Errorf("ApplyFilter(non-finite) = %q, want anull", got)
	}
}

func TestLoudnessFinite(t *testing.T) {
	finite := Loudness{IntegratedLUFS: -15.71, TruePeakDBTP: -1.49, LRA: 5.90, Threshold: -26.06, Offset: -0.99}
	if !finite.Finite() {
		t.Error("a normal measurement should be Finite")
	}
	for _, m := range []Loudness{
		{IntegratedLUFS: math.Inf(-1)},
		{TruePeakDBTP: math.Inf(-1)},
		{Offset: math.Inf(1)},
		{LRA: math.NaN()},
	} {
		if m.Finite() {
			t.Errorf("%+v should not be Finite", m)
		}
	}
}

func newTestRunner(t *testing.T) *transcode.Runner {
	t.Helper()
	r, err := transcode.NewRunner(transcode.RunnerConfig{ShutdownGrace: 2 * time.Second})
	if err != nil {
		if errors.Is(err, waxerr.ErrFFmpegNotFound) {
			t.Skip("ffmpeg/ffprobe not installed")
		}
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// synthSine writes a steady stereo sine of the given length, attenuated so its
// loudness sits in a realistic range to normalize from.
func synthSine(t *testing.T, dir string, seconds int) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, "in.wav")
	dur := time.Duration(seconds) * time.Second
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration=" + dur.Truncate(time.Second).String(),
		"-af", "volume=-6dB",
		"-ac", "2",
		out,
	}
	if b, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("synth sine: %v: %s", err, b)
	}
	return out
}

// synthSilence writes a digitally silent stereo wav of the given length, for
// which loudnorm reports non-finite measurements.
func synthSilence(t *testing.T, dir string, seconds int) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, "silent.wav")
	dur := time.Duration(seconds) * time.Second
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo",
		"-t", dur.Truncate(time.Second).String(),
		out,
	}
	if b, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("synth silence: %v: %s", err, b)
	}
	return out
}

func TestMeasure_Integration(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, 3)

	l, err := Measure(context.Background(), r, in, nil)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if math.IsNaN(l.IntegratedLUFS) || math.IsInf(l.IntegratedLUFS, 0) {
		t.Fatalf("integrated loudness not finite: %v", l.IntegratedLUFS)
	}
	// A steady tone is neither silent nor above full scale.
	if l.IntegratedLUFS >= 0 || l.IntegratedLUFS < -70 {
		t.Errorf("integrated loudness = %v LUFS, want a plausible negative value", l.IntegratedLUFS)
	}
	if math.IsNaN(l.TruePeakDBTP) || math.IsInf(l.TruePeakDBTP, 0) {
		t.Errorf("true peak not finite: %v", l.TruePeakDBTP)
	}
}

func TestApply_Integration(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, 3)

	const target = -14.0

	// Measure, then fuse the loudnorm apply filter into a single FLAC encode.
	measured, err := Measure(context.Background(), r, in, nil)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	out := filepath.Join(dir, "out.flac")
	if _, err := r.Transcode(context.Background(), in, out, transcode.Spec{
		Codec:   transcode.CodecFLAC,
		Filters: []string{ApplyFilter(target, measured)},
	}); err != nil {
		t.Fatalf("normalizing transcode: %v", err)
	}

	// The lossless output should measure close to the target.
	got, err := Measure(context.Background(), r, out, nil)
	if err != nil {
		t.Fatalf("Measure(out): %v", err)
	}
	if diff := math.Abs(got.IntegratedLUFS - target); diff > 2.0 {
		t.Errorf("normalized loudness = %.2f LUFS, want within 2 LU of %.1f (off by %.2f)", got.IntegratedLUFS, target, diff)
	}
}

func TestApply_SilentTrack(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	silent := synthSilence(t, dir, 3)

	measured, err := Measure(context.Background(), r, silent, nil)
	if err != nil {
		t.Fatalf("Measure(silence): %v", err)
	}
	if measured.Finite() {
		t.Fatalf("silence should measure non-finite, got %+v", measured)
	}

	// Normalizing a silent track must succeed (pass-through), not fail on an
	// out-of-range loudnorm filter built from -inf measurements.
	out := filepath.Join(dir, "out.flac")
	if _, err := r.Transcode(context.Background(), silent, out, transcode.Spec{
		Codec:   transcode.CodecFLAC,
		Filters: []string{ApplyFilter(-14, measured)},
	}); err != nil {
		t.Fatalf("normalizing a silent track should succeed, got: %v", err)
	}
	if _, err := r.Probe(context.Background(), out); err != nil {
		t.Errorf("output is not a valid audio file: %v", err)
	}
}
