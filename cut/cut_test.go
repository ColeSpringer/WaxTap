package cut

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2/transcode"
	"github.com/colespringer/waxtap/v2/waxerr"
)

func TestResolveMode(t *testing.T) {
	flac := transcode.Spec{Codec: transcode.CodecFLAC}
	copyc := transcode.Spec{Codec: transcode.CodecCopy}
	copyWithFilters := transcode.Spec{Codec: transcode.CodecCopy, Filters: []string{"loudnorm=I=-14"}}
	flacWithFilters := transcode.Spec{Codec: transcode.CodecFLAC, Filters: []string{"loudnorm=I=-14"}}
	cases := []struct {
		name      string
		mode      Mode
		crossfade time.Duration
		enc       transcode.Spec
		want      Mode
		wantErr   bool
	}{
		{"smart-no-encode-copies", ModeSmart, 0, copyc, ModeCopy, false},
		{"smart-with-transcode-is-accurate", ModeSmart, 0, flac, ModeAccurate, false},
		{"smart-crossfade-needs-codec", ModeSmart, time.Second, copyc, 0, true},
		{"smart-crossfade-with-codec", ModeSmart, time.Second, flac, ModeAccurate, false},
		{"smart-filters-are-accurate", ModeSmart, 0, flacWithFilters, ModeAccurate, false},
		{"smart-filters-need-codec", ModeSmart, 0, copyWithFilters, 0, true},
		{"copy-no-crossfade", ModeCopy, 0, copyc, ModeCopy, false},
		{"copy-rejects-crossfade", ModeCopy, time.Second, copyc, 0, true},
		{"copy-rejects-filters", ModeCopy, 0, copyWithFilters, 0, true},
		{"copy-rejects-transcode", ModeCopy, 0, flac, 0, true},
		{"accurate-needs-codec", ModeAccurate, 0, copyc, 0, true},
		{"accurate-with-codec", ModeAccurate, 0, flac, ModeAccurate, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveMode(c.mode, c.crossfade, c.enc)
			if c.wantErr {
				if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
					t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("resolveMode = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolveMode_CrossfadeCopyNamesCrossfade(t *testing.T) {
	copyc := transcode.Spec{Codec: transcode.CodecCopy}
	for _, mode := range []Mode{ModeAccurate, ModeCopy} {
		_, err := resolveMode(mode, time.Second, copyc)
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Fatalf("mode %v: err = %v, want ErrIncompatibleSpec", mode, err)
		}
		if !strings.Contains(err.Error(), "crossfade") || !strings.Contains(err.Error(), "output format") {
			t.Errorf("mode %v: message = %q, want it to name the crossfade and the output format", mode, err)
		}
		if strings.Contains(err.Error(), "accurate cut") {
			t.Errorf("mode %v: message = %q, should not leak the internal accurate-cut mode", mode, err)
		}
	}
}

func TestValidateCrossfade(t *testing.T) {
	cases := []struct {
		name  string
		keeps []Range
		d     time.Duration
		ok    bool
	}{
		{"endpoints-fit", []Range{{0, s(2)}, {s(4), s(6)}}, time.Second, true},
		{"first-span-too-short", []Range{{0, 500 * time.Millisecond}, {s(4), s(6)}}, time.Second, false},
		{"interior-needs-double", []Range{{0, s(2)}, {s(3), s(4)}, {s(5), s(7)}}, time.Second, false},
		{"interior-fits-double", []Range{{0, s(3)}, {s(4), s(7)}, {s(8), s(11)}}, time.Second, true},
		{"single-span-ignored", []Range{{0, 500 * time.Millisecond}}, time.Second, true},
		{"no-crossfade", []Range{{0, 500 * time.Millisecond}, {s(4), s(6)}}, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateCrossfade(c.keeps, c.d)
			if c.ok && err != nil {
				t.Errorf("ValidateCrossfade = %v, want nil", err)
			}
			if !c.ok && !errors.Is(err, waxerr.ErrIncompatibleSpec) {
				t.Errorf("ValidateCrossfade = %v, want ErrIncompatibleSpec", err)
			}
		})
	}
}

func TestEncodeGraph(t *testing.T) {
	keeps := []Range{{0, s(10)}, {s(20), s(100)}}
	// No filters: the cut ends directly at [out].
	plain := encodeGraph(keeps, 0, s(100), nil)
	if plain != Graph(keeps, 0, s(100), "out") {
		t.Errorf("encodeGraph(no filters) = %q, want the plain cut graph", plain)
	}
	// With filters: the cut ends at [cut], then the filter chain runs to [out].
	fused := encodeGraph(keeps, 0, s(100), []string{"loudnorm=I=-14", "volume=2"})
	wantTail := ";[cut]loudnorm=I=-14,volume=2[out]"
	if !strings.HasSuffix(fused, wantTail) {
		t.Errorf("encodeGraph(filters) = %q, want suffix %q", fused, wantTail)
	}
	if !strings.HasPrefix(fused, Graph(keeps, 0, s(100), "cut")) {
		t.Errorf("encodeGraph(filters) should start with the cut graph ending at [cut]: %q", fused)
	}
}

func TestConcatEscape(t *testing.T) {
	cases := map[string]string{
		"/tmp/song.flac":   "'/tmp/song.flac'",
		"/tmp/it's a.flac": `'/tmp/it'\''s a.flac'`,
		"/tmp/a b/c.flac":  "'/tmp/a b/c.flac'",
	}
	for in, want := range cases {
		if got := concatEscape(in); got != want {
			t.Errorf("concatEscape(%q) = %q, want %q", in, got, want)
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

// synthSine writes a steady stereo sine of the given length to dir/name using
// the given encoder (e.g. "flac"). Fixtures are generated, never committed.
func synthSine(t *testing.T, dir, name string, seconds int, encoder string) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration=" + strconv.Itoa(seconds),
		"-ac", "2", "-c:a", encoder, out,
	}
	if b, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Fatalf("synth sine: %v: %s", err, b)
	}
	return out
}

func probeDuration(t *testing.T, r *transcode.Runner, path string) time.Duration {
	t.Helper()
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe(%s): %v", path, err)
	}
	return pr.Format.Duration
}

// assertDuration checks a probed duration is within tol of want.
func assertDuration(t *testing.T, got, want, tol time.Duration) {
	t.Helper()
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > tol {
		t.Errorf("duration = %v, want %v +/- %v", got, want, tol)
	}
}

func TestRender_AccurateRemovesMiddle(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 10, "flac")
	out := filepath.Join(dir, "out.flac")

	res, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{4 * time.Second, 6 * time.Second}},
		Mode:   ModeAccurate,
		Encode: transcode.Spec{Codec: transcode.CodecFLAC},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.Applied || res.Mode != ModeAccurate {
		t.Errorf("result = %+v, want Applied accurate", res)
	}
	assertDuration(t, res.Removed, 2*time.Second, 50*time.Millisecond)
	// Re-encode is sample-accurate, so a tight tolerance holds.
	assertDuration(t, probeDuration(t, r, out), 8*time.Second, 100*time.Millisecond)

	pr, _ := r.Probe(context.Background(), out)
	if a, _ := pr.AudioStream(); a.CodecName != "flac" {
		t.Errorf("output codec = %q, want flac", a.CodecName)
	}
}

