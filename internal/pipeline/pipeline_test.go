package pipeline

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

func newTestRunner(t *testing.T) *media.Runner {
	t.Helper()
	r := media.NewRunner(media.RunnerConfig{})
	return r
}

// codecFor maps a fixture codec name to a media.Codec.
func codecFor(name string) media.Codec {
	switch name {
	case "flac":
		return media.CodecFLAC
	case "aac":
		return media.CodecAAC
	case "opus":
		return media.CodecOpus
	case "vorbis":
		return media.CodecVorbis
	case "mp3":
		return media.CodecMP3
	case "alac":
		return media.CodecALAC
	case "aiff":
		return media.CodecAIFF
	default:
		return media.CodecWAV
	}
}

// synth writes a pure-Go WAV sine and, when codec != "wav", transcodes it to name
// through the in-process engine. This replaces the old ffmpeg lavfi fixtures, so
// the suite needs no external tools.
func synth(t *testing.T, dir, name string, seconds, channels int, codec string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	wav := mediatest.SineWAV(seconds, channels)
	if codec == "wav" {
		if err := os.WriteFile(out, wav, 0o644); err != nil {
			t.Fatal(err)
		}
		return out
	}
	src := filepath.Join(t.TempDir(), "src.wav") // separate dir: never pollute the fixture dir
	if err := os.WriteFile(src, wav, 0o644); err != nil {
		t.Fatal(err)
	}
	r := newTestRunner(t)
	if _, err := r.Transcode(context.Background(), src, out, media.Spec{Codec: codecFor(codec)}); err != nil {
		t.Fatalf("synth %s (%s): %v", name, codec, err)
	}
	return out
}

func synthSine(t *testing.T, dir, name string, seconds int, codec string) string {
	t.Helper()
	return synth(t, dir, name, seconds, 2, codec)
}

// synthSurround writes a steady sine as 6 channels, for downmix tests.
func synthSurround(t *testing.T, dir, name string, seconds int, codec string) string {
	t.Helper()
	return synth(t, dir, name, seconds, 6, codec)
}

func probeDuration(t *testing.T, r *media.Runner, path string) time.Duration {
	t.Helper()
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return pr.Format.Duration
}

