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

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// A 6 s rip divided into three 2 s tracks, the shape every case below varies.
const splitSheet = `PERFORMER "Test Performer"
TITLE "Test Album"
CATALOG 0000000000000
REM DATE 2026
REM GENRE Test
FILE "rip.wav" WAVE
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Two"
    PERFORMER "Guest"
    ISRC AAAAA0000001
    INDEX 00 00:01:70
    INDEX 01 00:02:00
  TRACK 03 AUDIO
    TITLE "Three"
    INDEX 01 00:04:00
`

// splitRip writes a 6 s stereo rip at name in dir.
func splitRip(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, mediatest.SineWAV(6, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanSplit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rip := splitRip(t, dir, "rip.wav")

	plan, err := newOfflineClient(t).PlanSplit(ctx, rip, []byte(splitSheet))
	if err != nil {
		t.Fatalf("PlanSplit: %v", err)
	}
	if plan.Rate != 44100 || plan.SheetFile != "rip.wav" {
		t.Errorf("Rate = %d, SheetFile = %q", plan.Rate, plan.SheetFile)
	}
	if len(plan.Pieces) != 3 {
		t.Fatalf("%d pieces, want 3", len(plan.Pieces))
	}
	wantStart := []int64{0, 88200, 176400}
	wantEnd := []int64{88200, 176400, media.ToEnd}
	wantPerf := []string{"Test Performer", "Guest", "Test Performer"}
	for i, p := range plan.Pieces {
		if p.StartSample != wantStart[i] || p.EndSample != wantEnd[i] {
			t.Errorf("piece %d = [%d, %d), want [%d, %d)", i, p.StartSample, p.EndSample, wantStart[i], wantEnd[i])
		}
		if p.Track != i+1 || p.Performer != wantPerf[i] {
			t.Errorf("piece %d = track %d by %q, want track %d by %q", i, p.Track, p.Performer, i+1, wantPerf[i])
		}
		if want := media.SampleTime(wantStart[i], 44100); p.Start != want {
			t.Errorf("piece %d Start = %v, want %v", i, p.Start, want)
		}
	}
	if plan.Album.Title != "Test Album" || plan.Album.Date != "2026" || plan.Album.Genre != "Test" || plan.Album.Catalog != "0000000000000" {
		t.Errorf("album = %+v", plan.Album)
	}
	if plan.Pieces[1].ISRC != "AAAAA0000001" {
		t.Errorf("piece 2 ISRC = %q", plan.Pieces[1].ISRC)
	}
}

func TestSplitWritesPiecesAndTags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")

	plan, err := c.PlanSplit(ctx, rip, []byte(splitSheet))
	if err != nil {
		t.Fatal(err)
	}
	outs := []string{filepath.Join(dir, "1.flac"), filepath.Join(dir, "2.flac"), filepath.Join(dir, "3.flac")}
	res, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(res.Outputs) != 3 {
		t.Fatalf("outputs = %v", res.Outputs)
	}

	r := media.NewRunner(media.RunnerConfig{})
	var total int64
	for i, out := range res.Outputs {
		pr, perr := r.Probe(ctx, out)
		if perr != nil {
			t.Fatal(perr)
		}
		a, ok := pr.AudioStream()
		if !ok {
			t.Fatalf("piece %d holds no audio", i+1)
		}
		if a.Samples != 88200 {
			t.Errorf("piece %d = %d samples, want 88200", i+1, a.Samples)
		}
		total += a.Samples

		doc, derr := waxlabel.ParseFile(ctx, out)
		if derr != nil {
			t.Fatal(derr)
		}
		get := func(k tag.Key) string {
			v, ok := doc.Get(k)
			if !ok || len(v) == 0 {
				return ""
			}
			return v[0]
		}
		wantTitle := []string{"One", "Two", "Three"}[i]
		wantArtist := []string{"Test Performer", "Guest", "Test Performer"}[i]
		if get(tag.Title) != wantTitle || get(tag.Artist) != wantArtist {
			t.Errorf("piece %d = %q by %q, want %q by %q", i+1, get(tag.Title), get(tag.Artist), wantTitle, wantArtist)
		}
		if get(tag.Album) != "Test Album" || get(tag.AlbumArtist) != "Test Performer" {
			t.Errorf("piece %d album = %q by %q", i+1, get(tag.Album), get(tag.AlbumArtist))
		}
		if got, want := get(tag.TrackNumber), []string{"1", "2", "3"}[i]; got != want {
			t.Errorf("piece %d track number = %q, want %q", i+1, got, want)
		}
		if get(tag.TrackTotal) != "3" {
			t.Errorf("piece %d track total = %q, want 3", i+1, get(tag.TrackTotal))
		}
		if get(tag.Genre) != "Test" || get(tag.CatalogNumber) != "0000000000000" {
			t.Errorf("piece %d genre = %q, catalog = %q", i+1, get(tag.Genre), get(tag.CatalogNumber))
		}
		if !strings.HasPrefix(get(tag.RecordingDate), "2026") {
			t.Errorf("piece %d date = %q, want 2026", i+1, get(tag.RecordingDate))
		}
		if isrc, want := get(tag.ISRC), ""; i != 1 && isrc != want {
			t.Errorf("piece %d ISRC = %q, want none", i+1, isrc)
		}
		if i == 1 && get(tag.ISRC) != "AAAAA0000001" {
			t.Errorf("piece 2 ISRC = %q", get(tag.ISRC))
		}
	}
	if want := int64(6 * 44100); total != want {
		t.Errorf("the pieces hold %d samples, the rip %d", total, want)
	}
}

