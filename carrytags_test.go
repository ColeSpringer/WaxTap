package waxtap

import (
	"context"
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// carryItem returns the TagCarry item of kind (and key, for a field). The
// report is the machine-readable twin of the output, so every carry test reads
// both.
func carryItem(t *testing.T, tc *TagCarry, kind CarryKind, key string) CarryItem {
	t.Helper()
	if tc == nil {
		t.Fatal("TagCarry is nil: the carry did not run")
	}
	for _, it := range tc.Items {
		if it.Kind == kind && it.Key == key {
			return it
		}
	}
	t.Fatalf("no %s item for %q in %+v", kind, key, tc.Items)
	return CarryItem{}
}

// TestCarryTagsAcrossTranscode pins the carry pass: a local transcode delivers
// the input's tags, picture, and chapters in the new container, and the
// itemized report says the same: every field carried but the own-audio one,
// the picture and chapter sets whole.
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
	for _, key := range []tag.Key{tag.Title, tag.Artist} {
		if it := carryItem(t, res.TagCarry, CarryField, string(key)); it.Disposition != DispositionCarried || it.Count != 1 {
			t.Errorf("TagCarry %s = %+v, want 1 value carried", key, it)
		}
	}
	if it := carryItem(t, res.TagCarry, CarryField, string(tag.ReplayGainTrackGain)); it.Disposition != DispositionExcluded || it.Reason == "" {
		t.Errorf("TagCarry ReplayGain = %+v, want excluded with a reason", it)
	}
	if it := carryItem(t, res.TagCarry, CarryPictures, ""); it.Disposition != DispositionCarried || it.Count != 1 {
		t.Errorf("TagCarry pictures = %+v, want 1 carried", it)
	}
	if it := carryItem(t, res.TagCarry, CarryChapters, ""); it.Disposition != DispositionCarried || it.Count != 3 || it.Removed != 0 {
		t.Errorf("TagCarry chapters = %+v, want 3 carried, none removed", it)
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
	if it := carryItem(t, res.TagCarry, CarryField, string(tag.ReplayGainTrackGain)); it.Disposition != DispositionCarried || it.Reason != "" {
		t.Errorf("TagCarry ReplayGain after the restore = %+v, want carried", it)
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
	if len(res.TagCarry) != 1 {
		t.Fatalf("album TagCarry = %+v, want one report per track", res.TagCarry)
	}
	if it := carryItem(t, res.TagCarry[0], CarryChapters, ""); it.Disposition != DispositionCarried || it.Count != 3 {
		t.Errorf("album TagCarry chapters = %+v, want 3 carried", it)
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

// TestRemapChaptersKeepsPointChapters pins the drop rule's edge: a chapter
// with content inside the source is dropped when the cut removed all of it. A
// chapter with no content of its own, sharing its start with the next or
// sitting at or past the end (forms the Musepack and ASF readers keep), is a
// point mark that follows the lyric-line rule: dropped when the cut took its
// instant (the twin of a removed chapter goes with it), kept and shifted
// otherwise, a join boundary and the end of the source included.
func TestRemapChaptersKeepsPointChapters(t *testing.T) {
	cut := &appliedCut{
		keeps: []cutrange.Range{{Start: 0, End: 1 * time.Second}, {Start: 2 * time.Second, End: 3 * time.Second}},
		total: 3 * time.Second,
	}
	chs := []waxlabel.Chapter{
		{Start: 0, Title: "One"},
		{Start: 500 * time.Millisecond, Title: "Twin A"},
		{Start: 500 * time.Millisecond, Title: "Twin B"},
		{Start: 1 * time.Second, Title: "Removed"},
		{Start: 1500 * time.Millisecond, Title: "Cut twin A"},
		{Start: 1500 * time.Millisecond, Title: "Cut twin B"},
		{Start: 2 * time.Second, Title: "Three"},
		{Start: 2 * time.Second, Title: "Three's twin"},
		{Start: 3 * time.Second, Title: "At end"},
		{Start: 4 * time.Second, Title: "Past end"},
	}
	want := []waxlabel.Chapter{
		{Start: 0, Title: "One"},
		{Start: 500 * time.Millisecond, Title: "Twin A"},
		{Start: 500 * time.Millisecond, Title: "Twin B"},
		{Start: 1 * time.Second, Title: "Three"},
		{Start: 1 * time.Second, Title: "Three's twin"},
		{Start: 2 * time.Second, Title: "At end"},
		{Start: 2 * time.Second, Title: "Past end"},
	}
	if got := remapChapters(chs, cut); !slices.Equal(got, want) {
		t.Errorf("remapChapters = %+v, want %+v", got, want)
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
	// The report counts what the output holds and what the cut took, and a
	// removal is not a loss: the set stays carried.
	if it := carryItem(t, res.TagCarry, CarryChapters, ""); it.Disposition != DispositionCarried || it.Count != 2 || it.Removed != 1 {
		t.Errorf("TagCarry chapters = %+v, want 2 carried with 1 removed by the cut", it)
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
	if res.TagCarry != nil {
		t.Errorf("untagged TagCarry = %+v, want nil: nothing to carry means no carry ran", res.TagCarry)
	}
}

// lyricedFLAC builds a 10 s FLAC carrying chapters at 0/2/5 s and one synced
// lyric set with lines at 1.5 s and 3 s. It is separate from taggedFLAC so the
// chapter tests keep their own fixture and their no-warning assertions.
func lyricedFLAC(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()
	wav := filepath.Join(dir, "lyriced.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(10, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	flac := filepath.Join(dir, "lyriced.flac")
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
	plan, perr := doc.Edit().
		Set(tag.Title, "Lyriced Title").
		SetChapters(
			waxlabel.Chapter{Start: 0, Title: "One"},
			waxlabel.Chapter{Start: 2 * time.Second, Title: "Two"},
			waxlabel.Chapter{Start: 5 * time.Second, Title: "Three"},
		).
		SetSyncedLyrics(waxlabel.SyncedLyrics{
			Language: "eng",
			Lines: []waxlabel.SyncedLine{
				{Time: 1500 * time.Millisecond, Text: "first"},
				{Time: 3 * time.Second, Text: "second"},
			},
		}).
		Prepare()
	if perr != nil {
		t.Fatalf("prepare fixture tags: %v", perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatalf("write fixture tags: %v", err)
	}
	return flac
}

// Synced lyrics follow a cut the way chapters do: survivors shift by the audio
// removed before them, and a line whose instant was removed goes with it. Each
// row re-checks the chapters, which guards the chained-document rewrite: a
// lyrics save built on the pre-chapter document would put the old marks back.
func TestCarryTagsCutRemapsSyncedLyrics(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		remove     TimeRange
		wantTimes  []time.Duration
		wantNote   string // "" means no WarnTagCarry at all
		wantChapts []time.Duration
		// wantSets and wantRemoved are the report's view: sets on the output,
		// lines the cut took.
		wantSets, wantRemoved int
	}{
		{
			name:       "head cut shifts both lines",
			remove:     TimeRange{Start: 0, End: 1 * time.Second},
			wantTimes:  []time.Duration{500 * time.Millisecond, 2 * time.Second},
			wantChapts: []time.Duration{0, 1 * time.Second, 4 * time.Second},
			wantSets:   1,
		},
		{
			name:        "a line inside the removed span is dropped",
			remove:      TimeRange{Start: 2 * time.Second, End: 5 * time.Second},
			wantTimes:   []time.Duration{1500 * time.Millisecond},
			wantNote:    "1 synced lyric line pointed at removed audio",
			wantSets:    1,
			wantRemoved: 1,
		},
		{
			name:        "removing everything they point at drops the set",
			remove:      TimeRange{Start: 0, End: 6 * time.Second},
			wantNote:    "2 synced lyric lines",
			wantRemoved: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in := lyricedFLAC(t, dir)
			out := filepath.Join(dir, "cut.flac")

			res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
				Input: in,
				ProcessSpec: ProcessSpec{
					Output:    ToFile(out),
					Transcode: &TranscodeSpec{Format: FormatFLAC},
					Cut:       &CutSpec{Ranges: []TimeRange{tc.remove}},
				},
			})
			if err != nil {
				t.Fatalf("Process: %v", err)
			}

			detail := warningDetail(res, WarnTagCarry)
			if tc.wantNote == "" {
				if detail != "" {
					t.Errorf("carry warned: %q", detail)
				}
			} else if !strings.Contains(detail, tc.wantNote) {
				t.Errorf("carry note = %q, want it to contain %q", detail, tc.wantNote)
			}
			// A set the cut emptied reports removed, not carried with nothing.
			wantDisp := DispositionCarried
			if tc.wantSets == 0 {
				wantDisp = DispositionRemoved
			}
			if it := carryItem(t, res.TagCarry, CarrySyncedLyrics, ""); it.Disposition != wantDisp || it.Count != tc.wantSets || it.Removed != tc.wantRemoved {
				t.Errorf("TagCarry synced lyrics = %+v, want %d sets %s with %d lines removed", it, tc.wantSets, wantDisp, tc.wantRemoved)
			}

			doc, err := waxlabel.ParseFile(ctx, out)
			if err != nil {
				t.Fatalf("parse output: %v", err)
			}
			var got []time.Duration
			for _, sl := range doc.SyncedLyrics() {
				for _, l := range sl.Lines {
					got = append(got, l.Time)
				}
			}
			if !equalTimes(got, tc.wantTimes) {
				t.Errorf("lyric times = %v, want %v", got, tc.wantTimes)
			}
			if tc.wantChapts != nil {
				var chs []time.Duration
				for _, c := range doc.Chapters() {
					chs = append(chs, c.Start)
				}
				if !equalTimes(chs, tc.wantChapts) {
					t.Errorf("chapter starts = %v, want %v (the lyrics rewrite must build on the remapped chapters)", chs, tc.wantChapts)
				}
			}
		})
	}
}

// A source whose only metadata is a synced-lyrics set, cut so that every line
// goes, ends with an empty destination: the warning lead must say no metadata
// carried, not "carried with losses" from the pre-rewrite transfer report.
func TestCarryTagsCutEmptyingOnlyMetadataSaysNothingCarried(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	wav := filepath.Join(dir, "plain.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(10, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	flac := filepath.Join(dir, "lyriconly.flac")
	if _, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input:       wav,
		ProcessSpec: ProcessSpec{Output: ToFile(flac), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	}); err != nil {
		t.Fatalf("fixture transcode: %v", err)
	}
	doc, err := waxlabel.ParseFile(ctx, flac)
	if err != nil {
		t.Fatal(err)
	}
	plan, perr := doc.Edit().SetSyncedLyrics(waxlabel.SyncedLyrics{
		Language: "eng",
		Lines:    []waxlabel.SyncedLine{{Time: 3 * time.Second, Text: "only"}},
	}).Prepare()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}

	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: flac,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "cut.flac")),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 2 * time.Second, End: 5 * time.Second}}},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	detail := warningDetail(res, WarnTagCarry)
	if detail == "" {
		t.Fatalf("no tag-carry warning; warnings = %+v", res.Warnings)
	}
	if !strings.Contains(detail, "no metadata carried") {
		t.Errorf("detail = %q, want the no-metadata lead: nothing remains on the output", detail)
	}
	if it := carryItem(t, res.TagCarry, CarrySyncedLyrics, ""); it.Disposition != DispositionRemoved || it.Count != 0 || it.Removed != 1 {
		t.Errorf("TagCarry synced lyrics = %+v, want the set removed by the cut, its 1 line counted", it)
	}
}

