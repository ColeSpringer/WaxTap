package waxtap

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
)

func sec(n float64) time.Duration { return time.Duration(n * float64(time.Second)) }

// lyricSet builds a one-language set from bare timestamps.
func lyricSet(times ...time.Duration) waxlabel.SyncedLyrics {
	sl := waxlabel.SyncedLyrics{Language: "eng"}
	for i, t := range times {
		sl.Lines = append(sl.Lines, waxlabel.SyncedLine{Time: t, Text: string(rune('a' + i))})
	}
	return sl
}

func lineTimes(sl waxlabel.SyncedLyrics) []time.Duration {
	out := make([]time.Duration, 0, len(sl.Lines))
	for _, l := range sl.Lines {
		out = append(out, l.Time)
	}
	return out
}

func TestRemapSyncedLyrics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          []waxlabel.SyncedLyrics
		cut         *appliedCut
		wantSets    int
		wantTimes   []time.Duration // times of the first surviving set
		wantDropped int
	}{
		{
			name:        "head cut shifts what follows",
			in:          []waxlabel.SyncedLyrics{lyricSet(sec(2), sec(4))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: sec(1), End: sec(10)}}, total: sec(10)},
			wantSets:    1,
			wantTimes:   []time.Duration{sec(1), sec(3)},
			wantDropped: 0,
		},
		{
			name:        "a line inside the removed span is dropped",
			in:          []waxlabel.SyncedLyrics{lyricSet(sec(1), sec(3), sec(6))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: 0, End: sec(2)}, {Start: sec(5), End: sec(10)}}, total: sec(10)},
			wantSets:    1,
			wantTimes:   []time.Duration{sec(1), sec(3)}, // 3s removed; 6s lands at 2+1
			wantDropped: 1,
		},
		{
			name:        "a line at zero under a head cut is dropped",
			in:          []waxlabel.SyncedLyrics{lyricSet(0, sec(4))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: sec(1), End: sec(10)}}, total: sec(10)},
			wantSets:    1,
			wantTimes:   []time.Duration{sec(3)},
			wantDropped: 1,
		},
		{
			name:        "an emptied set is removed and its lines counted",
			in:          []waxlabel.SyncedLyrics{lyricSet(sec(1), sec(2))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: sec(6), End: sec(10)}}, total: sec(10)},
			wantSets:    0,
			wantDropped: 2,
		},
		{
			name: "sets are counted independently",
			in: []waxlabel.SyncedLyrics{
				lyricSet(sec(1)),
				lyricSet(sec(7)),
			},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: 0, End: sec(2)}, {Start: sec(5), End: sec(10)}}, total: sec(10)},
			wantSets:    2,
			wantTimes:   []time.Duration{sec(1)},
			wantDropped: 0,
		},
		{
			name:        "a line at the declared end survives a mid cut",
			in:          []waxlabel.SyncedLyrics{lyricSet(sec(1), sec(10))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: 0, End: sec(2)}, {Start: sec(5), End: sec(10)}}, total: sec(10)},
			wantSets:    1,
			wantTimes:   []time.Duration{sec(1), sec(7)}, // the end line stays at the new end
			wantDropped: 0,
		},
		{
			name:        "a line past the declared end is not the cut's to drop",
			in:          []waxlabel.SyncedLyrics{lyricSet(sec(1), sec(12))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: 0, End: sec(2)}, {Start: sec(5), End: sec(10)}}, total: sec(10)},
			wantSets:    1,
			wantTimes:   []time.Duration{sec(1), sec(7)},
			wantDropped: 0,
		},
		{
			name:        "a line inside a removed tail is dropped",
			in:          []waxlabel.SyncedLyrics{lyricSet(sec(1), sec(9))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: 0, End: sec(8)}}, total: sec(10)},
			wantSets:    1,
			wantTimes:   []time.Duration{sec(1)},
			wantDropped: 1,
		},
		{
			name:        "a crossfade join pulls the later line back",
			in:          []waxlabel.SyncedLyrics{lyricSet(sec(6))},
			cut:         &appliedCut{keeps: []cutrange.Range{{Start: 0, End: sec(2)}, {Start: sec(5), End: sec(10)}}, crossfade: sec(0.5), total: sec(10)},
			wantSets:    1,
			wantTimes:   []time.Duration{sec(2.5)}, // 2s kept, less the 0.5s overlap, plus 1s in
			wantDropped: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := remapSyncedLyrics(tc.in, tc.cut)
			if len(got) != tc.wantSets {
				t.Fatalf("sets = %d, want %d: %+v", len(got), tc.wantSets, got)
			}
			if dropped != tc.wantDropped {
				t.Errorf("dropped = %d, want %d", dropped, tc.wantDropped)
			}
			if tc.wantTimes != nil {
				if times := lineTimes(got[0]); !equalTimes(times, tc.wantTimes) {
					t.Errorf("times = %v, want %v", times, tc.wantTimes)
				}
			}
		})
	}
}