// Audio before track 1's INDEX 01 is a piece of its own: it is a pregap, or on
// some discs an entire hidden song, and folding it into track 1 or dropping it
// would both be silent.
func TestSplitKeepsTheLeadIn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	if err := mediatest.TagFile(ctx, rip, "TITLE", "Whole Disc"); err != nil {
		t.Fatal(err)
	}
	sheet := strings.Replace(splitSheet, "    INDEX 01 00:00:00", "    INDEX 01 00:02:00", 1)
	sheet = strings.Replace(sheet, "    INDEX 00 00:01:70\n    INDEX 01 00:02:00", "    INDEX 01 00:04:00", 1)
	sheet = strings.Replace(sheet, `    TITLE "Three"
    INDEX 01 00:04:00`, `    TITLE "Three"
    INDEX 01 00:05:00`, 1)

	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatalf("PlanSplit: %v", err)
	}
	if len(plan.Pieces) != 4 {
		t.Fatalf("%d pieces, want 4 (a lead-in and three tracks)", len(plan.Pieces))
	}
	if p := plan.Pieces[0]; p.Track != 0 || p.Title != "" || p.StartSample != 0 {
		t.Errorf("lead-in = %+v, want track 0 with no title at sample 0", p)
	}

	outs := make([]string, 4)
	for i := range outs {
		outs[i] = filepath.Join(dir, "out", string(rune('a'+i))+".flac")
	}
	if _, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC}); err != nil {
		t.Fatalf("Split: %v", err)
	}
	doc, err := waxlabel.ParseFile(ctx, outs[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []tag.Key{tag.Title, tag.TrackNumber, tag.TrackTotal, tag.ISRC} {
		if v, ok := doc.Get(k); ok {
			t.Errorf("lead-in carries %s = %q; the rip's own values describe the disc, not this piece", k, v)
		}
	}
	if v, ok := doc.Get(tag.Album); !ok || len(v) == 0 || v[0] != "Test Album" {
		t.Errorf("lead-in album = %v, want the sheet's", v)
	}
}

// Every refusal is about the pair of sheet and rip, and all of them land before
// a piece is written.
func TestPlanSplitRefusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")

	for _, tc := range []struct {
		name  string
		sheet string
		want  error
		says  string
	}{
		{
			name:  "two files",
			sheet: splitSheet + "FILE \"other.wav\" WAVE\n  TRACK 04 AUDIO\n    TITLE \"Four\"\n    INDEX 01 00:00:00\n",
			want:  ErrIncompatibleSpec,
			says:  "already separate",
		},
		{
			name:  "one track",
			sheet: "FILE \"rip.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"Only\"\n    INDEX 01 00:00:00\n",
			want:  ErrIncompatibleSpec,
			says:  "nothing to cut",
		},
		{
			name:  "data track",
			sheet: strings.Replace(splitSheet, "TRACK 02 AUDIO", "TRACK 02 MODE1/2352", 1),
			want:  ErrIncompatibleSpec,
			says:  "data track",
		},
		{
			name:  "start past the end",
			sheet: strings.Replace(splitSheet, "INDEX 01 00:04:00", "INDEX 01 00:30:00", 1),
			want:  ErrIncompatibleSpec,
			says:  "does not describe this rip",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.PlanSplit(ctx, rip, []byte(tc.sheet))
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want %v saying %q", err, tc.want, tc.says)
			}
		})
	}

	// A rate no CD uses cannot place the sheet's frames on a sample.
	odd := filepath.Join(dir, "odd.wav")
	if err := os.WriteFile(odd, mediatest.ToneWAV(440, 6, 2, 32000), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PlanSplit(ctx, odd, []byte(splitSheet)); !errors.Is(err, ErrUnsupportedInput) || !strings.Contains(err.Error(), "CD-family") {
		t.Errorf("a 32 kHz rip = %v, want ErrUnsupportedInput naming the rate", err)
	}
}

