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

// synthSurround writes a steady sine upmixed to 5.1, for downmix tests.
func synthSurround(t *testing.T, dir, name string, seconds int, encoder string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration=" + strconv.Itoa(seconds),
		"-af", "volume=-6dB", "-ac", "6", "-c:a", encoder, out,
	}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("synth surround: %v: %s", err, b)
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

func probeChannels(t *testing.T, r *transcode.Runner, path string) int {
	t.Helper()
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	a, _ := pr.AudioStream()
	return a.Channels
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

// TestRunPopulatesOutputProbe checks that a run producing a file leaves an
// OutputProbe for the caller, while a measure-only run (no output) leaves it nil.
func TestRunPopulatesOutputProbe(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")

	t.Run("transcode populates the probe", func(t *testing.T) {
		out := filepath.Join(dir, "out.mp3")
		res, err := Run(context.Background(), r, in, out, Spec{Codec: transcode.CodecMP3, Bitrate: 128000}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.OutputProbe == nil {
			t.Fatal("OutputProbe = nil, want a probe of the written output")
		}
		a, ok := res.OutputProbe.AudioStream()
		if !ok || a.SampleRate <= 0 || a.Channels <= 0 {
			t.Errorf("OutputProbe audio = %+v (ok=%v), want a positive sample rate and channel count", a, ok)
		}
		if res.OutputProbe.Format.Size <= 0 {
			t.Errorf("OutputProbe size = %d, want > 0", res.OutputProbe.Format.Size)
		}
	})

	t.Run("measure-only leaves it nil", func(t *testing.T) {
		out := filepath.Join(dir, "measured.flac")
		res, err := Run(context.Background(), r, in, out, Spec{Loudness: &Loudness{Apply: false}}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.OutputProbe != nil {
			t.Errorf("measure-only OutputProbe = %+v, want nil (no output written)", res.OutputProbe)
		}
	})
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

func TestRunRemuxExtensionlessInfersContainer(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 1, "flac")

	// Extensionless and .copy destinations infer a container from the source codec.
	for _, out := range []string{filepath.Join(dir, "out"), filepath.Join(dir, "out.copy")} {
		res, err := Run(context.Background(), r, in, out, Spec{Codec: transcode.CodecCopy, Remux: true}, nil)
		if err != nil {
			t.Fatalf("remux to %q = %v, want success (inferred container)", out, err)
		}
		if res.Transcoded {
			t.Errorf("remux to %q reported a re-encode; want a stream copy", out)
		}
		if !fileExists(out) {
			t.Errorf("remux to %q wrote no output", out)
		}
	}
}

func TestRunCopyCutWithoutContainerRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")

	// The removal creates two copied segments and exercises the multi-range path.
	for _, out := range []string{filepath.Join(dir, "mytrack"), filepath.Join(dir, "mytrack.copy")} {
		_, err := Run(context.Background(), r, in, out, Spec{
			Codec:   transcode.CodecCopy,
			CutMode: cut.ModeSmart,
			Remove:  []cut.Range{{Start: 800 * time.Millisecond, End: 1200 * time.Millisecond}},
		}, nil)
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("copy cut to %q = %v, want ErrIncompatibleSpec", out, err)
		}
		if fileExists(out) {
			t.Errorf("copy cut to %q wrote output despite rejection", out)
		}
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

func TestRunDownmixSurroundToStereo(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSurround(t, dir, "in.flac", 2, "flac")
	if got := probeChannels(t, r, in); got != 6 {
		t.Skipf("synth produced %d channels, want 6", got)
	}
	out := filepath.Join(dir, "out.flac")

	// Downmix with no transcode target re-encodes at the source codec (flac).
	res, err := Run(context.Background(), r, in, out, Spec{Downmix: 2}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != transcode.CodecFLAC {
		t.Errorf("downmix result = %+v, want a flac re-encode", res)
	}
	if got := probeChannels(t, r, out); got != 2 {
		t.Errorf("output channels = %d, want 2 (folded)", got)
	}
}

func TestRunDownmixNoOpWhenSourceFitsLayout(t *testing.T) {
	// A stereo source already satisfies a stereo target, so the pipeline writes no
	// output. The same rule prevents a mono source from being expanded to stereo.
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{Downmix: 2}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputPath != "" || fileExists(out) {
		t.Errorf("no-op downmix should write nothing, got %+v exists=%v", res, fileExists(out))
	}
}

func TestRunDownmixWithTranscode(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSurround(t, dir, "in.flac", 2, "flac")
	if got := probeChannels(t, r, in); got != 6 {
		t.Skipf("synth produced %d channels, want 6", got)
	}
	out := filepath.Join(dir, "out.mp3")

	res, err := Run(context.Background(), r, in, out, Spec{Codec: transcode.CodecMP3, Downmix: 2}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != transcode.CodecMP3 {
		t.Errorf("downmix+transcode result = %+v", res)
	}
	if got := probeChannels(t, r, out); got != 2 {
		t.Errorf("output channels = %d, want 2", got)
	}
}

func TestRunDownmixWithNormalize(t *testing.T) {
	// The fold runs before loudnorm, so the normalized, folded output measures at
	// the target (and ffmpeg accepts the fused fold,loudnorm chain).
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSurround(t, dir, "in.flac", 3, "flac")
	if got := probeChannels(t, r, in); got != 6 {
		t.Skipf("synth produced %d channels, want 6", got)
	}
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Codec:    transcode.CodecFLAC,
		Downmix:  2,
		Loudness: &Loudness{Apply: true, Target: -14},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.LoudnessApplied || res.OutputLoudness == nil {
		t.Fatalf("downmix+normalize result = %+v", res)
	}
	if got := probeChannels(t, r, out); got != 2 {
		t.Errorf("output channels = %d, want 2 (folded before loudnorm)", got)
	}
	if got := res.OutputLoudness.IntegratedLUFS; got < -16 || got > -12 {
		t.Errorf("output loudness = %v, want within 2 LU of -14", got)
	}
}

func TestRunDownmixIntoIncompatibleContainer(t *testing.T) {
	// Downmix-only (no explicit transcode) into a container that cannot hold the
	// source codec must encode to the container's codec, not pick the FLAC source
	// encoder and fail inside ffmpeg.
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSurround(t, dir, "in.flac", 2, "flac")
	if got := probeChannels(t, r, in); got != 6 {
		t.Skipf("synth produced %d channels, want 6", got)
	}
	out := filepath.Join(dir, "out.mp3")

	res, err := Run(context.Background(), r, in, out, Spec{Downmix: 2}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != transcode.CodecMP3 {
		t.Errorf("downmix into mp3 result = %+v, want an mp3 encode", res)
	}
	if got := probeChannels(t, r, out); got != 2 {
		t.Errorf("output channels = %d, want 2", got)
	}
}

func TestRunRejectsEmptyExplicitCut(t *testing.T) {
	// An explicit cut whose ranges all fall outside the media is rejected before
	// any output is written (so it cannot clobber an existing destination).
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.flac")

	_, err := Run(context.Background(), r, in, out, Spec{
		Remove:             []cut.Range{{Start: 999 * time.Second, End: 1000 * time.Second}},
		RejectEmptyRemoval: true,
	}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("out-of-range explicit cut err = %v, want ErrIncompatibleSpec", err)
	}
	if fileExists(out) {
		t.Error("a rejected cut must not write output")
	}
}

func TestRunDownmixUnsupportedSourceCodec(t *testing.T) {
	// WaxTap has no AC-3 encoder preset, so a downmix without a transcode target
	// must return ErrIncompatibleSpec before invoking ffmpeg.
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSurround(t, dir, "in.ac3", 2, "ac3")
	out := filepath.Join(dir, "out.ac3")

	_, err := Run(context.Background(), r, in, out, Spec{Downmix: 2}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("unsupported-codec downmix err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestContainerAccepts(t *testing.T) {
	cases := []struct {
		ext, codec string
		want       bool
	}{
		{"flac", "flac", true},
		{"flac", "aac", false},
		{"m4a", "aac", true},
		{"m4a", "alac", true}, // ambiguous container accepts both
		{"m4a", "opus", false},
		{"ogg", "opus", true},
		{"ogg", "vorbis", true}, // ambiguous container accepts both
		{"ogg", "aac", false},
		{"wav", "pcm_s16le", true},
		{"opus", "opus", true},
		{"webm", "opus", true},
		{"webm", "aac", false},
		{"mka", "aac", true},
		{"aac", "aac", true},
		{"aac", "alac", false}, // raw ADTS is AAC-only, unlike .m4a
		{"", "aac", true},      // unknown container: permissive
	}
	for _, c := range cases {
		if got := containerAccepts(c.ext, c.codec); got != c.want {
			t.Errorf("containerAccepts(%q,%q) = %v, want %v", c.ext, c.codec, got, c.want)
		}
	}
}

func TestContainerTablesConsistent(t *testing.T) {
	// Each container's default encoder must produce a codec accepted by that
	// container. codecName maps presets to representative ffprobe codec names.
	codecName := map[transcode.Codec]string{
		transcode.CodecFLAC:   "flac",
		transcode.CodecAAC:    "aac",
		transcode.CodecMP3:    "mp3",
		transcode.CodecOpus:   "opus",
		transcode.CodecVorbis: "vorbis",
		transcode.CodecWAV:    "pcm_s16le",
		transcode.CodecALAC:   "alac",
	}
	for _, ext := range []string{"flac", "wav", "mp3", "m4a", "mp4", "m4b", "aac", "ogg", "oga", "opus", "webm", "mka", "mkv"} {
		c, ok := containerCodec(ext)
		if !ok {
			t.Errorf("containerCodec(%q) = not ok, want a default codec", ext)
			continue
		}
		name, known := codecName[c]
		if !known {
			t.Fatalf("test codecName map is missing %v (returned by containerCodec(%q))", c, ext)
		}
		if !containerAccepts(ext, name) {
			t.Errorf("inconsistent tables: containerCodec(%q)=%v but containerAccepts(%q,%q)=false", ext, c, ext, name)
		}
	}
}

func TestRunCutExtensionChangeTranscodes(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.m4a", 4, "aac")
	out := filepath.Join(dir, "out.flac")

	// AAC cannot be stream-copied into FLAC, so an automatic cut encodes with the
	// destination container's default codec.
	res, err := Run(context.Background(), r, in, out, Spec{
		Remove: []cut.Range{{Start: time.Second, End: 2 * time.Second}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != transcode.CodecFLAC || !res.Cut {
		t.Errorf("extension-change cut result = %+v, want a flac encode with Cut", res)
	}
	pr, err := r.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("probe out: %v", err)
	}
	if a, _ := pr.AudioStream(); a.CodecName != "flac" {
		t.Errorf("output codec = %q, want flac", a.CodecName)
	}
}

func TestRunCutSameContainerCopies(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	// Matroska reports the true post-copy duration, so a same-container smart cut
	// stream-copies cleanly. Raw FLAC is deliberately excluded here: the pipeline
	// escalates a raw-.flac smart cut to a re-encode, covered separately.
	in := synthSine(t, dir, "in.mka", 4, "flac")
	out := filepath.Join(dir, "out.mka")

	res, err := Run(context.Background(), r, in, out, Spec{
		Remove: []cut.Range{{Start: time.Second, End: 2 * time.Second}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Transcoded {
		t.Errorf("same-container cut should stream-copy, not transcode: %+v", res)
	}
	if !res.Cut || !fileExists(out) {
		t.Errorf("cut should have applied and written output: %+v", res)
	}
}

// TestRunSmartCutFlacReencodes verifies that a smart cut from raw FLAC to raw
// FLAC upgrades to a lossless re-encode with a correct duration header.
func TestRunSmartCutFlacReencodes(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Remove: []cut.Range{{Start: time.Second, End: 2 * time.Second}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Cut || !res.Transcoded || res.OutputCodec != transcode.CodecFLAC {
		t.Errorf("smart FLAC cut result = %+v, want a flac re-encode with Cut", res)
	}
	// The header must reflect the trimmed length (~3s), not the 4s source.
	if d := probeDuration(t, r, out); d < 2700*time.Millisecond || d > 3300*time.Millisecond {
		t.Errorf("output duration = %v, want ~3s (stale header would report ~4s)", d)
	}
	if a, _ := res.OutputProbe.AudioStream(); a.CodecName != "flac" {
		t.Errorf("output codec = %q, want flac", a.CodecName)
	}
}

// TestRunCopyCutFlacRejected verifies that explicit copy/remux into raw FLAC is
// rejected instead of writing a file with stale duration metadata.
func TestRunCopyCutFlacRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")

	specs := map[string]Spec{
		"cut-mode copy": {
			Remove:  []cut.Range{{Start: time.Second, End: 2 * time.Second}},
			Codec:   transcode.CodecCopy,
			CutMode: cut.ModeCopy,
		},
		"format copy": {
			Remove: []cut.Range{{Start: time.Second, End: 2 * time.Second}},
			Codec:  transcode.CodecCopy,
			Remux:  true,
		},
	}
	for name, spec := range specs {
		out := filepath.Join(dir, "out.flac")
		_, err := Run(context.Background(), r, in, out, spec, nil)
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("%s into .flac err = %v, want ErrIncompatibleSpec", name, err)
		}
		if fileExists(out) {
			t.Errorf("%s wrote output despite rejection", name)
		}
	}
}

func TestRunForcedCopyIncompatibleContainerRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.m4a", 4, "aac")
	out := filepath.Join(dir, "out.flac")

	// An explicit remux of aac into a flac container is impossible: fail cleanly.
	_, err := Run(context.Background(), r, in, out, Spec{Codec: transcode.CodecCopy, Remux: true}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("forced copy into incompatible container err = %v, want ErrIncompatibleSpec", err)
	}

	// --cut-mode copy into an incompatible container is likewise rejected.
	_, err = Run(context.Background(), r, in, out, Spec{
		Remove:  []cut.Range{{Start: time.Second, End: 2 * time.Second}},
		CutMode: cut.ModeCopy,
	}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("forced cut-copy into incompatible container err = %v, want ErrIncompatibleSpec", err)
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