func TestRender_CopyRemovesMiddle(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	// Matroska is used here because it reports the true post-copy duration; a
	// FLAC container keeps a stale STREAMINFO total-samples header after a stream
	// copy even though the audio is trimmed. Copy mode itself is codec-agnostic.
	in := synthSine(t, dir, "in.mka", 10, "flac")
	out := filepath.Join(dir, "out.mka")

	// Removing a middle span leaves two keep ranges: the multi-segment concat path.
	res, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{4 * time.Second, 6 * time.Second}},
		Mode:   ModeCopy,
	})
	if err != nil {
		t.Fatalf("Render(copy): %v", err)
	}
	if res.Mode != ModeCopy {
		t.Errorf("mode = %v, want copy", res.Mode)
	}
	// Stream copy snaps to frame boundaries, so allow a looser tolerance.
	assertDuration(t, probeDuration(t, r, out), 8*time.Second, 300*time.Millisecond)
}

func TestRender_CopyEdgeTrim(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.mka", 10, "flac")
	out := filepath.Join(dir, "out.mka")

	// A single leading removal leaves one keep span: the single-segment copy path.
	res, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{0, 3 * time.Second}},
		Mode:   ModeCopy,
	})
	if err != nil {
		t.Fatalf("Render(copy edge trim): %v", err)
	}
	if !res.Applied || res.Mode != ModeCopy {
		t.Errorf("result = %+v, want Applied copy", res)
	}
	assertDuration(t, probeDuration(t, r, out), 7*time.Second, 300*time.Millisecond)
}