// A cut's remap keeps the report's item order and its counts honest: a set
// the cut emptied never entered the transfer, so its item is added where the
// transfer would have placed it; a set the destination dropped keeps the
// remap's count and takes what the cut removed beside it.
func TestTagCarryRemappedKeepsOrderAndCounts(t *testing.T) {
	tc := &TagCarry{Items: []CarryItem{
		{Kind: CarryField, Key: "TITLE", Count: 1},
		{Kind: CarryPictures, Count: 1},
		{Kind: CarrySyncedLyrics, Count: 1},
	}}
	tc.remapped(CarryChapters, cutRemap{kept: 0, removed: 3, input: 3}, false)
	var kinds []CarryKind
	for _, it := range tc.Items {
		kinds = append(kinds, it.Kind)
	}
	if want := []CarryKind{CarryField, CarryPictures, CarryChapters, CarrySyncedLyrics}; !slices.Equal(kinds, want) {
		t.Errorf("item kinds = %v, want the transfer's order %v", kinds, want)
	}
	if it := carryItem(t, tc, CarryChapters, ""); it.Disposition != DispositionRemoved || it.Count != 0 || it.Removed != 3 {
		t.Errorf("emptied set = %+v, want removed outright with 3 counted", it)
	}

	dropped := &TagCarry{Items: []CarryItem{{Kind: CarryChapters, Count: 2, Disposition: DispositionDropped, Reason: "wavpack cannot hold chapters"}}}
	dropped.remapped(CarryChapters, cutRemap{kept: 2, removed: 1, input: 3}, false)
	if it := dropped.Items[0]; it.Disposition != DispositionDropped || it.Count != 2 || it.Removed != 1 || it.Reason == "" {
		t.Errorf("dropped set after a cut = %+v, want the remap's 2 dropped with the 1 the cut took beside it", it)
	}
}