func probeChannels(t *testing.T, r *media.Runner, path string) int {
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

func TestRunReportsLevels(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	hot := filepath.Join(dir, "hot.wav")
	if err := os.WriteFile(hot, mediatest.HotFloatWAV(1, 2, 13), 0o644); err != nil {
		t.Fatal(err)
	}

	// A plain transcode carries the encode's level measurement out of the result:
	// the float fixture's 13 overs per channel all clip on the way to integer FLAC.
	res, err := Run(context.Background(), r, hot, filepath.Join(dir, "out.flac"), Spec{Codec: media.CodecFLAC}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Levels.ClippedSamples != 26 || !res.Levels.Quantized {
		t.Errorf("transcode Levels = %+v, want 26 clipped quantized samples", res.Levels)
	}
	if want := "clipping: 26 of "; !strings.Contains(res.Levels.Note(), want) {
		t.Errorf("transcode Note() = %q, want it to carry %q", res.Levels.Note(), want)
	}

	// So does a cut, whose write goes through Render instead of Transcode. The
	// overs sit in the first ~13ms, inside the kept half.
	res, err = Run(context.Background(), r, hot, filepath.Join(dir, "cut.flac"), Spec{
		Codec:  media.CodecFLAC,
		Remove: []cutrange.Range{{Start: 500 * time.Millisecond, End: time.Second}},
	}, nil)
	if err != nil {
		t.Fatalf("Run (cut): %v", err)
	}
	if !res.Cut {
		t.Fatal("cut not applied")
	}
	if res.Levels.ClippedSamples != 26 {
		t.Errorf("cut Levels = %+v, want 26 clipped samples", res.Levels)
	}
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
	res, err := Run(context.Background(), r, in, out, Spec{Codec: media.CodecMP3}, emit)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputPath != out || !res.Transcoded || res.OutputCodec != media.CodecMP3 {
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
		res, err := Run(context.Background(), r, in, out, Spec{Codec: media.CodecMP3, Bitrate: 128000}, nil)
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
		Remove: []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
		Codec:  media.CodecFLAC,
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

	// An explicit copy remux rewrites the container without re-encoding.
	out := filepath.Join(dir, "out.mka")
	res, err := Run(context.Background(), r, in, out, Spec{Codec: media.CodecCopy, Remux: true}, nil)
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
	res2, err := Run(context.Background(), r, in, out2, Spec{Codec: media.CodecCopy}, nil)
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
		res, err := Run(context.Background(), r, in, out, Spec{Codec: media.CodecCopy, Remux: true}, nil)
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
			Codec:   media.CodecCopy,
			CutMode: media.ModeSmart,
			Remove:  []cutrange.Range{{Start: 800 * time.Millisecond, End: 1200 * time.Millisecond}},
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
		Remove: []cutrange.Range{{Start: 10 * time.Second, End: 20 * time.Second}}, // beyond EOF: clamps away
		Codec:  media.CodecMP3,
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
		Remove:   []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
		Codec:    media.CodecFLAC,
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

// crestFixture writes the moderate-crest source the convergence tests need: one
// PeakLimit pass misses the target by more than the reporting tolerance, and
// correcting reaches it.
func crestFixture(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, mediatest.CrestWAV(8, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestRunPeakLimitConverges covers F1 Part A: with the peaks handed to the
// limiter, the pipeline measures its own output and corrects the gain until the
// delivered loudness reaches the target.
func TestRunPeakLimitConverges(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := crestFixture(t, dir, "crest.wav")

	for _, target := range []float64{-24, -20, -16, -14, -10} {
		out := filepath.Join(dir, "out.flac")
		res, err := Run(context.Background(), r, in, out, Spec{
			Codec:    media.CodecFLAC,
			Loudness: &Loudness{Apply: true, Target: target, PeakLimit: true},
		}, nil)
		if err != nil {
			t.Fatalf("Run(target %g): %v", target, err)
		}
		if res.OutputLoudness == nil {
			t.Fatalf("target %g: no output loudness measured", target)
		}
		if miss := math.Abs(target - res.OutputLoudness.IntegratedLUFS); miss > loudness.ConvergeToleranceDB {
			t.Errorf("target %g: delivered %.3f LUFS, miss %.3f > tolerance %g",
				target, res.OutputLoudness.IntegratedLUFS, miss, loudness.ConvergeToleranceDB)
		}
		// The limiter must still hold the ceiling under the extra gain iteration
		// pushes through it, which is the regression the search could have caused.
		if tp := res.OutputLoudness.TruePeakDBTP; tp > loudness.TruePeakCeilingDB+0.1 {
			t.Errorf("target %g: true peak %.3f dBTP is over the %g ceiling",
				target, tp, loudness.TruePeakCeilingDB)
		}
		// maxLoudnessWrites+1: the search budget, plus the one restoring write that
		// is allowed past it.
		if res.LoudnessPasses < 1 || res.LoudnessPasses > maxLoudnessWrites+1 {
			t.Errorf("target %g: LoudnessPasses = %d, want 1..%d", target, res.LoudnessPasses, maxLoudnessWrites+1)
		}
	}
}

// TestRunPeakLimitOutputLoudnessMatchesDisk: the reported measurement must
// describe the file actually delivered. Getting this wrong makes --json describe a
// file that is not there, which is the same class of defect as the silent miss.
func TestRunPeakLimitOutputLoudnessMatchesDisk(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := crestFixture(t, dir, "crest.wav")
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Codec:    media.CodecFLAC,
		Loudness: &Loudness{Apply: true, Target: -14, PeakLimit: true},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputLoudness == nil {
		t.Fatal("no output loudness measured")
	}
	disk, err := loudness.Measure(context.Background(), r, out, 0)
	if err != nil {
		t.Fatalf("re-measure output: %v", err)
	}
	if math.Abs(disk.IntegratedLUFS-res.OutputLoudness.IntegratedLUFS) > 0.01 {
		t.Errorf("reported %.3f LUFS but the file on disk measures %.3f",
			res.OutputLoudness.IntegratedLUFS, disk.IntegratedLUFS)
	}
}

// TestRunPeakLimitNoCorrectionWhenOnTarget: a source whose peak and loudness track
// each other lands on the target in one pass, so the search costs nothing.
func TestRunPeakLimitNoCorrectionWhenOnTarget(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 3, "flac") // ~-6.7 LUFS, peak ~-6 dBTP
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Codec:    media.CodecFLAC,
		Loudness: &Loudness{Apply: true, Target: -24, PeakLimit: true}, // attenuating: the limiter never engages
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LoudnessPasses != 1 {
		t.Errorf("LoudnessPasses = %d, want 1 (pass 1 landed inside tolerance)", res.LoudnessPasses)
	}
}

// TestRunPeakCapStaysSinglePass: cap's head clamp is the whole policy, so it must
// not iterate. Its output is unchanged from before the search existed.
func TestRunPeakCapStaysSinglePass(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := crestFixture(t, dir, "crest.wav")
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Codec:    media.CodecFLAC,
		Loudness: &Loudness{Apply: true, Target: -14}, // PeakLimit false
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LoudnessPasses != 1 {
		t.Errorf("LoudnessPasses = %d, want 1 (cap never iterates)", res.LoudnessPasses)
	}
	if res.OutputLoudness == nil {
		t.Fatal("cap mode should still post-measure")
	}
	// The crest source peaks at ~0 dBTP, so the clamp allows about -1 dB of gain and
	// the output lands nowhere near -14. Iterating would have "fixed" that, which is
	// exactly what cap must not do.
	if got := res.OutputLoudness.IntegratedLUFS; got > -20 {
		t.Errorf("cap output = %.3f LUFS, want well short of -14 (the clamp should bind)", got)
	}
}

// TestRunPeakLimitFailedWriteLeavesNoOutput: iteration is the one change that
// could leave a committed earlier pass behind when the job fails, so a rejected
// output path must still produce no file at all.
func TestRunPeakLimitFailedWriteLeavesNoOutput(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := crestFixture(t, dir, "crest.wav")
	// A directory that does not exist: staging cannot open a temp file there.
	out := filepath.Join(dir, "missing", "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Codec:    media.CodecFLAC,
		Loudness: &Loudness{Apply: true, Target: -14, PeakLimit: true},
	}, nil)
	if err == nil {
		t.Fatalf("Run into an unwritable path succeeded: %+v", res)
	}
	if fileExists(out) {
		t.Error("a failed run left a file at the output path")
	}
}

// TestRunPeakLimitCancelDuringSearchFails: a cancellation partway through the gain
// search must surface, not be swallowed as success. The file at the output path is
// complete (every write commits atomically) but carries an uncorrected gain, so
// reporting success would present a loudness nobody asked for as the delivered
// result, and would drop the Ctrl-C exit code on the floor.
func TestRunPeakLimitCancelDuringSearchFails(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := crestFixture(t, dir, "crest.wav")
	out := filepath.Join(dir, "out.flac")

	// Cancel as soon as the first write finishes, so the search is interrupted
	// between passes rather than before any output exists.
	ctx, cancel := context.WithCancel(context.Background())
	var wrote bool
	emit := func(s Stage) {
		if s == StageAnalyzing && wrote {
			cancel()
		}
		if s == StageTranscoding {
			wrote = true
		}
	}

	_, err := Run(ctx, r, in, out, Spec{
		Codec:    media.CodecFLAC,
		Loudness: &Loudness{Apply: true, Target: -14, PeakLimit: true},
	}, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The committed pass stays on disk: a caller can name it, and nothing partial
	// can be there because every write is atomic.
	if !fileExists(out) {
		t.Error("the committed pass was removed; a canceled search should leave it in place")
	}
}

func TestRunLoudnessApplyWithoutTranscodeRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 1, "flac")
	out := filepath.Join(dir, "out.flac")

	_, err := Run(context.Background(), r, in, out, Spec{
		Codec:    media.CodecCopy,
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
		Remove: []cutrange.Range{{Start: 0, End: time.Hour}},
		Codec:  media.CodecFLAC,
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
	if !res.Transcoded || res.OutputCodec != media.CodecFLAC {
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

	res, err := Run(context.Background(), r, in, out, Spec{Codec: media.CodecMP3, Downmix: 2}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != media.CodecMP3 {
		t.Errorf("downmix+transcode result = %+v", res)
	}
	if got := probeChannels(t, r, out); got != 2 {
		t.Errorf("output channels = %d, want 2", got)
	}
}

func TestRunDownmixWithNormalize(t *testing.T) {
	// The fold runs before the gain, so the normalized, folded output measures at
	// the target.
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSurround(t, dir, "in.flac", 3, "flac")
	if got := probeChannels(t, r, in); got != 6 {
		t.Skipf("synth produced %d channels, want 6", got)
	}
	out := filepath.Join(dir, "out.flac")

	res, err := Run(context.Background(), r, in, out, Spec{
		Codec:    media.CodecFLAC,
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
		t.Errorf("output channels = %d, want 2 (folded before the gain)", got)
	}
	if got := res.OutputLoudness.IntegratedLUFS; got < -16 || got > -12 {
		t.Errorf("output loudness = %v, want within 2 LU of -14", got)
	}
}

func TestRunDownmixIntoIncompatibleContainer(t *testing.T) {
	// Downmix-only (no explicit transcode) into a container that cannot hold the
	// source codec must encode to the container's codec, not pick the FLAC source
	// encoder and fail at the muxer.
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
	if !res.Transcoded || res.OutputCodec != media.CodecMP3 {
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
		Remove:             []cutrange.Range{{Start: 999 * time.Second, End: 1000 * time.Second}},
		RejectEmptyRemoval: true,
	}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("out-of-range explicit cut err = %v, want ErrIncompatibleSpec", err)
	}
	if fileExists(out) {
		t.Error("a rejected cut must not write output")
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
		{"aac", "alac", false},      // raw ADTS is AAC-only, unlike .m4a
		{"", "aac", true},           // unknown container: permissive
		{"aiff", "pcm_s16le", true}, // a probed PCM source
		{"aiff", "wav", false},      // CodecWAV is RIFF, which .aiff cannot hold
		{"wav", "aiff", false},      // and the mirror
		{"aif", "aiff", true},       // both output spellings
	}
	for _, c := range cases {
		if got := containerAccepts(c.ext, c.codec); got != c.want {
			t.Errorf("containerAccepts(%q,%q) = %v, want %v", c.ext, c.codec, got, c.want)
		}
	}
}

// TestSourceEncodeCodecPCMFollowsExtension covers the fallback a downmix or a
// declined copy-cut takes. PCM has two container-defining encoders, so a .aiff
// output has to reach CodecAIFF; every PCM source used to fall to CodecWAV and
// write RIFF bytes whatever the file was named.
func TestSourceEncodeCodecPCMFollowsExtension(t *testing.T) {
	cases := []struct {
		name, outExt string
		want         media.Codec
		ok           bool
	}{
		{"pcm_s16le", "aiff", media.CodecAIFF, true},
		{"pcm_s16le", "aif", media.CodecAIFF, true},
		{"pcm_s16le", "aifc", media.CodecAIFF, true}, // all four spellings, or these
		{"pcm_s16le", "afc", media.CodecAIFF, true},  // two collect RIFF bytes
		{"pcm_f32le", "aiff", media.CodecAIFF, true},
		{"pcm_s16le", "wav", media.CodecWAV, true},
		{"pcm_s16le", "", media.CodecWAV, true}, // no extension: the RIFF default
		{"pcm_s16le", "mka", media.CodecWAV, true},
		// Only PCM is extension-directed; other families ignore outExt.
		{"flac", "aiff", media.CodecFLAC, true},
		{"opus", "aiff", media.CodecOpus, true},
		{"dts", "aiff", media.CodecCopy, false},
	}
	for _, c := range cases {
		got, ok := sourceEncodeCodec(c.name, c.outExt)
		if got != c.want || ok != c.ok {
			t.Errorf("sourceEncodeCodec(%q,%q) = %v,%v; want %v,%v", c.name, c.outExt, got, ok, c.want, c.ok)
		}
	}
}

func TestContainerTablesConsistent(t *testing.T) {
	// Each container's default encoder must produce a codec accepted by that
	// container. codecName maps presets to representative codec names.
	codecName := map[media.Codec]string{
		media.CodecFLAC:   "flac",
		media.CodecAAC:    "aac",
		media.CodecMP3:    "mp3",
		media.CodecOpus:   "opus",
		media.CodecVorbis: "vorbis",
		media.CodecWAV:    "pcm_s16le",
		media.CodecALAC:   "alac",
		media.CodecAIFF:   "aiff",
	}
	for _, ext := range []string{"flac", "wav", "aiff", "aif", "aifc", "afc", "mp3", "m4a", "mp4", "m4b", "aac", "ogg", "oga", "opus", "webm", "mka", "mkv"} {
		c, ok := media.ContainerCodec(ext)
		if !ok {
			t.Errorf("media.ContainerCodec(%q) = not ok, want a default codec", ext)
			continue
		}
		name, known := codecName[c]
		if !known {
			t.Fatalf("test codecName map is missing %v (returned by media.ContainerCodec(%q))", c, ext)
		}
		if !containerAccepts(ext, name) {
			t.Errorf("inconsistent tables: media.ContainerCodec(%q)=%v but containerAccepts(%q,%q)=false", ext, c, ext, name)
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
		Remove: []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != media.CodecFLAC || !res.Cut {
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
	// Opus is on WaxFlow's cut-remux allowlist, so a same-container smart cut is a
	// lossless packet copy (no re-encode). FLAC is deliberately excluded here: it is
	// off the allowlist and escalates to a re-encode, covered separately.
	in := synthSine(t, dir, "in.mka", 4, "opus")
	out := filepath.Join(dir, "out.mka")

	res, err := Run(context.Background(), r, in, out, Spec{
		Remove: []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
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
		Remove: []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Cut || !res.Transcoded || res.OutputCodec != media.CodecFLAC {
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

	// The advice must name the flag the caller actually passed: telling someone
	// to drop --format copy when they never wrote it sends them looking for a
	// flag that is not in their command.
	specs := map[string]struct {
		spec   Spec
		want   []string
		reject string
	}{
		"cut-mode copy": {
			spec: Spec{
				Remove:  []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
				Codec:   media.CodecCopy,
				CutMode: media.ModeCopy,
			},
			want:   []string{"--cut-mode copy"},
			reject: "--format copy",
		},
		"format copy": {
			spec: Spec{
				Remove: []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
				Codec:  media.CodecCopy,
				Remux:  true,
			},
			want:   []string{"--format copy"},
			reject: "--cut-mode copy",
		},
		"both flags": {
			spec: Spec{
				Remove:  []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
				Codec:   media.CodecCopy,
				CutMode: media.ModeCopy,
				Remux:   true,
			},
			want: []string{"--format copy", "--cut-mode copy"},
		},
	}
	for name, tc := range specs {
		out := filepath.Join(dir, "out.flac")
		_, err := Run(context.Background(), r, in, out, tc.spec, nil)
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("%s into .flac err = %v, want ErrIncompatibleSpec", name, err)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s err = %q, want it to name %q", name, err, w)
			}
		}
		if tc.reject != "" && strings.Contains(err.Error(), tc.reject) {
			t.Errorf("%s err = %q, must not name %q, which was not passed", name, err, tc.reject)
		}
		if fileExists(out) {
			t.Errorf("%s wrote output despite rejection", name)
		}
	}
}

// TestRunCopyCutWithEncodeRejected covers the F4 invariant: an explicit copy cut
// combined with an encode is rejected rather than silently downgraded to a
// re-encode. Both rows use a container that accepts the source codec, so the
// container-mismatch guard above them does not fire first and the new check is
// what rejects them.
func TestRunCopyCutWithEncodeRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	remove := []cutrange.Range{{Start: time.Second, End: 2 * time.Second}}

	t.Run("transcode-target", func(t *testing.T) {
		in := synthSine(t, dir, "stereo.flac", 4, "flac")
		out := filepath.Join(dir, "target.flac")
		_, err := Run(context.Background(), r, in, out, Spec{
			Remove: remove, Codec: media.CodecFLAC, CutMode: media.ModeCopy,
		}, nil)
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("err = %v, want ErrIncompatibleSpec", err)
		}
		if fileExists(out) {
			t.Error("wrote output despite rejection")
		}
	})

	// The downmix branch sets Codec and transcoding without consulting CutMode, so
	// this reaches the same downgrade by a route the facade check cannot see (it
	// skips downmix on purpose, since the fold decision needs the probe).
	t.Run("downmix-no-format", func(t *testing.T) {
		in := synthSurround(t, dir, "surround.mka", 2, "flac")
		if got := probeChannels(t, r, in); got != 6 {
			t.Skipf("synth produced %d channels, want 6", got)
		}
		out := filepath.Join(dir, "folded.mka")
		_, err := Run(context.Background(), r, in, out, Spec{
			Remove: remove, Codec: media.CodecCopy, CutMode: media.ModeCopy, Downmix: 2,
		}, nil)
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("err = %v, want ErrIncompatibleSpec", err)
		}
		if fileExists(out) {
			t.Error("wrote output despite rejection")
		}
	})
}

func TestRunForcedCopyIncompatibleContainerRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.m4a", 4, "aac")
	out := filepath.Join(dir, "out.flac")

	// An explicit remux of aac into a flac container is impossible: fail cleanly.
	_, err := Run(context.Background(), r, in, out, Spec{Codec: media.CodecCopy, Remux: true}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("forced copy into incompatible container err = %v, want ErrIncompatibleSpec", err)
	}

	// --cut-mode copy into an incompatible container is likewise rejected.
	_, err = Run(context.Background(), r, in, out, Spec{
		Remove:  []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
		CutMode: media.ModeCopy,
	}, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("forced cut-copy into incompatible container err = %v, want ErrIncompatibleSpec", err)
	}
}

func assertHasStage(t *testing.T, seen []Stage, want Stage) {
	t.Helper()
	if !slices.Contains(seen, want) {
		t.Errorf("stage %v not emitted; saw %v", want, seen)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// SourceChannels is the only thing a caller can compare the output layout
// against: nothing in a request says "6 channels", so without it an encoder that
// folds a surround master to stereo leaves no signal at all.
func TestRunReportsSourceChannels(t *testing.T) {
	dir := t.TempDir()
	in := synthSurround(t, dir, "in.wav", 1, "wav")
	res, err := Run(t.Context(), newTestRunner(t), in, filepath.Join(dir, "out.mp3"),
		Spec{Codec: media.CodecMP3}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SourceChannels != 6 {
		t.Errorf("SourceChannels = %d, want 6", res.SourceChannels)
	}
	// The fold itself is WaxFlow policy and correct; what matters here is that both
	// halves of the comparison are on the Result.
	if res.OutputProbe == nil {
		t.Fatal("OutputProbe is nil; the fold cannot be detected without it")
	}
	if a, ok := res.OutputProbe.AudioStream(); !ok || a.Channels >= res.SourceChannels {
		t.Errorf("output channels = %+v, want fewer than the source's %d", a, res.SourceChannels)
	}
}

// TestRunCopyRemuxReportsRemuxing covers F9: `--format copy` reported
// "transcoding" because the write closure sent StageTranscoding unconditionally.
// It also pins the subtle half: a copy the pipeline promotes to a real encoder
// must still report transcoding, so the branch has to read the encoder's own
// codec and not the requested spec.
func TestRunCopyRemuxReportsRemuxing(t *testing.T) {
	r := newTestRunner(t)

	t.Run("copy remux reports remuxing", func(t *testing.T) {
		dir := t.TempDir()
		in := synthSine(t, dir, "in.flac", 2, "flac")
		emit, seen := recordStages()
		res, err := Run(t.Context(), r, in, filepath.Join(dir, "out.mka"),
			Spec{Codec: media.CodecCopy, Remux: true}, emit)
		if err != nil {
			t.Fatalf("Run remux: %v", err)
		}
		if res.Transcoded {
			t.Error("a container copy re-encoded nothing; Transcoded should stay false")
		}
		assertHasStage(t, *seen, StageRemuxing)
		for _, s := range *seen {
			if s == StageTranscoding {
				t.Errorf("a copy remux must not report transcoding; saw %v", *seen)
			}
		}
	})

	// A requested downmix promotes a copy to a real encode at the source codec.
	// That is a transcode however it was requested, and it is exactly the case
	// branching on the requested spec.Codec would get wrong: spec.Codec is
	// reassigned before the write closure is built.
	t.Run("promoted copy still reports transcoding", func(t *testing.T) {
		dir := t.TempDir()
		in := synthSurround(t, dir, "in.flac", 2, "flac")
		if got := probeChannels(t, r, in); got != 6 {
			t.Skipf("synth produced %d channels, want 6", got)
		}
		emit, seen := recordStages()
		res, err := Run(t.Context(), r, in, filepath.Join(dir, "out.flac"),
			Spec{Codec: media.CodecCopy, Downmix: 2}, emit)
		if err != nil {
			t.Fatalf("Run promoted copy: %v", err)
		}
		if !res.Transcoded {
			t.Error("a copy promoted to an encoder should report Transcoded")
		}
		assertHasStage(t, *seen, StageTranscoding)
		for _, s := range *seen {
			if s == StageRemuxing {
				t.Errorf("a promoted copy must not report remuxing; saw %v", *seen)
			}
		}
	})
}

// TestStageStringsAreDistinct guards the label the CLI renders: progress.go
// prints Stage.String() directly, so a new stage needs no rendering change but
// does need a name of its own.
func TestStageStringsAreDistinct(t *testing.T) {
	seen := map[string]Stage{}
	for _, s := range []Stage{StageProbing, StageAnalyzing, StageCutting, StageNormalizing, StageTranscoding, StageRemuxing} {
		name := s.String()
		if name == "unknown" {
			t.Errorf("stage %d renders as %q", s, name)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("stages %d and %d share the label %q", prev, s, name)
		}
		seen[name] = s
	}
	if got := StageRemuxing.String(); got != "remuxing" {
		t.Errorf("StageRemuxing.String() = %q, want %q", got, "remuxing")
	}
}

// taggedWV encodes a stereo sine to WavPack and tags it through WaxLabel (one
// own-audio value included), returning its path.
func taggedWV(t *testing.T, dir, name string) string {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "src.wav")
	if err := os.WriteFile(src, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	r := newTestRunner(t)
	if _, err := r.Transcode(ctx, src, out, media.Spec{Codec: media.CodecWavPack}); err != nil {
		t.Fatalf("synth wv: %v", err)
	}
	if err := mediatest.TagFile(ctx, out,
		"TITLE", "Pipeline Tagged",
		"REPLAYGAIN_TRACK_GAIN", "-2.00 dB"); err != nil {
		t.Fatalf("tag fixture: %v", err)
	}
	return out
}

func probeTag(t *testing.T, r *media.Runner, path, key string) string {
	t.Helper()
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	if vs := pr.Tags[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// A downmix with no transcode target promotes into the source's own family.
// The pipeline writes no tags anywhere: metadata is the facade's WaxLabel
// post-pass, so the re-encode of a tagged source comes out bare and a download
// that did not ask for metadata cannot inherit the stream's own container tags.
func TestRunDownmixPromotionWritesNoTags(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := taggedWV(t, dir, "in.wv")
	out := filepath.Join(dir, "mono.wv")
	res, err := Run(context.Background(), r, in, out, Spec{Downmix: 1}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.OutputCodec != media.CodecWavPack || !res.Transcoded {
		t.Fatalf("result = codec %v transcoded %v, want a wavpack re-encode", res.OutputCodec, res.Transcoded)
	}
	if got := probeTag(t, r, out, "TITLE"); got != "" {
		t.Errorf("TITLE = %q, want none (the facade's carry pass owns metadata)", got)
	}
}

// An explicit remux of a WavPack source (--format copy) also comes out bare at
// this layer: the facade's carry pass restores the metadata, own-audio values
// included, onto the finished file.
func TestRunRemuxWavPackStripsTags(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := taggedWV(t, dir, "in.wv")
	out := filepath.Join(dir, "copy.wv")
	res, err := Run(context.Background(), r, in, out, Spec{Remux: true}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Transcoded {
		t.Error("remux reported Transcoded")
	}
	if got := probeTag(t, r, out, "TITLE"); got != "" {
		t.Errorf("TITLE = %q, want none at the pipeline layer", got)
	}
}

// A cut into a .wv container the source codec cannot enter promotes to the
// WavPack encoder, the same rule every other container promotion follows. (An
// explicit --format copy into .wv stays a rejection: that request contradicts
// itself, and TestRunRemux's family covers it.)
func TestRunCutIntoWVPromotes(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.wv")
	spec := Spec{Remove: []cutrange.Range{{Start: 0, End: 500 * time.Millisecond}}}
	res, err := Run(context.Background(), r, in, out, spec, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.OutputCodec != media.CodecWavPack || !res.Transcoded || !res.Cut {
		t.Errorf("result = codec %v transcoded %v cut %v, want a cut wavpack encode", res.OutputCodec, res.Transcoded, res.Cut)
	}
}

func TestSourceEncodeCodecNewFamilies(t *testing.T) {
	cases := []struct {
		name string
		want media.Codec
		ok   bool
	}{
		{"he-aac", media.CodecHEAAC, true},
		{"wavpack", media.CodecWavPack, true},
		{"ape", media.CodecAPE, true},
		{"wma", media.CodecCopy, false},      // decode-only: no encoder keeps the family
		{"musepack", media.CodecCopy, false}, // likewise
	}
	for _, c := range cases {
		got, ok := sourceEncodeCodec(c.name, "")
		if got != c.want || ok != c.ok {
			t.Errorf("sourceEncodeCodec(%q) = %v,%v want %v,%v", c.name, got, ok, c.want, c.ok)
		}
	}
	for ext, want := range map[string]media.Codec{"wv": media.CodecWavPack, "ape": media.CodecAPE} {
		if got, ok := media.ContainerCodec(ext); !ok || got != want {
			t.Errorf("media.ContainerCodec(%q) = %v,%v want %v,true", ext, got, ok, want)
		}
	}
}

// A Musepack input transcodes by extension like any other lossy source and
// reports its codec; a request that would need a Musepack writer declines in
// the pipeline's own words rather than reaching the engine with no format.
func TestRunMusepackSource(t *testing.T) {
	r := newTestRunner(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mpc")
	if err := os.WriteFile(in, mediatest.TaggedMPC(), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Run(ctx, r, in, filepath.Join(dir, "out.flac"), Spec{Codec: media.CodecFLAC}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.SourceCodec != "musepack" || res.OutputCodec != media.CodecFLAC || !res.Transcoded {
		t.Errorf("result = source %q codec %v transcoded %v, want a musepack source encoded to flac", res.SourceCodec, res.OutputCodec, res.Transcoded)
	}

	// A cut into the source's own container is refused for the container's
	// sake: nothing writes a .mpc file, so no encoder can be inferred for it.
	cut := Spec{Remove: []cutrange.Range{{Start: 0, End: 50 * time.Millisecond}}}
	_, err = Run(ctx, r, in, filepath.Join(dir, "cut.mpc"), cut, nil)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") || !strings.Contains(err.Error(), "--format") {
		t.Errorf("cut into .mpc = %v, want ErrIncompatibleSpec naming the read-only container and the --format escape", err)
	}

	// Every request to keep the source packets fails for the source's sake,
	// before any container is judged: a copy, a copy-mode cut, and a copy cut
	// with no container all say the codec is decode-only and name the escape,
	// instead of "cannot copy musepack audio into a .mpc container; transcode
	// instead" for a container that would then refuse the transcode too.
	for name, spec := range map[string]Spec{
		"copy":          {Remux: true},
		"copy-mode cut": {Remove: cut.Remove, CutMode: media.ModeCopy},
	} {
		for _, out := range []string{"out.mpc", "out.mka", "out"} {
			_, err := Run(ctx, r, in, filepath.Join(dir, out), spec, nil)
			if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") || !strings.Contains(err.Error(), "pass --format") {
				t.Errorf("%s into %s = %v, want ErrIncompatibleSpec naming the decode-only source and the escape", name, out, err)
			}
		}
	}
}

// A downmix promotes a copy spec to a real encode, so a normalization asking
// for both is a request the pipeline can satisfy; refusing it as "copy" read as
// a contradiction the user had not written.
func TestNormalizeWithDownmixPromotesTheCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "surround.wav")
	if err := os.WriteFile(in, mediatest.SineWAV(2, 6), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.wav")

	res, err := Run(ctx, media.NewRunner(media.RunnerConfig{}), in, out, Spec{
		Codec:    media.CodecCopy,
		Downmix:  2,
		Loudness: &Loudness{Apply: true, Target: -18},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || !res.LoudnessApplied {
		t.Errorf("Transcoded=%v LoudnessApplied=%v, want the promoted encode", res.Transcoded, res.LoudnessApplied)
	}
	if res.OutputProbe == nil {
		t.Fatal("no output probe")
	}
	if a, ok := res.OutputProbe.AudioStream(); !ok || a.Channels != 2 {
		t.Errorf("output channels = %+v, want the stereo fold", a)
	}

	// A copy with nothing to promote it is still refused.
	if _, err := Run(ctx, media.NewRunner(media.RunnerConfig{}), in, filepath.Join(dir, "copy.wav"), Spec{
		Codec:    media.CodecCopy,
		Loudness: &Loudness{Apply: true, Target: -18},
	}, nil); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("err = %v, want ErrIncompatibleSpec", err)
	}
}

// A loudness apply that names no encoder, into a container the source codec
// cannot enter, takes the encoder the container implies rather than being
// refused for naming no target. The promotion is not silent: it is a lossless
// source landing in a lossy codec nothing asked for, which the caller reports
// as implicit-lossy, and OutputCodec names what was written.
func TestNormalizeIntoAForeignContainerPromotesAndSaysSo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	r := media.NewRunner(media.RunnerConfig{})
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transcode(ctx, wav, in, media.Spec{Codec: media.CodecFLAC}); err != nil {
		t.Fatal(err)
	}

	res, err := Run(ctx, r, in, filepath.Join(dir, "out.opus"), Spec{
		Codec:    media.CodecCopy,
		Remove:   []cutrange.Range{{Start: 0, End: 500 * time.Millisecond}},
		Loudness: &Loudness{Apply: true, Target: -18},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != media.CodecOpus {
		t.Errorf("Transcoded=%v OutputCodec=%v, want the container's encoder", res.Transcoded, res.OutputCodec)
	}
	if !res.LoudnessApplied {
		t.Error("LoudnessApplied = false, want the gain the promoted encode carried")
	}
	// The source codec is what the caller's implicit-lossy warning reads.
	if res.SourceCodec != "flac" {
		t.Errorf("SourceCodec = %q, want flac", res.SourceCodec)
	}
}

// A format the container chose is a fallback: once the source is staged and
// probed, a container that carries its codec keeps it, and only one that
// cannot takes the encoder the caller passed. A knob or a gain that needs
// an encode gets the same-family encoder, so an Opus source into .ogg
// normalizes by its header gain rather than becoming Vorbis.
func TestRunContainerChosenKeepsACarriedSource(t *testing.T) {
	dir := t.TempDir()
	src := synthSine(t, dir, "src.opus", 2, "opus")
	r := newTestRunner(t)
	keep := []cutrange.Range{{Start: 500 * time.Millisecond, End: time.Second}}
	cases := []struct {
		name       string
		out        string
		spec       Spec
		transcoded bool
		codec      media.Codec
		cut        bool
	}{
		{"mka keeps opus", "out.mka", Spec{Codec: media.CodecOpus, ContainerChosen: true}, false, media.CodecCopy, false},
		{"ogg keeps opus", "out.ogg", Spec{Codec: media.CodecVorbis, ContainerChosen: true}, false, media.CodecCopy, false},
		{"m4a cannot carry opus", "out.m4a", Spec{Codec: media.CodecAAC, ContainerChosen: true}, true, media.CodecAAC, false},
		{"a bitrate the codec takes needs an encode", "out.mka", Spec{Codec: media.CodecOpus, Bitrate: 96000, ContainerChosen: true}, true, media.CodecOpus, false},
		{"a bit depth the codec ignores keeps the copy", "out.mka", Spec{Codec: media.CodecOpus, BitDepth: 16, ContainerChosen: true}, false, media.CodecCopy, false},
		{"named opus still encodes", "out.mka", Spec{Codec: media.CodecOpus}, true, media.CodecOpus, false},
		// A cut is not copy-only: it copies packets where the codec allows,
		// as a local cut into this container does, and the flag that gets a
		// plain copy past the no-op return must not make it one.
		{"a cut stays a packet copy", "out.mka", Spec{Codec: media.CodecOpus, ContainerChosen: true, Remove: keep}, false, media.CodecCopy, true},
		{"a crossfade re-encodes in the family", "out.mka", Spec{Codec: media.CodecOpus, ContainerChosen: true, Remove: keep, Crossfade: 50 * time.Millisecond}, true, media.CodecOpus, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name+"-"+tc.out)
			res, err := Run(context.Background(), r, src, out, tc.spec, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Transcoded != tc.transcoded || res.OutputCodec != tc.codec || res.Cut != tc.cut {
				t.Errorf("transcoded=%v codec=%v cut=%v, want %v %v %v", res.Transcoded, res.OutputCodec, res.Cut, tc.transcoded, tc.codec, tc.cut)
			}
			p, err := r.Probe(context.Background(), out)
			if err != nil {
				t.Fatal(err)
			}
			if a, _ := p.AudioStream(); tc.codec == media.CodecCopy && a.CodecName != "opus" {
				t.Errorf("kept copy holds %s, want opus", a.CodecName)
			}
		})
	}
	// The gain rides in the head of a kept Opus copy under cap.
	res, err := Run(context.Background(), r, src, filepath.Join(dir, "gain.ogg"),
		Spec{Codec: media.CodecVorbis, ContainerChosen: true, Loudness: &Loudness{Apply: true, Target: -18}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Transcoded || !res.GainInHeader {
		t.Errorf("transcoded=%v gainInHeader=%v, want a header-gain copy", res.Transcoded, res.GainInHeader)
	}
}

// The chosen-container bit is a fallback, so it never makes a spec that
// would have been written fail. Two sources take the family encoder rather
// than the copy the container rule would otherwise reach for: PCM, whose
// packets belong to their container and cannot move, and an output naming
// no container at all, where there is no rule to apply and the muxer comes
// from the format.
func TestRunContainerChosenNeverRefusesWhatItCannotKeep(t *testing.T) {
	dir := t.TempDir()
	r := newTestRunner(t)
	wav := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}

	// Matroska carries PCM through WaxFlow's wav row, so the container rule
	// keeps the source; a copy of it would be declined by the remux.
	out := filepath.Join(dir, "pcm.mka")
	res, err := Run(context.Background(), r, wav, out, Spec{Codec: media.CodecOpus, ContainerChosen: true}, nil)
	if err != nil {
		t.Fatalf("PCM into .mka: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != media.CodecWAV {
		t.Errorf("transcoded=%v codec=%v, want the lossless wav row, not a copy the remux declines", res.Transcoded, res.OutputCodec)
	}
	if p, perr := r.Probe(context.Background(), out); perr != nil {
		t.Fatal(perr)
	} else if a, _ := p.AudioStream(); a.CodecName != "pcm" {
		t.Errorf("the .mka holds %s, want pcm", a.CodecName)
	}

	// No extension: nothing names a container, so the caller's format stands
	// and its own muxer writes the file.
	opus := synthSine(t, dir, "in.opus", 1, "opus")
	bare := filepath.Join(dir, "bare")
	res, err = Run(context.Background(), r, opus, bare, Spec{Codec: media.CodecOpus, ContainerChosen: true}, nil)
	if err != nil {
		t.Fatalf("an extensionless output: %v", err)
	}
	if !res.Transcoded || res.OutputCodec != media.CodecOpus {
		t.Errorf("transcoded=%v codec=%v, want the format the caller passed", res.Transcoded, res.OutputCodec)
	}
}

// A packet copy reports the spans it landed on, and the short-decode note
// measures the output against them rather than against the request: the joins
// move inward by under one packet each, which is not input damage.
func TestRunCopyCutReportsTheLandedKeeps(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.opus", 3, "opus")
	out := filepath.Join(dir, "out.opus")

	res, err := Run(context.Background(), r, in, out, Spec{
		// Both edges sit off the 20 ms Opus grid, so both joins snap.
		Remove:  []cutrange.Range{{Start: 1010 * time.Millisecond, End: 2010 * time.Millisecond}},
		CutMode: media.ModeSmart,
		Codec:   media.CodecCopy,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CutMode != media.ModeCopy || res.Transcoded {
		t.Fatalf("CutMode = %v, Transcoded = %v, want a packet copy", res.CutMode, res.Transcoded)
	}
	if res.CutSnaps != 1 || res.CutSnapMax <= 0 || res.CutSnapMax >= 20*time.Millisecond {
		t.Errorf("CutSnaps = %d, CutSnapMax = %v, want 2 joins under 20 ms", res.CutSnaps, res.CutSnapMax)
	}
	if len(res.Keeps) != 2 || res.Keeps[1].Start <= 2010*time.Millisecond {
		t.Errorf("Keeps = %v, want the second span's head landed above the requested 2.01s", res.Keeps)
	}
	for _, w := range res.SourceWarnings {
		if strings.Contains(w, "ended at") {
			t.Errorf("SourceWarnings = %v, want no short-decode note for a snapped copy cut", res.SourceWarnings)
		}
	}
}
