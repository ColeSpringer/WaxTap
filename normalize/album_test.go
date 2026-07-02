package normalize

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2/cut"
	"github.com/colespringer/waxtap/v2/transcode"
)

func TestAlbumGainFilter(t *testing.T) {
	// A uniform offset of target - albumIntegrated, as a volume filter.
	if got := AlbumGainFilter(-14, -20); got != "volume=6.00dB" {
		t.Errorf("AlbumGainFilter(-14,-20) = %q, want volume=6.00dB", got)
	}
	if got := AlbumGainFilter(-14, -8); got != "volume=-6.00dB" {
		t.Errorf("AlbumGainFilter(-14,-8) = %q, want volume=-6.00dB", got)
	}
	// Non-finite measurement => pass-through, never a malformed filter.
	if g := AlbumGainFilter(-14, math.Inf(-1)); g != passthroughFilter {
		t.Errorf("AlbumGainFilter(non-finite) = %q, want %q", g, passthroughFilter)
	}
	if g := AlbumGainFilter(math.NaN(), -20); g != passthroughFilter {
		t.Errorf("AlbumGainFilter(NaN target) = %q, want %q", g, passthroughFilter)
	}
}

func TestAlbumMeasureFormatGraphShape(t *testing.T) {
	// Guard the conform-before-concat shape so a future edit cannot silently drop
	// the per-input aformat that lets mixed-rate albums concat.
	if !strings.Contains(albumMeasureFormat, "channel_layouts=stereo") {
		t.Errorf("album measure format missing stereo conform: %q", albumMeasureFormat)
	}
}

func TestMeasureAlbumEmpty(t *testing.T) {
	if _, _, err := MeasureAlbum(context.Background(), nil, nil); err == nil {
		t.Error("MeasureAlbum(nil) should error")
	}
}

func TestMeasureComplex(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	in := synthSine(t, dir, 4)

	// Cut the middle second, then measure the post-cut audio through the cut graph.
	total := probeDur(t, r, in)
	keeps := cut.Keeps([]cut.Range{{Start: time.Second, End: 2 * time.Second}}, total)
	graph := cut.Graph(keeps, 0, total, "pre")

	l, err := MeasureComplex(context.Background(), r, in, graph, "pre", 0)
	if err != nil {
		t.Fatalf("MeasureComplex: %v", err)
	}
	if !l.Finite() {
		t.Fatalf("measurement not finite: %+v", l)
	}
	if l.IntegratedLUFS < -30 || l.IntegratedLUFS > 0 {
		t.Errorf("integrated loudness = %v LUFS, implausible", l.IntegratedLUFS)
	}
}

func TestMeasureAlbumIntegration(t *testing.T) {
	r := newTestRunner(t)
	dir := t.TempDir()
	a := synthSine(t, dir, 2)                               // -6dB sine (in.wav)
	b := synthSineNamed(t, dir, "b.wav", 2, "volume=-18dB") // quieter

	album, per, err := MeasureAlbum(context.Background(), r, []string{a, b})
	if err != nil {
		t.Fatalf("MeasureAlbum: %v", err)
	}
	if len(per) != 2 {
		t.Fatalf("per-track count = %d, want 2", len(per))
	}
	if !album.Finite() || !per[0].Finite() || !per[1].Finite() {
		t.Fatalf("non-finite measurements: album=%+v per=%+v", album, per)
	}
	// The quieter track must measure lower than the louder one.
	if per[1].IntegratedLUFS >= per[0].IntegratedLUFS {
		t.Errorf("quieter track %v not below louder %v", per[1].IntegratedLUFS, per[0].IntegratedLUFS)
	}
	// The group measurement falls between the two tracks (not a track copy).
	lo, hi := per[1].IntegratedLUFS, per[0].IntegratedLUFS
	if album.IntegratedLUFS < lo-1 || album.IntegratedLUFS > hi+1 {
		t.Errorf("album loudness %v not between %v and %v", album.IntegratedLUFS, lo, hi)
	}
}

func probeDur(t *testing.T, r *transcode.Runner, path string) time.Duration {
	t.Helper()
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return pr.Format.Duration
}

// synthSineNamed writes a stereo sine of the given length and per-channel filter
// (e.g. "volume=-18dB") to dir/name. Fixtures are generated, never committed.
func synthSineNamed(t *testing.T, dir, name string, seconds int, af string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	out := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration=" + strconv.Itoa(seconds),
		"-af", af, "-ac", "2", out,
	}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("synth sine: %v: %s", err, b)
	}
	return out
}