func TestRender_Crossfade(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 10, "flac")
	out := filepath.Join(dir, "out.flac")

	// Keep [0,4) and [6,10): two 4s spans, crossfaded 1s gives about 7s.
	res, err := Render(context.Background(), r, in, out, Spec{
		Remove:    []Range{{4 * time.Second, 6 * time.Second}},
		Mode:      ModeAccurate,
		Crossfade: time.Second,
		Encode:    transcode.Spec{Codec: transcode.CodecFLAC},
	})
	if err != nil {
		t.Fatalf("Render(crossfade): %v", err)
	}
	if !res.Applied {
		t.Fatal("crossfade cut should be applied")
	}
	assertDuration(t, probeDuration(t, r, out), 7*time.Second, 150*time.Millisecond)
}

func TestRender_SmartFusesWithTranscode(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 10, "flac")
	out := filepath.Join(dir, "out.mp3")

	res, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{4 * time.Second, 6 * time.Second}},
		Mode:   ModeSmart,
		Encode: transcode.Spec{Codec: transcode.CodecMP3, Bitrate: 128000},
	})
	if err != nil {
		t.Fatalf("Render(smart+transcode): %v", err)
	}
	if res.Mode != ModeAccurate {
		t.Errorf("smart with a transcode should resolve to accurate, got %v", res.Mode)
	}
	pr, _ := r.Probe(context.Background(), out)
	if a, _ := pr.AudioStream(); a.CodecName != "mp3" {
		t.Errorf("output codec = %q, want mp3", a.CodecName)
	}
}

func TestRender_NoOp(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 5, "flac")
	out := filepath.Join(dir, "out.flac")

	// A removal entirely outside the media clamps to nothing.
	res, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{30 * time.Second, 40 * time.Second}},
		Mode:   ModeCopy,
	})
	if err != nil {
		t.Fatalf("Render(no-op): %v", err)
	}
	if res.Applied {
		t.Errorf("result = %+v, want Applied=false", res)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a no-op cut must not write output")
	}
}

func TestRender_FusesEncodeFilters(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 10, "flac")
	out := filepath.Join(dir, "out.flac")

	// atempo=2.0 halves the duration. Removing [4,6] keeps 8s; if the filter is
	// fused (not dropped) the output is ~4s, proving Encode.Filters survive.
	res, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{4 * time.Second, 6 * time.Second}},
		Mode:   ModeAccurate,
		Encode: transcode.Spec{Codec: transcode.CodecFLAC, Filters: []string{"atempo=2.0"}},
	})
	if err != nil {
		t.Fatalf("Render(fused filters): %v", err)
	}
	if !res.Applied {
		t.Fatal("cut should be applied")
	}
	// 8s of kept audio at 2x tempo gives about 4s; a dropped filter leaves ~8s.
	assertDuration(t, probeDuration(t, r, out), 4*time.Second, 200*time.Millisecond)
}

func TestRender_CrossfadeTooLongRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 5, "flac")
	out := filepath.Join(dir, "out.flac")

	// Removing [0.5s,4s] leaves a 0.5s first span; a 1s crossfade cannot fit and
	// would otherwise make ffmpeg emit an empty file with exit 0.
	_, err := Render(context.Background(), r, in, out, Spec{
		Remove:    []Range{{500 * time.Millisecond, 4 * time.Second}},
		Mode:      ModeAccurate,
		Crossfade: time.Second,
		Encode:    transcode.Spec{Codec: transcode.CodecFLAC},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a rejected crossfade must not write output")
	}
}

func TestRender_CopyWithTranscodeRejected(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 5, "flac")
	out := filepath.Join(dir, "out.mp3")

	// Copy mode with a real output codec is contradictory: copy means no encode,
	// so the MP3 request would otherwise be silently dropped (or fail in ffmpeg).
	_, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{1 * time.Second, 2 * time.Second}},
		Mode:   ModeCopy,
		Encode: transcode.Spec{Codec: transcode.CodecMP3},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a rejected copy+transcode must not write output")
	}
}

func TestRender_WholeTrackRemoved(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 5, "flac")
	out := filepath.Join(dir, "out.flac")

	_, err := Render(context.Background(), r, in, out, Spec{
		Remove: []Range{{0, 60 * time.Second}},
		Mode:   ModeCopy,
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
}
