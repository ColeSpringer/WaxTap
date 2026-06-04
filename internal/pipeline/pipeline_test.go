package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/colespringer/waxtap/cut"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/waxerr"
)

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

func synthSine(t *testing.T, dir, name string, seconds int, encoder string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration=" + strconv.Itoa(seconds),
		"-af", "volume=-6dB", "-ac", "2", "-c:a", encoder, out,
	}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("synth sine: %v: %s", err, b)
	}
	return out
}

func probeDuration(t *testing.T, r *transcode.Runner, path string) time.Duration {
	t.Helper()
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return pr.Format.Duration
}

func recordStages() (func(Stage), *[]Stage) {
	var seen []Stage
	return func(s Stage) { seen = append(seen, s) }, &seen
}

func TestRunMeasureOnly(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.flac")

	emit, seen := recordStages()
	res, err := Run(context.Background(), r, in, out, Spec{
		Loudness: &Loudness{Apply: false},
	}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputPath != "" {
		t.Errorf("measure-only OutputPath = %q, want empty", res.OutputPath)
	}
	if !res.LoudnessMeasured || res.InputLoudness == nil {
		t.Errorf("measure-only result = %+v", res)
	}
	if res.LoudnessApplied || res.Transcoded || res.Cut {
		t.Errorf("measure-only should do no output work: %+v", res)
	}
	if fileExists(out) {
		t.Error("measure-only must not write output")
	}
	assertHasStage(t, *seen, StageProbing)
	assertHasStage(t, *seen, StageAnalyzing)
}

func TestRunTranscode(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.mp3")

	emit, seen := recordStages()
	res, err := Run(context.Background(), r, in, out, Spec{Codec: transcode.CodecMP3}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputPath != out || !res.Transcoded || res.OutputCodec != transcode.CodecMP3 {
		t.Errorf("transcode result = %+v", res)
	}
	if !fileExists(out) {
		t.Error("transcode output missing")
	}
	if res.SourceCodec != "flac" {
		t.Errorf("SourceCodec = %q, want flac", res.SourceCodec)
	}
	assertHasStage(t, *seen, StageTranscoding)
}

func TestRunCutFusedTranscode(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Remove: []cut.Range{{Start: time.Second, End: 2 * time.Second}},
		Codec:  transcode.CodecFLAC,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Cut || !res.Transcoded {
		t.Errorf("cut+transcode result = %+v", res)
	}
	if d := probeDuration(t, r, out); d < 2500*time.Millisecond || d > 3500*time.Millisecond {
		t.Errorf("output duration = %v, want ~3s (4s - 1s cut)", d)
	}
}

func TestRunRemux(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")

	// An explicit copy remux runs ffmpeg -c copy and produces output.
	out := filepath.Join(dir, "out.mka")
	res, err := Run(context.Background(), r, in, out, Spec{Codec: transcode.CodecCopy, Remux: true}, nil)
	if err != nil {
		t.Fatalf("Run remux: %v", err)
	}
	if res.OutputPath != out || res.Transcoded {
		t.Errorf("remux result = %+v, want OutputPath set and Transcoded false", res)
	}
	if !fileExists(out) {
		t.Error("remux wrote no output")
	}

	// Copy without Remux is a no-op: no output, deliver the source unchanged.
	out2 := filepath.Join(dir, "out2.mka")
	res2, err := Run(context.Background(), r, in, out2, Spec{Codec: transcode.CodecCopy}, nil)
	if err != nil {
		t.Fatalf("Run no-op: %v", err)
	}
	if res2.OutputPath != "" || fileExists(out2) {
		t.Errorf("copy without Remux should be a no-op, got OutputPath=%q exists=%v", res2.OutputPath, fileExists(out2))
	}
}

func TestRunNoOpCutStillTranscodes(t *testing.T) {
	// SponsorBlock returned nothing useful (a zero-length / fully-clamped range),
	// but a transcode was requested: the transcode must still run.
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.mp3")

	res, err := Run(context.Background(), r, in, out, Spec{
		Remove: []cut.Range{{Start: 10 * time.Second, End: 20 * time.Second}}, // beyond EOF: clamps away
		Codec:  transcode.CodecMP3,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Cut {
		t.Error("Cut should be false when nothing was removed")
	}
	if !res.Transcoded || !fileExists(out) {
		t.Errorf("transcode must still run: %+v", res)
	}
}

func TestRunCutLoudnessApply(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Remove:   []cut.Range{{Start: time.Second, End: 2 * time.Second}},
		Codec:    transcode.CodecFLAC,
		Loudness: &Loudness{Apply: true, Target: -14},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Cut || !res.LoudnessApplied || res.OutputLoudness == nil {
		t.Fatalf("cut+apply result = %+v", res)
	}
	// Measured over the post-cut audio, normalized to ~-14 LUFS.
	if got := res.OutputLoudness.IntegratedLUFS; got < -16 || got > -12 {
		t.Errorf("output loudness = %v, want within 2 LU of -14", got)
	}
	if d := probeDuration(t, r, out); d < 2500*time.Millisecond || d > 3500*time.Millisecond {
		t.Errorf("output duration = %v, want ~3s", d)
	}
}

func TestRunLoudnessApplyWithoutTranscodeRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 1, "flac")
	out := filepath.Join(dir, "out.flac")

	_, err := Run(context.Background(), r, in, out, Spec{
		Codec:    transcode.CodecCopy,
		Loudness: &Loudness{Apply: true, Target: -14},
	}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("apply+copy err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestRunWholeTrackRemovedRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.flac")

	_, err := Run(context.Background(), r, in, out, Spec{
		Remove: []cut.Range{{Start: 0, End: time.Hour}},
		Codec:  transcode.CodecFLAC,
	}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("whole-track removal err = %v, want ErrIncompatibleSpec", err)
	}
}

func assertHasStage(t *testing.T, seen []Stage, want Stage) {
	t.Helper()
	for _, s := range seen {
		if s == want {
			return
		}
	}
	t.Errorf("stage %v not emitted; saw %v", want, seen)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
