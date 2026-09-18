package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// A span is sample-exact and its open form runs to whatever the source holds.
func TestRenderSpan(t *testing.T) {
	ctx := context.Background()
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := filepath.Join(dir, "rip.wav")
	if err := os.WriteFile(in, mediatest.SineWAV(6, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	const total = 6 * 44100

	samples := func(path string) int64 {
		t.Helper()
		pr, err := r.Probe(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		a, ok := pr.AudioStream()
		if !ok {
			t.Fatalf("%s holds no audio", path)
		}
		return a.Samples
	}

	bounded := filepath.Join(dir, "bounded.flac")
	if _, err := r.RenderSpan(ctx, in, bounded, 88200, 176400, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatal(err)
	}
	if got := samples(bounded); got != 88200 {
		t.Errorf("bounded span = %d samples, want 88200", got)
	}

	open := filepath.Join(dir, "open.flac")
	if _, err := r.RenderSpan(ctx, in, open, 176400, ToEnd, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatal(err)
	}
	if got, want := samples(open), int64(total-176400); got != want {
		t.Errorf("open span = %d samples, want %d", got, want)
	}

	if _, err := r.RenderSpan(ctx, in, filepath.Join(dir, "copy.wav"), 0, 100, Spec{Codec: CodecCopy}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("a copy span = %v, want ErrIncompatibleSpec", err)
	}
	if _, err := r.RenderSpan(ctx, in, filepath.Join(dir, "past.flac"), total+1, ToEnd, Spec{Codec: CodecFLAC}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("a span starting past the end = %v, want ErrIncompatibleSpec", err)
	}
}