// A truncated rip whose header still covers the sheet is refused up front,
// rather than on a later piece with the earlier ones already written.
func TestPlanSplitRefusesATruncatedRip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := splitRip(t, dir, "src.wav")
	mp3 := filepath.Join(dir, "rip.mp3")
	if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(mp3), Transcode: &TranscodeSpec{Format: FormatMP3},
	}}); err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(mp3)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.mp3")
	if err := os.WriteFile(cut, whole[:len(whole)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = c.PlanSplit(ctx, cut, []byte(splitSheet))
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not describe this rip") {
		t.Errorf("err = %v, want the up-front refusal", err)
	}
}

func TestSplitOutputRefusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	plan, err := c.PlanSplit(ctx, rip, []byte(splitSheet))
	if err != nil {
		t.Fatal(err)
	}
	ok := []string{filepath.Join(dir, "1.flac"), filepath.Join(dir, "2.flac"), filepath.Join(dir, "3.flac")}

	for _, tc := range []struct {
		name string
		outs []string
		spec TranscodeSpec
	}{
		{"copy", ok, TranscodeSpec{Format: FormatCopy}},
		{"output is the rip", []string{ok[0], rip, ok[2]}, TranscodeSpec{Format: FormatFLAC}},
		{"two pieces one path", []string{ok[0], ok[1], ok[1]}, TranscodeSpec{Format: FormatFLAC}},
		{"wrong count", ok[:2], TranscodeSpec{Format: FormatFLAC}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Split(ctx, plan, tc.outs, tc.spec); !errors.Is(err, ErrIncompatibleSpec) {
				t.Errorf("err = %v, want ErrIncompatibleSpec", err)
			}
		})
	}
	if _, err := os.Stat(ok[0]); err == nil {
		t.Error("a refused split wrote a piece")
	}
}

// The rip's own metadata rides underneath the sheet's: its cover art and the
// fields the sheet has nothing to say about are kept.
func TestSplitCarriesTheRipsOwnMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := splitRip(t, dir, "src.wav")
	rip := filepath.Join(dir, "rip.flac")
	if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(rip), Transcode: &TranscodeSpec{Format: FormatFLAC},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := mediatest.TagFile(ctx, rip, "ALBUM", "Rip's Own Album", "COMPOSER", "A Composer"); err != nil {
		t.Fatal(err)
	}
	sheet := strings.Replace(splitSheet, "TITLE \"Test Album\"\n", "", 1)

	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatal(err)
	}
	outs := []string{filepath.Join(dir, "1.flac"), filepath.Join(dir, "2.flac"), filepath.Join(dir, "3.flac")}
	if _, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC}); err != nil {
		t.Fatalf("Split: %v", err)
	}
	doc, err := waxlabel.ParseFile(ctx, outs[0])
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := doc.Get(tag.Album); !ok || len(v) == 0 || v[0] != "Rip's Own Album" {
		t.Errorf("album = %v, want the rip's, which the sheet did not name", v)
	}
	if v, ok := doc.Get(tag.Composer); !ok || len(v) == 0 || v[0] != "A Composer" {
		t.Errorf("composer = %v, want it carried from the rip", v)
	}
	if v, ok := doc.Get(tag.Title); !ok || len(v) == 0 || v[0] != "One" {
		t.Errorf("title = %v, want the sheet's track title over the rip's", v)
	}
}

