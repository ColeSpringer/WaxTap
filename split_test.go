package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
			name: "no audio track",
			sheet: strings.NewReplacer("TRACK 01 AUDIO", "TRACK 01 MODE1/2352", "TRACK 02 AUDIO", "TRACK 02 MODE1/2352",
				"TRACK 03 AUDIO", "TRACK 03 MODE1/2352").Replace(splitSheet),
			want: ErrIncompatibleSpec,
			says: "no audio track",
		},
		{
			// A cooked sector is 512 samples where a raw one is a frame's 588,
			// so every boundary past it would land early.
			name:  "cooked data track ahead of the audio",
			sheet: strings.Replace(splitSheet, "TRACK 01 AUDIO", "TRACK 01 MODE1/2048", 1),
			want:  ErrIncompatibleSpec,
			says:  "2352-byte sectors",
		},
		{
			name:  "start past the end",
			sheet: strings.Replace(splitSheet, "INDEX 01 00:04:00", "INDEX 01 00:30:00", 1),
			want:  ErrIncompatibleSpec,
			says:  "does not describe this",
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
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not describe this") {
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
		// One file on Windows and macOS, where the later piece would replace
		// the earlier; refused everywhere so a set is the same set everywhere.
		{"two pieces apart in case", []string{ok[0], filepath.Join(dir, "1.FLAC"), ok[2]}, TranscodeSpec{Format: FormatFLAC}},
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

// pieceTag reads one tag off a written piece, "" when it carries none.
func pieceTag(t *testing.T, ctx context.Context, path string, k tag.Key) string {
	t.Helper()
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := doc.Get(k)
	if !ok || len(v) == 0 {
		return ""
	}
	return v[0]
}

// A mixed-mode disc's data track is a piece nothing writes: the split skips it
// and the audio keeps the disc's own numbering, 2 and up of a total that counts
// the data track. Written, it would be noise named after a song.
func TestPlanSplitSkipsADataTrack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	sheet := `PERFORMER "Test Performer"
TITLE "Test Album"
FILE "rip.wav" WAVE
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 00 00:01:70
    INDEX 01 00:02:00
  TRACK 03 AUDIO
    TITLE "Three"
    INDEX 01 00:04:00
`
	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatalf("PlanSplit: %v", err)
	}
	if len(plan.Pieces) != 2 || plan.Pieces[0].Track != 2 || plan.Pieces[1].Track != 3 {
		t.Fatalf("pieces = %+v, want tracks 2 and 3", plan.Pieces)
	}
	// The audio begins at its own INDEX 01: the mode change puts data-mode
	// sectors inside the pregap after the data track.
	if p := plan.Pieces[0]; p.StartSample != 88200 || p.EndSample != 176400 {
		t.Errorf("track 2 = [%d, %d), want [88200, 176400)", p.StartSample, p.EndSample)
	}
	want := []SplitSkip{{Track: 1, Type: "MODE1/2352", StartSample: 0, EndSample: 88200, End: 2 * time.Second}}
	if !slices.Equal(plan.Skipped, want) {
		t.Errorf("skipped = %+v, want %+v", plan.Skipped, want)
	}
	if plan.TrackTotal != 3 || plan.TrackCount() != 2 {
		t.Errorf("TrackTotal = %d, TrackCount = %d; want 3 on the disc, 2 written", plan.TrackTotal, plan.TrackCount())
	}

	outs := []string{filepath.Join(dir, "2.flac"), filepath.Join(dir, "3.flac")}
	res, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(res.Outputs) != 2 {
		t.Fatalf("outputs = %v, want the two audio pieces", res.Outputs)
	}
	r := media.NewRunner(media.RunnerConfig{})
	for i, out := range res.Outputs {
		pr, perr := r.Probe(ctx, out)
		if perr != nil {
			t.Fatal(perr)
		}
		if a, ok := pr.AudioStream(); !ok || a.Samples != 88200 {
			t.Errorf("piece %d holds %d samples, want 88200", i+1, a.Samples)
		}
		number := []string{"2", "3"}[i]
		if got := pieceTag(t, ctx, out, tag.TrackNumber); got != number {
			t.Errorf("piece %d track number = %q, want %q: the disc's own", i+1, got, number)
		}
		if got := pieceTag(t, ctx, out, tag.TrackTotal); got != "3" {
			t.Errorf("piece %d track total = %q, want 3", i+1, got)
		}
	}
}

