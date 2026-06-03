package transcode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// newTestRunner builds a Runner, skipping the test when ffmpeg/ffprobe are not
// installed so the suite still runs in minimal environments.
func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerConfig{ShutdownGrace: 2 * time.Second})
	if err != nil {
		if errors.Is(err, waxerr.ErrFFmpegNotFound) {
			t.Skip("ffmpeg/ffprobe not installed")
		}
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// synthSine writes a short stereo sine wav to dir using ffmpeg directly (not the
// code under test), so test fixtures are generated rather than committed. extra
// args are inserted before the lavfi input (e.g. "-re" for realtime pacing).
func synthSine(t *testing.T, dir string, seconds int, extra ...string) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, "in.wav")
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	args = append(args, extra...)
	args = append(args,
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration="+strconv.Itoa(seconds),
		"-ac", "2",
		out,
	)
	if b, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("synth sine: %v: %s", err, b)
	}
	return out
}

// synthWav writes a 1s stereo sine to dir/name with the given output codec args
// (e.g. "-c:a", "pcm_s24le"), used to produce sources of a specific bit depth.
func synthWav(t *testing.T, dir, name string, codecArgs ...string) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=1", "-ac", "2",
	}
	args = append(args, codecArgs...)
	args = append(args, out)
	if b, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("synth %s: %v: %s", name, err, b)
	}
	return out
}

// synthVideoOnly writes a 1s video-only file (no audio stream) to dir.
func synthVideoOnly(t *testing.T, dir string) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, "video.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x64:rate=10",
		"-an", "-c:v", "mpeg4", out,
	}
	if b, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("synth video: %v: %s", err, b)
	}
	return out
}

// assertNoTempLeft fails if any staging temp for output remains. NewExternal
// names temps "<output>.<rand><ext>", so they match the glob "<output>.*" while
// the final file (exactly output) does not.
func assertNoTempLeft(t *testing.T, output string) {
	t.Helper()
	matches, _ := filepath.Glob(output + ".*")
	if len(matches) != 0 {
		t.Fatalf("leftover temp files for %s: %v", output, matches)
	}
}

func TestRunner_Probe(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, 1)

	pr, err := r.Probe(context.Background(), in)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	a, ok := pr.AudioStream()
	if !ok {
		t.Fatal("Probe found no audio stream in a sine wav")
	}
	if a.Channels != 2 || a.SampleRate != 44100 {
		t.Errorf("audio = %dch %dHz, want 2/44100", a.Channels, a.SampleRate)
	}
	if pr.Format.Duration < 900*time.Millisecond || pr.Format.Duration > 1200*time.Millisecond {
		t.Errorf("duration = %v, want ~1s", pr.Format.Duration)
	}
}

func TestRunner_ProbeUnsupported(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "notmedia.txt")
	if err := os.WriteFile(bad, []byte("this is not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.Probe(context.Background(), bad)
	if !errors.Is(err, waxerr.ErrUnsupportedInput) {
		t.Fatalf("Probe(text) err = %v, want ErrUnsupportedInput", err)
	}
}

func TestRunner_ProbeNoAudio(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	// A video-only file ffprobe reads fine but that carries no audio stream must
	// be classified as unsupported at the probe boundary, not later.
	vid := synthVideoOnly(t, dir)
	_, err := r.Probe(context.Background(), vid)
	if !errors.Is(err, waxerr.ErrUnsupportedInput) {
		t.Fatalf("Probe(video-only) err = %v, want ErrUnsupportedInput", err)
	}
}

func TestRunner_TranscodeWAVPreservesBitDepth(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()

	cases := []struct {
		srcCodec  string
		wantCodec string
	}{
		{"pcm_s24le", "pcm_s24le"}, // a 24-bit source must not be downgraded to 16-bit
		{"pcm_s16le", "pcm_s16le"}, // a 16-bit source stays 16-bit
		{"pcm_f32le", "pcm_f32le"}, // a float source stays float, not quantized to integer
	}
	for _, c := range cases {
		t.Run(c.srcCodec, func(t *testing.T) {
			src := synthWav(t, dir, "src-"+c.srcCodec+".wav", "-c:a", c.srcCodec)
			out := filepath.Join(dir, "out-"+c.srcCodec+".wav")
			if _, err := r.Transcode(context.Background(), src, out, Spec{Codec: CodecWAV}); err != nil {
				t.Fatalf("Transcode WAV: %v", err)
			}
			pr, err := r.Probe(context.Background(), out)
			if err != nil {
				t.Fatalf("Probe(out): %v", err)
			}
			a, _ := pr.AudioStream()
			if a.CodecName != c.wantCodec {
				t.Errorf("WAV output codec = %q, want %q (no silent bit-depth change)", a.CodecName, c.wantCodec)
			}
		})
	}
}

func TestRunner_TranscodeLossless(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, 1)
	out := filepath.Join(dir, "out.flac")

	res, err := r.Transcode(context.Background(), in, out, Spec{Codec: CodecFLAC})
	if err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	if res.Output != out || res.Codec != CodecFLAC {
		t.Errorf("result = %+v, want output %q codec flac", res, out)
	}
	if res.Size <= 0 {
		t.Errorf("result size = %d, want > 0", res.Size)
	}

	pr, err := r.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("Probe(out): %v", err)
	}
	if a, _ := pr.AudioStream(); a.CodecName != "flac" {
		t.Errorf("output codec = %q, want flac", a.CodecName)
	}
	assertNoTempLeft(t, out)
}

