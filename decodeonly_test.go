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

// The decode-only inputs (Musepack, WMA) through the facade: each decodes to
// the requested format, its metadata rides the ordinary carry pass into the
// output, and neither can be written.

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

// A chaptered Musepack source carries its SV8 chapter packets, which nothing
// but the reference editor writes and WaxLabel reads since v1.6.2, onto an
// output that stores chapters, and a cut remaps them like any other carried
// chapter set: a chapter whose content the cut removed is dropped, the rest
// shift by the audio removed before them, and the start-only form (End zero,
// the untitled last chapter included) survives the remap. The items a
// chapter tag carries beside its title (Middle has an artist and a track
// number) are the chapter's metadata, not the file's, and must not surface
// as output tags.
func TestProcessCarriesMusepackChapters(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mpc")
	if err := os.WriteFile(in, mediatest.ChapteredMPC(), 0o644); err != nil {
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
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a whole chapter carry warned: %+v", res.Warnings)
	}
	doc := parseOutput(t, out)
	assertChapters(t, "transcode", doc.Chapters(), mediatest.ChapteredMPCChapters())
	if n := doc.Tags().Len(); n != 0 {
		t.Errorf("output carries %d tags, want none: a chapter tag's items are the chapter's, not the file's", n)
	}

	// Removing the first 200 ms erases Intro (0 to 181 ms) entirely and
	// shifts the rest by 200 ms, Middle clamped to the new start.
	cutOut := filepath.Join(dir, "cut.flac")
	res, err = c.Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(cutOut),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 0, End: 200 * time.Millisecond}}},
		},
	})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a remapped chapter carry warned: %+v", res.Warnings)
	}
	src := mediatest.ChapteredMPCChapters()
	assertChapters(t, "cut", parseOutput(t, cutOut).Chapters(), []waxlabel.Chapter{
		{Start: 0, Title: "Middle"},
		{Start: src[2].Start - 200*time.Millisecond, Title: "Coda"},
		{Start: src[3].Start - 200*time.Millisecond, Title: ""},
	})
}

// A WMA input is a local source like any other lossy one: it decodes to the
// requested format, and its tags and Marker Object chapters (which WaxLabel
// reads since v1.6.2) ride the ordinary carry pass into the output. The
// muxer's encoder stamp is excluded by policy rather than dropped, so nothing
// warns and the output carries exactly the title. A cut remaps the markers
// like any other chapter set. What a WMA file cannot be is written: a copy
// and an output carrying its extension both fail as incompatible specs, exit
// 2 at the CLI, before any engine call.
func TestProcessWMASource(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wma")
	if err := os.WriteFile(in, mediatest.ChapteredWMA(), 0o644); err != nil {
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
	if res.SourceFormat.Codec != "wma" || res.SourceFormat.Extension != "wma" {
		t.Errorf("source format = %+v, want wma/wma", res.SourceFormat)
	}
	if res.OutputFormat.Codec != "flac" || !res.Transcoded {
		t.Errorf("output format = %+v transcoded %v, want a flac encode", res.OutputFormat, res.Transcoded)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a whole carry warned: %+v", res.Warnings)
	}
	doc := parseOutput(t, out)
	assertTags(t, doc, mediatest.ChapteredWMATags)
	assertChapters(t, "transcode", doc.Chapters(), mediatest.ChapteredWMAChapters())

	// Removing [250 ms, 750 ms) cuts into Intro (0 to 500 ms), lands Mïddle
	// (500 ms to 1.25 s) on the join, and shifts Coda by the 500 ms removed.
	cutOut := filepath.Join(dir, "cut.flac")
	res, err = c.Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(cutOut),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 250 * time.Millisecond, End: 750 * time.Millisecond}}},
		},
	})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a remapped marker carry warned: %+v", res.Warnings)
	}
	assertChapters(t, "cut", parseOutput(t, cutOut).Chapters(), []waxlabel.Chapter{
		{Start: 0, Title: "Intro"},
		{Start: 250 * time.Millisecond, Title: "Mïddle"},
		{Start: 750 * time.Millisecond, Title: "Coda"},
	})

	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "copy.wma")), Transcode: &TranscodeSpec{Format: FormatCopy}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") || !strings.Contains(err.Error(), "--format") {
		t.Errorf("copy = %v, want ErrIncompatibleSpec naming the decode-only source and the --format escape", err)
	}
	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "out.wma")), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("flac under .wma = %v, want ErrIncompatibleSpec (the extension is refused, not force-muxed)", err)
	}
}

// Album mode cannot take a WMA input yet, and the failure is upstream's: WMA
// carries no padding count, so the engine's decoder delivers a frame's tail
// past the declared length (the media package's WMA test pins the tail), and
// the engine's album timeline refuses a member that delivers more audio than
// its headers declared. Per-track processing never concatenates and is
// unaffected. WaxTap names the track and says what to do in its own words,
// since the engine's read like a corrupt file. The pin keeps the README's
// limitation true and makes an upstream fix visible: an album of WMA tracks
// succeeding means WaxFlow resolved it, and this test, the wording in
// albumTrackError, and the README note go.
func TestProcessAlbumWMAIsUpstreamLimited(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wma")
	if err := os.WriteFile(in, mediatest.ChapteredWMA(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := newOfflineClient(t).ProcessAlbum(context.Background(),
		[]AlbumTrack{{Input: in, Output: filepath.Join(dir, "out.flac")}}, -18, TranscodeSpec{Format: FormatFLAC})
	if !errors.Is(err, ErrUnsupportedInput) || !strings.Contains(err.Error(), "track in.wma") || !strings.Contains(err.Error(), "one at a time") {
		t.Errorf("album of a WMA track = %v, want ErrUnsupportedInput naming the track and the one-at-a-time escape (an album succeeding means WaxFlow fixed it: retire this pin, the wording, and the README note)", err)
	}
}

// parseOutput reads the metadata a process left on path.
func parseOutput(t *testing.T, path string) *waxlabel.Document {
	t.Helper()
	doc, err := waxlabel.ParseFile(t.Context(), path)
	if err != nil {
		t.Fatalf("parse %s: %v", filepath.Base(path), err)
	}
	return doc
}

// assertTags compares the tags a process left on doc against want, exactly:
// every key with its one value, and nothing else.
func assertTags(t *testing.T, doc *waxlabel.Document, want map[string]string) {
	t.Helper()
	if n := doc.Tags().Len(); n != len(want) {
		t.Errorf("output carries %d tags, want %d: %v", n, len(want), want)
	}
	for key, val := range want {
		k, err := tag.ParseKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := doc.Get(k); len(got) != 1 || got[0] != val {
			t.Errorf("carried %s = %q, want the one value %q", key, got, val)
		}
	}
}

// assertChapters compares a carried chapter set against the source's. The
// output's chapter form keeps milliseconds (VorbisComment CHAPTERxxx), so a
// start within that of the source's counts as carried; every other field,
// End included, must come back as the source had it.
func assertChapters(t *testing.T, step string, got, want []waxlabel.Chapter) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: chapters = %+v, want %d: %+v", step, got, len(want), want)
	}
	for i, w := range want {
		g := got[i]
		if (g.Start - w.Start).Abs() < time.Millisecond {
			g.Start = w.Start
		}
		if g != w {
			t.Errorf("%s: chapter %d = %+v, want %+v", step, i, got[i], w)
		}
	}
}