func equalTimes(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// lyricsDropNote must say which fraction of the document's sets went, not just
// count lines: losing a whole language among several is its own fact.
func TestLyricsDropNote(t *testing.T) {
	for _, tc := range []struct {
		dropped, setsDropped, origSets int
		want                           string
	}{
		{1, 0, 1, "1 synced lyric line pointed at removed audio and was dropped"},
		{2, 1, 1, "2 synced lyric lines pointed at removed audio and were dropped; the set was dropped"},
		{3, 1, 2, "3 synced lyric lines pointed at removed audio and were dropped; 1 of 2 sets were dropped"},
		{4, 2, 2, "4 synced lyric lines pointed at removed audio and were dropped; all 2 sets were dropped"},
	} {
		if got := lyricsDropNote(tc.dropped, tc.setsDropped, tc.origSets); got != tc.want {
			t.Errorf("lyricsDropNote(%d, %d, %d) = %q, want %q", tc.dropped, tc.setsDropped, tc.origSets, got, tc.want)
		}
	}
}

// Some taggers store LRC in the ordinary lyrics field rather than in a synced
// set. A cut moves it the same way: a line whose instant lands in removed
// audio is dropped, and the rest shift by the audio taken before them.
func TestProcessRemapsLRCTextThroughACut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 6, "flac")

	doc, err := waxlabel.ParseFile(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	plan, perr := doc.Edit().
		Set(tag.Title, "Lyric Fixture").
		Set(tag.Lyrics, "[00:01.00]line one\n[00:04.00]line four\n[00:05.50]line five").
		Prepare()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "cut.flac")
	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			// Keeps [0,2s) and [4.5s,6s): line one at 1.0 s is removed with
			// the first span's tail, line five at 5.5 s survives into the
			// second, and line four at 4.0 s falls in the removed middle.
			Cut: &CutSpec{Ranges: []TimeRange{{Start: 2 * time.Second, End: 4500 * time.Millisecond}}},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	got, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	vals, ok := got.Get(tag.Lyrics)
	if !ok || len(vals) != 1 {
		t.Fatalf("output LYRICS = %v (ok %v), want one value", vals, ok)
	}
	lines := waxlabel.ParseLRCFull(vals[0])
	if len(lines) != 2 {
		t.Fatalf("output holds %d timed lines, want 2: %q", len(lines), vals[0])
	}
	// Line one keeps its instant (nothing was removed before 1 s); line five
	// shifts back by the 2.5 s the cut removed.
	if lines[0].Time != time.Second {
		t.Errorf("line one at %v, want 1s", lines[0].Time)
	}
	if want := 3 * time.Second; lines[1].Time != want {
		t.Errorf("line five at %v, want %v (5.5s less the 2.5s removed)", lines[1].Time, want)
	}
	if it := carryItem(t, res.TagCarry, CarryField, string(tag.Lyrics)); it.Removed != 1 {
		t.Errorf("LYRICS carry item = %+v, want Removed 1", it)
	}
	detail := warningDetail(res, WarnTagCarry)
	if !strings.Contains(detail, "timed lyric line") {
		t.Errorf("warning detail = %q, want it to name the dropped timed line", detail)
	}
}

