package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	wferr "github.com/colespringer/waxflow/waxerr"

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
	if _, err := r.RenderSpan(ctx, in, bounded, 88200, 176400, Measured{}, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatal(err)
	}
	if got := samples(bounded); got != 88200 {
		t.Errorf("bounded span = %d samples, want 88200", got)
	}

	open := filepath.Join(dir, "open.flac")
	if _, err := r.RenderSpan(ctx, in, open, 176400, ToEnd, Measured{}, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatal(err)
	}
	if got, want := samples(open), int64(total-176400); got != want {
		t.Errorf("open span = %d samples, want %d", got, want)
	}

	if _, err := r.RenderSpan(ctx, in, filepath.Join(dir, "copy.wav"), 0, 100, Measured{}, Spec{Codec: CodecCopy}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("a copy span = %v, want ErrIncompatibleSpec", err)
	}
	if _, err := r.RenderSpan(ctx, in, filepath.Join(dir, "past.flac"), total+1, ToEnd, Measured{}, Spec{Codec: CodecFLAC}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("a span starting past the end = %v, want ErrIncompatibleSpec", err)
	}
}

// A span is held to the measured length, not the header's. An ASF rounds its
// duration to a millisecond and this fixture holds more than it declares, so a
// span in the gap is one the file serves and its own declaration refuses: it
// passes a plan checked against the measurement, and has to pass here too.
func TestRenderSpanHeldToTheMeasure(t *testing.T) {
	ctx := context.Background()
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	wma := writeFixture(t, dir, "in.wma", mediatest.ChapteredWMA())

	pr := mustProbe(t, r, wma)
	declared, _ := pr.AudioStream()
	length, err := r.MeasureLength(ctx, wma)
	if err != nil {
		t.Fatal(err)
	}
	if length.Samples <= declared.Samples+1 {
		t.Fatalf("measured %d against a declared %d; the fixture's header no longer under-declares", length.Samples, declared.Samples)
	}
	from := declared.Samples + (length.Samples-declared.Samples)/2

	unheld := filepath.Join(dir, "unheld.flac")
	if _, err := r.RenderSpan(ctx, wma, unheld, from, ToEnd, Measured{}, Spec{Codec: CodecFLAC}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("a span past the declaration, unmeasured = %v, want ErrIncompatibleSpec", err)
	}
	if _, serr := os.Stat(unheld); serr == nil {
		t.Error("a refused span left an output behind")
	}

	held := filepath.Join(dir, "held.flac")
	if _, err := r.RenderSpan(ctx, wma, held, from, ToEnd, length.Measured(), Spec{Codec: CodecFLAC}); err != nil {
		t.Fatalf("a span inside the measured length: %v", err)
	}
	out := mustProbe(t, r, held)
	if a, _ := out.AudioStream(); a.Samples != length.Samples-from {
		t.Errorf("the span holds %d samples, want the %d the file has past sample %d", a.Samples, length.Samples-from, from)
	}
}

// An open span is held to the measured length against a shortfall: a source
// that ends short of it, with no damage reported on the way, fails the span
// instead of writing it short. Whose length was wrong decides the failure. A
// length the file stated is the file's fault, bad input; one counted by
// decoding this same file is the engine disagreeing with itself, which is no
// statement about the file and is left at the engine's internal code (exit 1;
// see classifyEngineError).
func TestRenderSpanFailsAShortfallOfTheMeasure(t *testing.T) {
	ctx := context.Background()
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := writeFixture(t, dir, "rip.wav", mediatest.SineWAV(2, 2))
	const total = 2 * 44100

	stated := filepath.Join(dir, "stated.flac")
	_, err := r.RenderSpan(ctx, in, stated, 44100, ToEnd, Measured{Samples: total + 4410}, Spec{Codec: CodecFLAC})
	if !errors.Is(err, waxerr.ErrUnsupportedInput) {
		t.Errorf("a shortfall of a stated length = %v, want ErrUnsupportedInput", err)
	}
	counted := filepath.Join(dir, "counted.flac")
	_, err = r.RenderSpan(ctx, in, counted, 44100, ToEnd, Measured{Samples: total + 4410, Counted: true}, Spec{Codec: CodecFLAC})
	if wferr.CodeOf(err) != wferr.CodeInternal || errors.Is(err, waxerr.ErrUnsupportedInput) || errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("a shortfall of a counted length = %v, want the engine's internal code, unclassified", err)
	}
	for _, out := range []string{stated, counted} {
		if _, serr := os.Stat(out); serr == nil {
			t.Errorf("%s was written short", filepath.Base(out))
		}
	}
}

// No span is empty. One that names no samples is refused as the request it is,
// whichever bound made it so: an end at its start, or an open end starting
// exactly where the source ends, which the engine takes as a span of nothing.
// Nothing is written for either.
func TestRenderSpanRefusesAnEmptySpan(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := writeFixture(t, dir, "rip.wav", mediatest.SineWAV(1, 2))
	const total = 44100
	for name, span := range map[string][2]int64{
		"an end at its start":               {100, 100},
		"an open end at the source's end":   {total, ToEnd},
		"a bounded end at the source's end": {total, total},
	} {
		out := filepath.Join(dir, "empty.flac")
		_, err := r.RenderSpan(context.Background(), in, out, span[0], span[1], Measured{}, Spec{Codec: CodecFLAC})
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("%s = %v, want ErrIncompatibleSpec", name, err)
		}
		if _, serr := os.Stat(out); serr == nil {
			t.Errorf("%s was written", name)
		}
	}
}