// A data track the rip holds none of is still the disc's: it is listed as
// skipped, so a caller can say why the numbering starts at 2, and counted in
// TrackTotal when it precedes the audio. EAC lists a mixed-mode disc's data
// track at frame 0 beside the audio session it ripped, XLD gives it a FILE of
// its own, and an Enhanced CD's sits in a second session past the rip, which no
// player counts.
func TestPlanSplitListsTheDataTracksTheRipHoldsNoneOf(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	const audio = `  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 01 00:00:00
  TRACK 03 AUDIO
    TITLE "Three"
    INDEX 01 00:03:00
`
	const dataFile = "FILE \"rip.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"
	for _, tc := range []struct {
		name    string
		sheet   string
		tracks  []int // the pieces', in order
		skipped []SplitSkip
		total   int
	}{
		{
			name:    "beside the audio at frame 0",
			sheet:   "FILE \"rip.wav\" WAVE\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n" + audio,
			tracks:  []int{2, 3},
			skipped: []SplitSkip{{Track: 1, Type: "MODE1/2352"}},
			total:   3,
		},
		{
			name:    "in a FILE of its own",
			sheet:   dataFile + "FILE \"rip.wav\" WAVE\n" + audio,
			tracks:  []int{2, 3},
			skipped: []SplitSkip{{Track: 1, Type: "MODE1/2352"}},
			total:   3,
		},
		{
			// The audio ahead of track 2's INDEX 01 is the data track's pregap,
			// in the data track's mode, and is skipped as the data track is.
			name: "in a FILE of its own, with its pregap in the rip",
			sheet: dataFile + "FILE \"rip.wav\" WAVE\n" +
				strings.Replace(audio, "    INDEX 01 00:00:00", "    INDEX 00 00:00:00\n    INDEX 01 00:02:00", 1),
			tracks:  []int{2, 3},
			skipped: []SplitSkip{{Track: 1, Type: "MODE1/2352"}, {StartSample: 0, EndSample: 88200, End: 2 * time.Second}},
			total:   3,
		},
		{
			name: "after the audio",
			sheet: "FILE \"rip.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n" +
				"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:02:00\n" +
				"  TRACK 03 MODE1/2352\n    INDEX 00 00:08:00\n    INDEX 01 00:10:00\n",
			tracks:  []int{1, 2},
			skipped: []SplitSkip{{Track: 3, Type: "MODE1/2352"}},
			total:   2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := c.PlanSplit(ctx, rip, []byte(tc.sheet))
			if err != nil {
				t.Fatalf("PlanSplit: %v", err)
			}
			var tracks []int
			for _, p := range plan.Pieces {
				tracks = append(tracks, p.Track)
			}
			if !slices.Equal(tracks, tc.tracks) {
				t.Errorf("the pieces are tracks %v, want %v", tracks, tc.tracks)
			}
			if !slices.Equal(plan.Skipped, tc.skipped) {
				t.Errorf("skipped = %+v, want %+v", plan.Skipped, tc.skipped)
			}
			if plan.TrackTotal != tc.total {
				t.Errorf("TrackTotal = %d, want %d", plan.TrackTotal, tc.total)
			}
			if last := plan.Pieces[len(plan.Pieces)-1]; last.EndSample != media.ToEnd {
				t.Errorf("the last audio piece ends at %d, want the rip's end", last.EndSample)
			}
		})
	}
}

