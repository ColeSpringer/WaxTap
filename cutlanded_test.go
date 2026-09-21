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

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// segmentedOpus writes a 3 s Opus fixture whose middle second is silent, so a
// cut that keeps only the outer seconds can be told apart from one that leaked
// the removed span back in.
func segmentedOpus(t *testing.T, dir, name string) string {
	t.Helper()
	wav := filepath.Join(t.TempDir(), "seg.wav")
	if err := os.WriteFile(wav, mediatest.SegmentedWAV(2, 48000,
		mediatest.Segment{Seconds: 1, FreqHz: 440}, mediatest.Segment{Seconds: 1}, mediatest.Segment{Seconds: 1, FreqHz: 880}), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	r := media.NewRunner(media.RunnerConfig{})
	if _, err := r.Transcode(context.Background(), wav, out, media.Spec{Codec: media.CodecOpus}); err != nil {
		t.Fatalf("synth %s: %v", name, err)
	}
	return out
}

// A packet copy reports the mode it ran in and the joins it moved, and it
// warns once naming both. The figures ride on the result so a consumer reads
// numbers rather than parsing the sentence.
func TestProcessCopyCutReportsTheSnappedJoins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := segmentedOpus(t, dir, "in.opus")
	out := filepath.Join(dir, "cut.opus")

	// Both edges sit off the 20 ms Opus grid, so both interior joins snap.
	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{Input: in, ProcessSpec: ProcessSpec{
		Cut:    &CutSpec{Ranges: []TimeRange{{Start: 1010 * time.Millisecond, End: 2010 * time.Millisecond}}},
		Output: ToFile(out),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.CutApplied || res.CutMode != CutCopy || res.Transcoded {
		t.Fatalf("CutApplied = %v, CutMode = %v, Transcoded = %v, want a packet copy", res.CutApplied, res.CutMode, res.Transcoded)
	}
	// One removed range is one join, whichever of its two edges moved.
	if res.CutSnaps != 1 || res.CutSnapMax <= 0 || res.CutSnapMax >= 20*time.Millisecond {
		t.Errorf("CutSnaps = %d, CutSnapMax = %v, want the 1 join under 20 ms", res.CutSnaps, res.CutSnapMax)
	}
	w, ok := findWarning(res.Warnings, WarnCutSnapped)
	if !ok || !strings.Contains(w.Detail, "1 join") {
		t.Errorf("warnings = %v, want one cut-snapped naming 1 join", res.Warnings)
	}
}

// Chapters carried through a copy cut land on the timeline the output really
// holds, not on the one the request described: a mark after the cut moves by
// the audio that actually went.
func TestProcessCopyCutRemapsChaptersOntoTheLandedTimeline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := segmentedOpus(t, dir, "in.opus")
	doc, err := waxlabel.ParseFile(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	plan, perr := doc.Edit().SetChapters(
		waxlabel.Chapter{Start: 0, Title: "One"},
		waxlabel.Chapter{Start: 2500 * time.Millisecond, Title: "Two"},
	).Prepare()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "cut.opus")
	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{Input: in, ProcessSpec: ProcessSpec{
		Cut:    &CutSpec{Ranges: []TimeRange{{Start: 1010 * time.Millisecond, End: 2010 * time.Millisecond}}},
		Output: ToFile(out),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.CutSnaps == 0 {
		t.Fatal("the fixture was meant to snap; the remap below proves nothing without it")
	}
	// The output's own length says how much audio really went, so the
	// expectation is computed from the file rather than from the request.
	removed := 3*time.Second - res.OutputFormat.Duration
	chs := mustChapters(t, ctx, out)
	if len(chs) != 2 {
		t.Fatalf("output carries %d chapters, want 2", len(chs))
	}
	if want, got := 2500*time.Millisecond-removed, chs[1].Start; got < want-time.Millisecond || got > want+time.Millisecond {
		t.Errorf("chapter two starts at %v, want %v (2.5s less the %v the cut really removed)", got, want, removed)
	}
}

// copy-exact needs a container that can state a trim per packet, and delivers
// exact interior tails: only the head of the second span moves.
func TestProcessCopyExactSplicesTheInteriorTails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := segmentedOpus(t, dir, "in.opus")
	c := newOfflineClient(t)
	cut := func(out string) (*Result, error) {
		return c.Process(ctx, ProcessRequest{Input: in, ProcessSpec: ProcessSpec{
			Cut: &CutSpec{
				Ranges: []TimeRange{{Start: 1010 * time.Millisecond, End: 2010 * time.Millisecond}},
				Mode:   CutCopyExact,
			},
			Output: ToFile(out),
		}})
	}

	if _, err := cut(filepath.Join(dir, "no.opus")); !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), ".mka") {
		t.Fatalf("err = %v, want ErrIncompatibleSpec naming the Matroska outputs", err)
	}

	res, err := cut(filepath.Join(dir, "yes.mka"))
	if err != nil {
		t.Fatal(err)
	}
	if res.CutMode != CutCopyExact || res.Transcoded {
		t.Fatalf("CutMode = %v, Transcoded = %v, want a spliced packet copy", res.CutMode, res.Transcoded)
	}
	// The first span's tail is exact under the splice, so only the second
	// span's head moved.
	if res.CutSnaps != 1 {
		t.Errorf("CutSnaps = %d, want 1 (the second span's head; the first span's tail is exact)", res.CutSnaps)
	}
	w, ok := findWarning(res.Warnings, WarnCutSnapped)
	if !ok || !strings.Contains(w.Detail, "1 join") {
		t.Errorf("warnings = %v, want one cut-snapped naming 1 join", res.Warnings)
	}
}

