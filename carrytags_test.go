package waxtap

import (
	"context"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// taggedFLAC builds a FLAC fixture carrying tags, a cover picture, and three
// chapters, by transcoding a synthetic WAV and tagging the result.
func taggedFLAC(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()
	wav := filepath.Join(dir, "fixture.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	flac := filepath.Join(dir, "fixture.flac")
	if _, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: wav,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(flac),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	}); err != nil {
		t.Fatalf("fixture transcode: %v", err)
	}
	doc, err := waxlabel.ParseFile(ctx, flac)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	png := mediatest.PNGBytes(mediatest.SolidCover(64, 64, color.RGBA{R: 200, A: 255}))
	plan, perr := doc.Edit().
		Set(tag.Title, "Carried Title").
		Set(tag.Artist, "Carried Artist").
		Set(tag.ReplayGainTrackGain, "-6.50 dB").
		AddPicture(waxlabel.Picture{Type: waxlabel.PicFrontCover, Data: png}).
		SetChapters(
			waxlabel.Chapter{Start: 0, Title: "One"},
			waxlabel.Chapter{Start: 1 * time.Second, Title: "Two"},
			waxlabel.Chapter{Start: 2 * time.Second, Title: "Three"},
		).
		Prepare()
	if perr != nil {
		t.Fatalf("prepare fixture tags: %v", perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatalf("write fixture tags: %v", err)
	}
	return flac
}

// TestCarryTagsAcrossTranscode pins the carry pass: a local transcode delivers
// the input's tags, picture, and chapters in the new container.
func TestCarryTagsAcrossTranscode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedFLAC(t, dir)
	out := filepath.Join(dir, "out.opus")

	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatOpus},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("full FLAC->Opus carry warned: %+v", res.Warnings)
	}

	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	for key, want := range map[tag.Key]string{tag.Title: "Carried Title", tag.Artist: "Carried Artist"} {
		if got, ok := doc.Get(key); !ok || got[0] != want {
			t.Errorf("output %s = %q (ok=%v), want %q", key, got, ok, want)
		}
	}
	if n := len(doc.Pictures()); n != 1 {
		t.Errorf("output pictures = %d, want 1", n)
	}
	if n := len(doc.Chapters()); n != 3 {
		t.Errorf("output chapters = %d, want 3", n)
	}
	// A re-encode invalidates own-audio values, so ReplayGain must not carry.
	if got, ok := doc.Get(tag.ReplayGainTrackGain); ok {
		t.Errorf("re-encoded output carries ReplayGain %q, want none", got)
	}
}

// TestCarryTagsRemuxRestoresOwnAudio pins the remux exception: a whole-file
// packet copy leaves the audio untouched, so the own-audio tags the transfer
// excludes (ReplayGain here) still hold and are restored.
func TestCarryTagsRemuxRestoresOwnAudio(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedFLAC(t, dir)
	out := filepath.Join(dir, "remux.flac")

	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatCopy},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("remux carry warned: %+v", res.Warnings)
	}
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if got, ok := doc.Get(tag.ReplayGainTrackGain); !ok || got[0] != "-6.50 dB" {
		t.Errorf("remux output ReplayGain = %q (ok=%v), want -6.50 dB restored", got, ok)
	}
	if got, ok := doc.Get(tag.Title); !ok || got[0] != "Carried Title" {
		t.Errorf("remux output TITLE = %q (ok=%v), want Carried Title", got, ok)
	}
}

// TestCarryTagsAlbum pins the album path: ProcessAlbum re-encodes every track
// through its own loop, and each output still carries the input's metadata.
func TestCarryTagsAlbum(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedFLAC(t, dir)
	out := filepath.Join(dir, "album.flac")

	res, err := newOfflineClient(t).ProcessAlbum(ctx,
		[]AlbumTrack{{Input: in, Output: out}}, -18, TranscodeSpec{Format: FormatFLAC})
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("album carry warned: %+v", res.Warnings)
	}
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if got, ok := doc.Get(tag.Title); !ok || got[0] != "Carried Title" {
		t.Errorf("album output TITLE = %q (ok=%v), want Carried Title", got, ok)
	}
	if n := len(doc.Chapters()); n != 3 {
		t.Errorf("album output chapters = %d, want 3", n)
	}
	// The applied gain changed the loudness ReplayGain describes.
	if got, ok := doc.Get(tag.ReplayGainTrackGain); ok {
		t.Errorf("album output carries ReplayGain %q, want none", got)
	}
}

// TestRemapChaptersUnsortedInput pins the defensive sort: until-next spans
// resolve against the following chapter, so a container that stored chapters
// out of timeline order must not resolve inverted spans and drop live ones.
func TestRemapChaptersUnsortedInput(t *testing.T) {
	cut := &appliedCut{
		keeps: []cutrange.Range{{Start: 0, End: 1 * time.Second}, {Start: 2 * time.Second, End: 3 * time.Second}},
		total: 3 * time.Second,
	}
	chs := []waxlabel.Chapter{
		{Start: 2 * time.Second, Title: "Three"},
		{Start: 0, Title: "One"},
		{Start: 1 * time.Second, Title: "Two"},
	}
	got := remapChapters(chs, cut)
	if len(got) != 2 || got[0].Title != "One" || got[0].Start != 0 ||
		got[1].Title != "Three" || got[1].Start != 1*time.Second {
		t.Errorf("remapChapters(unsorted) = %+v, want One@0 and Three@1s", got)
	}
}

// TestCarryTagsCutRemapsChapters pins the cut interaction: tags still carry,
// and chapters follow the cut timeline. Removing [1s,2s) from the 3s fixture
// erases chapter Two's content entirely and shifts Three from 2s to 1s; a
// correct remap is the expected outcome, so nothing warns.
func TestCarryTagsCutRemapsChapters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedFLAC(t, dir)
	out := filepath.Join(dir, "cut.flac")

	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 1 * time.Second, End: 2 * time.Second}}},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("cut carry warned: %+v", res.Warnings)
	}

	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if got, ok := doc.Get(tag.Title); !ok || got[0] != "Carried Title" {
		t.Errorf("output TITLE = %q (ok=%v), want Carried Title", got, ok)
	}
	chs := doc.Chapters()
	if len(chs) != 2 {
		t.Fatalf("output chapters = %+v, want One and Three", chs)
	}
	if chs[0].Title != "One" || chs[0].Start != 0 {
		t.Errorf("chapter 0 = %q@%v, want One@0", chs[0].Title, chs[0].Start)
	}
	if chs[1].Title != "Three" || chs[1].Start != 1*time.Second {
		t.Errorf("chapter 1 = %q@%v, want Three@1s", chs[1].Title, chs[1].Start)
	}
}

// TestCarryTagsUntaggedSourceSilent pins the no-op: an input with no metadata
// carries nothing and warns nothing.
func TestCarryTagsUntaggedSourceSilent(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "plain.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(2, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: wav,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "plain.flac")),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("untagged carry warned: %+v", res.Warnings)
	}
}