// A sheet that numbers the hidden track TRACK 00 gets the piece the lead-in
// does, track 0 with no number of its own, but keeps the title the sheet gave
// it; the disc's tracks are numbered of a total that leaves it out.
func TestSplitKeepsATrackZerosTitle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	sheet := `PERFORMER "Test Performer"
TITLE "Test Album"
FILE "rip.wav" WAVE
  TRACK 00 AUDIO
    TITLE "Secret"
    INDEX 01 00:00:00
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:02:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 01 00:04:00
`
	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatalf("PlanSplit: %v", err)
	}
	if len(plan.Pieces) != 3 || plan.Pieces[0].Track != 0 || plan.Pieces[0].Title != "Secret" {
		t.Fatalf("pieces = %+v, want track 0 \"Secret\" first", plan.Pieces)
	}
	if plan.TrackTotal != 2 {
		t.Errorf("TrackTotal = %d, want 2: track 0 is not a track of the disc's", plan.TrackTotal)
	}
	outs := []string{filepath.Join(dir, "0.flac"), filepath.Join(dir, "1.flac"), filepath.Join(dir, "2.flac")}
	if _, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC}); err != nil {
		t.Fatalf("Split: %v", err)
	}
	if got := pieceTag(t, ctx, outs[0], tag.Title); got != "Secret" {
		t.Errorf("track 0 title = %q, want the sheet's", got)
	}
	for _, k := range []tag.Key{tag.TrackNumber, tag.TrackTotal} {
		if got := pieceTag(t, ctx, outs[0], k); got != "" {
			t.Errorf("track 0 carries %s = %q; it has no number", k, got)
		}
	}
	if n, total := pieceTag(t, ctx, outs[1], tag.TrackNumber), pieceTag(t, ctx, outs[1], tag.TrackTotal); n != "1" || total != "2" {
		t.Errorf("track 1 is %s of %s, want 1 of 2", n, total)
	}
}

// A data track between two audio tracks ends the audio before it at its INDEX
// 00, the frame CUETools ends it at too, and the audio after it begins at its
// own INDEX 01.
func TestPlanSplitSkipsADataTrackBetweenAudioTracks(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	sheet := strings.Replace(splitSheet, "TRACK 02 AUDIO", "TRACK 02 MODE1/2352", 1)

	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatalf("PlanSplit: %v", err)
	}
	if len(plan.Pieces) != 2 || plan.Pieces[0].Track != 1 || plan.Pieces[1].Track != 3 {
		t.Fatalf("pieces = %+v, want tracks 1 and 3", plan.Pieces)
	}
	const index00 = 145 * 588 // 00:01:70 at 44.1 kHz
	if p := plan.Pieces[0]; p.EndSample != index00 {
		t.Errorf("track 1 ends at %d, want %d, the data track's INDEX 00", p.EndSample, index00)
	}
	want := []SplitSkip{{Track: 2, Type: "MODE1/2352", Start: media.SampleTime(index00, 44100), StartSample: index00, EndSample: 176400, End: 4 * time.Second}}
	if !slices.Equal(plan.Skipped, want) {
		t.Errorf("skipped = %+v, want %+v", plan.Skipped, want)
	}
	if plan.TrackTotal != 3 {
		t.Errorf("TrackTotal = %d, want 3", plan.TrackTotal)
	}
}

// A trailing data track the rip does hold (a whole-disc image of an Enhanced
// CD) ends the last audio piece at its INDEX 00 and is skipped to the end; the
// disc's count stops at the audio.
func TestPlanSplitHoldsATrailingDataTrack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	sheet := `FILE "rip.wav" WAVE
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 01 00:02:00
  TRACK 03 MODE1/2352
    INDEX 00 00:05:00
    INDEX 01 00:05:50
`
	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatalf("PlanSplit: %v", err)
	}
	if len(plan.Pieces) != 2 || plan.Pieces[1].EndSample != 5*44100 {
		t.Fatalf("pieces = %+v, want two, the last ending at the data track's INDEX 00", plan.Pieces)
	}
	want := []SplitSkip{{Track: 3, Type: "MODE1/2352", Start: 5 * time.Second, StartSample: 5 * 44100, EndSample: ToEnd, End: plan.Duration}}
	if !slices.Equal(plan.Skipped, want) {
		t.Errorf("skipped = %+v, want %+v", plan.Skipped, want)
	}
	if plan.TrackTotal != 2 {
		t.Errorf("TrackTotal = %d, want 2: a data track after the audio is not counted", plan.TrackTotal)
	}
}