func mustChapters(t *testing.T, ctx context.Context, path string) []waxlabel.Chapter {
	t.Helper()
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		t.Fatalf("parse %s: %v", filepath.Base(path), err)
	}
	return doc.Chapters()
}

// A smart cut that could not copy packets and decoded a lossy source says
// so, once, naming the encoder, the generation, and the reason. A lossless
// fallback loses nothing and says nothing, and a packet copy has nothing to
// say.
func TestProcessSmartCutSaysWhenItDecoded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	encode := func(out string, f TranscodeFormat) string {
		t.Helper()
		p := filepath.Join(dir, out)
		if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{Output: ToFile(p), Transcode: &TranscodeSpec{Format: f}}}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cut := func(in, out string) *Result {
		t.Helper()
		res, err := c.Process(ctx, ProcessRequest{Input: in, ProcessSpec: ProcessSpec{
			Output: ToFile(filepath.Join(dir, out)),
			Cut:    &CutSpec{Ranges: []TimeRange{{Start: time.Second, End: 2 * time.Second}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := cut(encode("in.mp3", FormatMP3), "cut.mp3")
	if !res.Transcoded || res.CutMode != CutAccurate {
		t.Fatalf("Transcoded = %v, CutMode = %v, want the family re-encode", res.Transcoded, res.CutMode)
	}
	if w, ok := findWarning(res.Warnings, WarnCutDecoded); !ok || !strings.Contains(w.Detail, "mp3") || !strings.Contains(w.Detail, "cannot be cut in place") || !strings.Contains(w.Detail, "second lossy generation") {
		t.Errorf("warnings = %v, want cut-decoded naming mp3, the reason, and the generation", res.Warnings)
	}
	if _, ok := findWarning(res.Warnings, WarnImplicitLossy); ok {
		t.Errorf("implicit-lossy fired beside cut-decoded: the container carries mp3")
	}
	if res := cut(encode("in.flac", FormatFLAC), "cut.flac"); res.Transcoded {
		if _, ok := findWarning(res.Warnings, WarnCutDecoded); ok {
			t.Errorf("a lossless fallback warned: %v", res.Warnings)
		}
	}
	if res := cut(segmentedOpus(t, dir, "in.opus"), "cut.opus"); res.Transcoded {
		t.Errorf("an opus cut was decoded")
	} else if _, ok := findWarning(res.Warnings, WarnCutDecoded); ok {
		t.Errorf("a packet copy warned: %v", res.Warnings)
	}
}
