package waxtap

import (
	"testing"
	"time"

	"github.com/colespringer/waxlabel"

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