// TRACKTOTAL is the disc's highest track number, not a count of the sheet's
// lines: a sheet that leaves the data track out and numbers its audio from 02,
// or skips a number, still says how many tracks the disc has.
func TestPlanSplitTrackTotalIsTheDiscsHighestNumber(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	for _, tc := range []struct {
		name    string
		numbers []string
		total   int
	}{
		{"data track left out", []string{"02", "03", "04"}, 4},
		{"a number skipped", []string{"01", "02", "04"}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sheet := strings.NewReplacer("TRACK 01", "TRACK "+tc.numbers[0], "TRACK 02", "TRACK "+tc.numbers[1], "TRACK 03", "TRACK "+tc.numbers[2]).Replace(splitSheet)
			plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
			if err != nil {
				t.Fatalf("PlanSplit: %v", err)
			}
			if plan.TrackTotal != tc.total {
				t.Errorf("TrackTotal = %d, want %d", plan.TrackTotal, tc.total)
			}
			out := filepath.Join(dir, tc.name+".flac")
			outs := []string{out, filepath.Join(dir, tc.name+"-b.flac"), filepath.Join(dir, tc.name+"-c.flac")}
			if _, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC}); err != nil {
				t.Fatalf("Split: %v", err)
			}
			if got := pieceTag(t, ctx, out, tag.TrackTotal); got != strconv.Itoa(tc.total) {
				t.Errorf("track total = %q, want %d", got, tc.total)
			}
		})
	}
}

// A sheet that numbers the hidden track TRACK 00 and gives it a pregap of its
// own has no track before it for that audio to belong to: it is folded into
// track 0 rather than kept as a second numberless piece with the same name.
func TestPlanSplitFoldsTheLeadInIntoATrackZero(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	sheet := `FILE "rip.wav" WAVE
  TRACK 00 AUDIO
    TITLE "Secret"
    INDEX 00 00:00:00
    INDEX 01 00:01:00
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:02:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 01 00:04:00
`
	plan, err := c.PlanSplit(ctx, rip, []byte(sheet))
	if err != nil {
		t.Fatalf("PlanSplit: %v", err)
	}
	if len(plan.Pieces) != 3 {
		t.Fatalf("%d pieces, want 3: track 0 whole, then tracks 1 and 2", len(plan.Pieces))
	}
	if p := plan.Pieces[0]; p.Track != 0 || p.Title != "Secret" || p.StartSample != 0 || p.EndSample != 88200 || p.Start != 0 {
		t.Errorf("track 0 = %+v, want \"Secret\" over [0, 88200)", p)
	}
}

// A rip that holds no audio is refused before a piece is attempted, rather
// than failing on the first one with the output directory already made.
func TestPlanSplitRefusesAnEmptyRip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.wav")
	if err := os.WriteFile(empty, mediatest.SineWAV(0, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := newOfflineClient(t).PlanSplit(ctx, empty, []byte(splitSheet))
	if !errors.Is(err, ErrUnsupportedInput) || !strings.Contains(err.Error(), "no audio") {
		t.Errorf("err = %v, want ErrUnsupportedInput saying the rip holds no audio", err)
	}
}

// A plan that states no total clears the rip's own TRACKTOTAL rather than
// leaving it under the sheet's numbers: a single-file rip tagged 1/1 would
// otherwise hand every piece "of 1".
func TestSplitClearsATrackTotalThePlanDoesNotState(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	rip := splitRip(t, dir, "rip.wav")
	if err := mediatest.TagFile(ctx, rip, "TRACKNUMBER", "1", "TRACKTOTAL", "1"); err != nil {
		t.Fatal(err)
	}
	plan, err := c.PlanSplit(ctx, rip, []byte(splitSheet))
	if err != nil {
		t.Fatal(err)
	}
	plan.TrackTotal = 0
	outs := []string{filepath.Join(dir, "1.flac"), filepath.Join(dir, "2.flac"), filepath.Join(dir, "3.flac")}
	if _, err := c.Split(ctx, plan, outs, TranscodeSpec{Format: FormatFLAC}); err != nil {
		t.Fatalf("Split: %v", err)
	}
	if n, total := pieceTag(t, ctx, outs[1], tag.TrackNumber), pieceTag(t, ctx, outs[1], tag.TrackTotal); n != "2" || total != "" {
		t.Errorf("piece 2 is %q of %q, want 2 of nothing", n, total)
	}
}
