package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxlabel"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// The Q7.8 scale: 256 units per dB, rounded to the nearest step.
func TestOpusGainQ78Scale(t *testing.T) {
	for _, tc := range []struct {
		db float64
		q  int
	}{{-3.5, -896}, {0.001, 0}, {0, 0}, {1, 256}} {
		if got := OpusGainQ78(tc.db); got != tc.q {
			t.Errorf("OpusGainQ78(%v) = %d, want %d", tc.db, got, tc.q)
		}
	}
	if got := OpusGainDB(-896); got != -3.5 {
		t.Errorf("OpusGainDB(-896) = %v, want -3.5", got)
	}
}

// A gain remux copies the packets and states the gain in the head, in every
// container that carries one, and a second remux moves the gain the first
// wrote rather than replacing it.
func TestRemuxWithOpusGainWritesTheHead(t *testing.T) {
	ctx := context.Background()
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	src := encodedFixture(t, dir, CodecOpus)

	if q, err := OpusHeaderGain(ctx, src); err != nil || q != 0 {
		t.Fatalf("a fresh encode states gain %d (%v), want 0", q, err)
	}

	out := filepath.Join(dir, "out.opus")
	if _, gain, err := r.RemuxWithOpusGain(ctx, src, out, -896); err != nil || gain != -896 {
		t.Fatalf("RemuxWithOpusGain = %d, %v; want -896", gain, err)
	}
	if q, err := OpusHeaderGain(ctx, out); err != nil || q != -896 {
		t.Errorf("OpusHeaderGain = %d (%v), want -896", q, err)
	}
	// An independent read of the same field, through WaxLabel rather than the
	// engine that wrote it.
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if got := doc.Properties().Tracks[0].OutputGain; got != -896 {
		t.Errorf("waxlabel reads gain %d, want -896", got)
	}

	// The packets are untouched: everything after the first Ogg page is the
	// input's bytes.
	if a, b := tailAfterFirstPage(t, src), tailAfterFirstPage(t, out); !bytes.Equal(a, b) {
		t.Errorf("the packet tail changed: %d bytes in, %d out", len(a), len(b))
	}

	again := filepath.Join(dir, "again.opus")
	if _, gain, err := r.RemuxWithOpusGain(ctx, out, again, -256); err != nil || gain != -1152 {
		t.Errorf("a second remux = %d, %v; want the source's own gain moved to -1152", gain, err)
	}

	for _, name := range []string{"out.webm", "out.mka"} {
		p := filepath.Join(dir, name)
		if _, gain, err := r.RemuxWithOpusGain(ctx, src, p, -512); err != nil || gain != -512 {
			t.Fatalf("%s: RemuxWithOpusGain = %d, %v", name, gain, err)
		}
		if q, err := OpusHeaderGain(ctx, p); err != nil || q != -512 {
			t.Errorf("%s: OpusHeaderGain = %d (%v), want -512", name, q, err)
		}
	}

	flac := encodedFixture(t, dir, CodecFLAC)
	if _, _, err := r.RemuxWithOpusGain(ctx, flac, filepath.Join(dir, "no.opus"), -256); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("a FLAC source = %v, want ErrIncompatibleSpec", err)
	}
	if _, err := OpusHeaderGain(ctx, flac); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("OpusHeaderGain of a FLAC = %v, want ErrIncompatibleSpec", err)
	}
}

// tailAfterFirstPage returns everything from the second OggS page onward.
func tailAfterFirstPage(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	i := bytes.Index(b[4:], []byte("OggS"))
	if i < 0 {
		t.Fatalf("%s holds one Ogg page", path)
	}
	return b[i+4:]
}