// A piece is one span of the rip, so the rip's chapters land on the piece's
// own timeline: shifted to it, and dropped where the piece does not hold them.
// Carried as they stand they would point at disc positions.
func TestSplitRemapsTheRipsChapters(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := splitRip(t, dir, "src.wav")
	rip := filepath.Join(dir, "rip.flac")
	if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(rip), Transcode: &TranscodeSpec{Format: FormatFLAC},
	}}); err != nil {
		t.Fatal(err)
	}
	doc, err := waxlabel.ParseFile(ctx, rip)
	if err != nil {
		t.Fatal(err)
	}
	// One chapter per track, at the sheet's own boundaries.
	plan, err := doc.Edit().SetChapters(
		waxlabel.Chapter{Start: 0, Title: "Disc One"},
		waxlabel.Chapter{Start: 2 * time.Second, Title: "Disc Two"},
		waxlabel.Chapter{Start: 4 * time.Second, Title: "Disc Three"},
	).Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}

	sp, err := c.PlanSplit(ctx, rip, []byte(splitSheet))
	if err != nil {
		t.Fatal(err)
	}
	outs := []string{filepath.Join(dir, "1.flac"), filepath.Join(dir, "2.flac"), filepath.Join(dir, "3.flac")}
	if _, err := c.Split(ctx, sp, outs, TranscodeSpec{Format: FormatFLAC}); err != nil {
		t.Fatalf("Split: %v", err)
	}
	for i, out := range outs {
		pd, perr := waxlabel.ParseFile(ctx, out)
		if perr != nil {
			t.Fatal(perr)
		}
		chs := pd.Chapters()
		if len(chs) != 1 {
			t.Errorf("piece %d has %d chapters, want the one that falls inside it: %+v", i+1, len(chs), chs)
			continue
		}
		if chs[0].Start != 0 {
			t.Errorf("piece %d chapter starts at %v, want it shifted to the piece's own start", i+1, chs[0].Start)
		}
		if want := []string{"Disc One", "Disc Two", "Disc Three"}[i]; chs[0].Title != want {
			t.Errorf("piece %d chapter = %q, want %q", i+1, chs[0].Title, want)
		}
	}
}

// A sheet that names no title for a track does not leave the rip's disc-wide
// one on the piece: per-track fields are the sheet's whether or not it filled
// them in.
func TestSplitDropsTheRipsTitleWhereTheSheetHasNone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := splitRip(t, dir, "src.wav")
	rip := filepath.Join(dir, "rip.flac")
	if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(rip), Transcode: &TranscodeSpec{Format: FormatFLAC},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := mediatest.TagFile(ctx, rip, "TITLE", "Whole Disc", "ISRC", "ZZZZZ0000000"); err != nil {
		t.Fatal(err)
	}
	sheet := strings.Replace(splitSheet, "    TITLE \"Two\"\n", "", 1)

	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatal(err)
	}
	outs := []string{filepath.Join(dir, "1.flac"), filepath.Join(dir, "2.flac"), filepath.Join(dir, "3.flac")}
	if _, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC}); err != nil {
		t.Fatalf("Split: %v", err)
	}
	doc, err := waxlabel.ParseFile(ctx, outs[1])
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := doc.Get(tag.Title); ok {
		t.Errorf("piece 2 title = %v, want none: the sheet named none and the rip's describes the disc", v)
	}
	// Piece 2 keeps its own ISRC from the sheet; the others drop the rip's.
	if v, ok := doc.Get(tag.ISRC); !ok || len(v) == 0 || v[0] != "AAAAA0000001" {
		t.Errorf("piece 2 ISRC = %v, want the sheet's", v)
	}
	third, err := waxlabel.ParseFile(ctx, outs[2])
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := third.Get(tag.ISRC); ok {
		t.Errorf("piece 3 ISRC = %v, want none: the rip's is the disc's", v)
	}
}

// A lossy rip's decode can overshoot full scale, which is why the clipping
// policy suppresses the warning for such a source. The split has to name the
// source codec for that to hold.
func TestSplitDoesNotWarnClippingOnALossyRip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := filepath.Join(dir, "src.wav")
	// Samples past full scale, so the rip's decode overshoots and the pieces'
	// integer encode clamps: the shape the clipping warning is about.
	if err := os.WriteFile(wav, mediatest.HotFloatWAV(6, 2, 13), 0o644); err != nil {
		t.Fatal(err)
	}
	rip := filepath.Join(dir, "rip.mp3")
	if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(rip), Transcode: &TranscodeSpec{Format: FormatMP3},
	}}); err != nil {
		t.Fatal(err)
	}
	plan, err := c.PlanSplit(ctx, rip, []byte(strings.Replace(splitSheet, `FILE "rip.wav" WAVE`, `FILE "rip.mp3" MP3`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	outs := []string{filepath.Join(dir, "1.wav"), filepath.Join(dir, "2.wav"), filepath.Join(dir, "3.wav")}
	// 16-bit pieces, so the overs the rip's decode carries are clamped and the
	// level measurement has something to report; a float output would clamp
	// nothing and the suppression would go untested.
	res, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatWAV, BitDepth: 16})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, w := range res.Warnings {
		if w.Code == WarnOutputClipping {
			t.Errorf("a lossy rip raised output-clipping: %q", w.Detail)
		}
	}
}