func TestRunner_TranscodeLossy(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, 1)
	out := filepath.Join(dir, "out.mp3")

	if _, err := r.Transcode(context.Background(), in, out, Spec{Codec: CodecMP3, Bitrate: 128000}); err != nil {
		t.Fatalf("Transcode mp3: %v", err)
	}
	pr, err := r.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("Probe(out): %v", err)
	}
	if a, _ := pr.AudioStream(); a.CodecName != "mp3" {
		t.Errorf("output codec = %q, want mp3", a.CodecName)
	}
}

func TestRunner_TranscodeCopy(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	// A FLAC source remuxed (copied) into another FLAC container stays FLAC with
	// no re-encode.
	in := synthSine(t, dir, 1)
	flac := filepath.Join(dir, "src.flac")
	if _, err := r.Transcode(context.Background(), in, flac, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatalf("seed flac: %v", err)
	}
	out := filepath.Join(dir, "copy.flac")
	if _, err := r.Transcode(context.Background(), flac, out, Spec{Codec: CodecCopy}); err != nil {
		t.Fatalf("Transcode copy: %v", err)
	}
	pr, err := r.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("Probe(out): %v", err)
	}
	if a, _ := pr.AudioStream(); a.CodecName != "flac" {
		t.Errorf("copied codec = %q, want flac", a.CodecName)
	}
}

func TestRunner_TranscodeCopyWithFiltersRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, 1)
	out := filepath.Join(dir, "out.wav")

	_, err := r.Transcode(context.Background(), in, out, Spec{
		Codec:   CodecCopy,
		Filters: []string{"loudnorm=I=-14"},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("output should not exist after a rejected spec")
	}
	assertNoTempLeft(t, out)
}

func TestRunner_TranscodeFailureCleansTemp(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.wav")
	out := filepath.Join(dir, "out.flac")

	_, err := r.Transcode(context.Background(), missing, out, Spec{Codec: CodecFLAC})
	if err == nil {
		t.Fatal("Transcode(missing input) = nil error, want failure")
	}
	if _, ok := errors.AsType[*RunError](err); !ok {
		t.Fatalf("err = %v, want *RunError", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("failed transcode left an output file behind")
	}
	assertNoTempLeft(t, out)
}

func TestRunner_CancellationKillsProcess(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "out.flac")

	// A realtime-paced 30s sine keeps ffmpeg running long enough to cancel it.
	cmd := Command{Args: []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-re", "-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-c:a", "flac", out,
	}}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := r.Run(ctx, cmd)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	// The process was killed promptly: nowhere near the 30s input length, and
	// within the shutdown grace plus slack.
	if elapsed > 5*time.Second {
		t.Fatalf("cancellation took %v, want a prompt kill", elapsed)
	}
}
