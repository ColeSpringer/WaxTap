package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// A Musepack input is a local source like any other lossy one: it decodes to
// the requested format, its APEv2 metadata rides the ordinary carry pass into
// the output, and nothing about the promotion warns (the source was lossy
// already). What it cannot be is written: a copy and an output carrying its
// extension both fail as incompatible specs, exit 2 at the CLI, before any
// engine call.
func TestProcessMusepackSource(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mpc")
	if err := os.WriteFile(in, mediatest.TaggedMPC(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out.flac")
	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.SourceFormat.Codec != "musepack" || res.SourceFormat.Extension != "mpc" {
		t.Errorf("source format = %+v, want musepack/mpc", res.SourceFormat)
	}
	if res.OutputFormat.Codec != "flac" || !res.Transcoded {
		t.Errorf("output format = %+v transcoded %v, want a flac encode", res.OutputFormat, res.Transcoded)
	}
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	for key, want := range mediatest.TaggedMPCTags {
		k, err := tag.ParseKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := doc.Get(k); !ok || got[0] != want {
			t.Errorf("carried %s = %v (ok %v), want %q", key, got, ok, want)
		}
	}

	// The promotion a cut into a foreign container makes is the shape that
	// warns for a lossless source (TestProcessWarnsImplicitLossyPromotion); a
	// source that was lossy already, which is what lossySource says of
	// musepack, must take the same Opus re-encode without the warning.
	cutOut := filepath.Join(dir, "cut.mka")
	res, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(cutOut), Cut: &CutSpec{Ranges: []TimeRange{{Start: 0, End: 50 * time.Millisecond}}}},
	})
	if err != nil {
		t.Fatalf("cut into .mka: %v", err)
	}
	if res.OutputFormat.Codec != "opus" || !res.Transcoded {
		t.Errorf("cut into .mka = %+v transcoded %v, want the container's Opus promotion", res.OutputFormat, res.Transcoded)
	}
	for _, w := range res.Warnings {
		if w.Code == WarnImplicitLossy {
			t.Errorf("warnings = %v; a source that was lossy already must not warn implicit-lossy", res.Warnings)
		}
	}

	// Keeping the packets under the source's own name is the natural copy
	// request, and the one Matroska's allowlist would not have caught: it is
	// refused because nothing writes Musepack, and the message says so.
	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "copy.mpc")), Transcode: &TranscodeSpec{Format: FormatCopy}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") || !strings.Contains(err.Error(), "--format") {
		t.Errorf("copy = %v, want ErrIncompatibleSpec naming the decode-only source and the --format escape", err)
	}
	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "out.mpc")), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("flac under .mpc = %v, want ErrIncompatibleSpec (the extension is refused, not force-muxed)", err)
	}
}
