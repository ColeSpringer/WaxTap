package media

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxlabel"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// TestDecoderRegistryParity pins the two codec tables to the engine's decoder
// registry: every codec WaxFlow decodes is either remuxable (codecToFormat) or
// decode-only (decodeOnlyCodecs), never both and never neither, PCM excepted
// (its packets take their layout from the container, so no copy survives and
// remuxDeclined has its own wording; TestPCMRemuxDeclined). A decoder
// registered upstream that lands nowhere fails here, so it gets classified on
// purpose (the lossy tables, the batch walk, the refusal wording) rather than
// declining by default.
func TestDecoderRegistryParity(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range format.Decoders() {
		name := codecName(id)
		seen[name] = true
		_, remux := codecToFormat(id)
		_, decodeOnly := DecodeOnlyCodec(name)
		switch {
		case id == codec.PCM:
			if remux || decodeOnly {
				t.Errorf("pcm: remuxable=%v decode-only=%v; PCM is neither, by its own rule", remux, decodeOnly)
			}
		case remux == decodeOnly:
			t.Errorf("the engine decodes %q: remuxable=%v decode-only=%v; a decoder is exactly one of the two, decide and add it", name, remux, decodeOnly)
		}
	}
	for name := range decodeOnlyCodecs {
		if !seen[name] {
			t.Errorf("decodeOnlyCodecs names %q, which the engine no longer decodes; drop the entry", name)
		}
	}
}

// Every extension WaxFlow registers for a container it only reads is refused
// as an output name by every gate in this package, whatever the codec, rather
// than force-muxed: FLAC bytes under out.mpp would be a file every player
// reads as Musepack. The list itself is pinned by hand to WaxFlow's
// format/registry.go, which exports no extension list.
func TestDecodeOnlyOutputExtensionsRejected(t *testing.T) {
	exts := DecodeOnlyExts()
	if want := []string{"asf", "mp+", "mpc", "mpp", "wma"}; !slices.Equal(exts, want) {
		t.Fatalf("DecodeOnlyExts() = %v, want the engine's spellings %v", exts, want)
	}
	if name, ok := DecodeOnlyContainer(".MPC"); !ok || name != "Musepack" {
		t.Errorf("DecodeOnlyContainer(\".MPC\") = %q, %v; want Musepack, true (dotted, any case)", name, ok)
	}
	if _, ok := DecodeOnlyContainer("flac"); ok {
		t.Error("DecodeOnlyContainer(\"flac\") = true")
	}
	for _, ext := range exts {
		out := "out." + ext
		if needsForcedMuxer(out) {
			t.Errorf("needsForcedMuxer(%q) = true; the extension must constrain the output, not be muxed over", out)
		}
		for _, name := range []string{"flac", "aac", "opus", "pcm", "wma", "musepack"} {
			if ContainerAccepts(ext, name) {
				t.Errorf("ContainerAccepts(%q, %q) = true, want false: nothing WaxTap writes may carry the name", ext, name)
			}
		}
		for _, c := range []Codec{CodecFLAC, CodecMP3, CodecOpus, CodecWAV} {
			err := CheckOutputContainer(c, out)
			if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") {
				t.Errorf("CheckOutputContainer(%v, %q) = %v, want ErrIncompatibleSpec naming the read-only container", c, out, err)
			}
		}
	}
}

// mpcFixture writes the tagged Musepack fixture into dir and returns its path.
func mpcFixture(t *testing.T, dir string) string {
	t.Helper()
	in := filepath.Join(dir, "in.mpc")
	if err := os.WriteFile(in, mediatest.TaggedMPC(), 0o644); err != nil {
		t.Fatal(err)
	}
	return in
}

// A Musepack source rides the ordinary decode path: the probe names it, its
// APEv2 tag reaches the probe, a lossless encode delivers every sample, and
// the two requests that would need a Musepack writer (a remux, and a copy-cut
// with no same-family encoder to fall back to) decline in WaxTap's own words
// before the engine is asked for a format it does not have.
func TestMusepackSourceDecodesAndDeclinesCopy(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	ctx := context.Background()
	dir := t.TempDir()
	in := mpcFixture(t, dir)

	pr := mustProbe(t, r, in)
	a, _ := pr.AudioStream()
	if a.CodecName != "musepack" || pr.Format.Container != "musepack" {
		t.Errorf("probe = codec %q in %q, want musepack in musepack", a.CodecName, pr.Format.Container)
	}
	if a.Samples != mediatest.TaggedMPCSamples || a.SampleRate != mediatest.TaggedMPCRate || a.Channels != 2 {
		t.Errorf("stream = %+v, want %d samples at %d Hz stereo", a, mediatest.TaggedMPCSamples, mediatest.TaggedMPCRate)
	}
	for key, want := range mediatest.TaggedMPCTags {
		if got := tagValue(pr, key); got != want {
			t.Errorf("probe tag %s = %q, want %q", key, got, want)
		}
	}

	out := filepath.Join(dir, "out.flac")
	if _, err := r.Transcode(ctx, in, out, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if o, _ := mustProbe(t, r, out).AudioStream(); o.CodecName != "flac" || o.Samples != mediatest.TaggedMPCSamples {
		t.Errorf("output = %+v, want flac holding every source sample", o)
	}

	_, err := r.Transcode(ctx, in, filepath.Join(dir, "copy.out"), Spec{Codec: CodecCopy})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "cannot remux musepack audio") || !strings.Contains(err.Error(), "pass --format") {
		t.Errorf("remux = %v, want ErrIncompatibleSpec naming musepack, the reason, and the escape", err)
	}

	cut := CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: 100 * time.Millisecond}},
		Total:   mediatest.TaggedMPCDuration,
		CopyCut: true,
		Encode:  Spec{Codec: CodecCopy},
	}
	_, err = r.Render(ctx, in, filepath.Join(dir, "cut.mka"), cut)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "pass an explicit format") {
		t.Errorf("copy-cut = %v, want ErrIncompatibleSpec naming the --format escape", err)
	}
	cut.Encode = Spec{Codec: CodecFLAC}
	res, err := r.Render(ctx, in, filepath.Join(dir, "cut.flac"), cut)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if res.Mode != ModeAccurate {
		t.Errorf("cut mode = %v, want ModeAccurate (Musepack has no packet cut)", res.Mode)
	}
}

// The read-side half of the invariant TestWaxLabelReadsEveryEngineOutput pins:
// carryTags parses a source through WaxLabel alone, so the decode-only inputs
// must be ones WaxLabel identifies too. Musepack is pinned here on the same
// fixture the engine decodes; WMA has no fixture (nothing here can write one)
// and keeps its read side pinned upstream.
func TestWaxLabelReadsMusepackInput(t *testing.T) {
	in := mpcFixture(t, t.TempDir())
	doc, err := waxlabel.ParseFile(context.Background(), in)
	if err != nil {
		t.Fatalf("WaxLabel cannot parse the Musepack fixture the engine decodes: %v", err)
	}
	if doc.Format() != waxlabel.FormatMusepack {
		t.Errorf("format = %v, want %v", doc.Format(), waxlabel.FormatMusepack)
	}
	if doc.Tags().Len() == 0 {
		t.Error("WaxLabel read no tags from the fixture's APEv2 block")
	}
}