// A cut that removes audio before every line drops none of them and shifts all
// of them. Reporting nothing dropped is right; leaving the field alone is not,
// because each timestamp would then point past its own words.
func TestProcessShiftsLRCTextWhenNothingIsDropped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 6, "flac")

	doc, err := waxlabel.ParseFile(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	plan, perr := doc.Edit().
		Set(tag.Lyrics, "[00:04.00]line four\n[00:05.00]line five").
		Prepare()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "cut.flac")
	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			// Removes [0, 2s): both lines survive, both move back by 2 s.
			Cut: &CutSpec{Ranges: []TimeRange{{Start: 0, End: 2 * time.Second}}},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}

	got, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	vals, ok := got.Get(tag.Lyrics)
	if !ok || len(vals) != 1 {
		t.Fatalf("output LYRICS = %v (ok %v), want one value", vals, ok)
	}
	lines := waxlabel.ParseLRCFull(vals[0])
	if len(lines) != 2 {
		t.Fatalf("output holds %d timed lines, want 2: %q", len(lines), vals[0])
	}
	if lines[0].Time != 2*time.Second || lines[1].Time != 3*time.Second {
		t.Errorf("lines at %v and %v, want 2s and 3s (4s and 5s less the 2s removed)", lines[0].Time, lines[1].Time)
	}
	// Nothing was dropped, so nothing is counted as removed.
	if res.TagCarry != nil {
		for _, it := range res.TagCarry.Items {
			if it.Kind == CarryField && it.Key == string(tag.Lyrics) && it.Removed != 0 {
				t.Errorf("LYRICS item = %+v, want Removed 0: no line was dropped", it)
			}
		}
	}
}

// An LRC sheet is more than its timed lines: ID tags, section headers, prose,
// and blanks all belong to the user's text. Rebuilding the field from the
// timed lines alone would delete every one of them.
func TestProcessKeepsUntimedLRCContentThroughACut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 6, "flac")

	const sheet = "[ar:Some Artist]\n[ti:Some Title]\n\n[Verse 1]\n[00:01.00]line one\nan untimed aside\n[00:04.00]line four\n[00:05.50]line five"
	doc, err := waxlabel.ParseFile(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	plan, perr := doc.Edit().Set(tag.Lyrics, sheet).Prepare()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "cut.flac")
	if _, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			// Keeps [0,2s) and [4.5s,6s): line one stays, line four goes with
			// the removed middle, line five shifts back by 2.5 s.
			Cut: &CutSpec{Ranges: []TimeRange{{Start: 2 * time.Second, End: 4500 * time.Millisecond}}},
		},
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	got, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	vals, ok := got.Get(tag.Lyrics)
	if !ok || len(vals) != 1 {
		t.Fatalf("output LYRICS = %v (ok %v), want one value", vals, ok)
	}
	text := vals[0]
	for _, want := range []string{"[ar:Some Artist]", "[ti:Some Title]", "[Verse 1]", "an untimed aside"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lost %q:\n%s", want, text)
		}
	}
	lines := waxlabel.ParseLRCFull(text)
	if len(lines) != 2 {
		t.Fatalf("output holds %d timed lines, want 2 (line four was removed):\n%s", len(lines), text)
	}
	if lines[0].Time != time.Second {
		t.Errorf("line one at %v, want 1s", lines[0].Time)
	}
	if want := 3 * time.Second; lines[1].Time != want {
		t.Errorf("line five at %v, want %v", lines[1].Time, want)
	}
}

// An [offset:] tag shifts every timestamp in the sheet and the parser applies
// it, so rewriting the times while the tag stayed would double-apply it. The
// sheet is left exactly as it is and the run says so.
func TestProcessLeavesAnOffsetLRCSheetAloneAndSaysSo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 6, "flac")

	const sheet = "[offset:+500]\n[00:04.00]line four\n[00:05.00]line five"
	doc, err := waxlabel.ParseFile(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	plan, perr := doc.Edit().Set(tag.Lyrics, sheet).Prepare()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "cut.flac")
	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 0, End: 2 * time.Second}}},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	got, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	vals, _ := got.Get(tag.Lyrics)
	if len(vals) != 1 || vals[0] != sheet {
		t.Errorf("LYRICS = %q, want the sheet untouched", vals)
	}
	if d := warningDetail(res, WarnTagCarry); !strings.Contains(d, "offset") {
		t.Errorf("warning = %q, want it to name the offset tag it would not touch", d)
	}
}
