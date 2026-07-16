package cutrange

import (
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/sponsorblock"
)

// s builds a duration from whole seconds for readable range literals.
func s(n int) time.Duration { return time.Duration(n) * time.Second }

func rangesEqual(a, b []Range) bool {
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

func TestMerge(t *testing.T) {
	const total = 100 * time.Second
	cases := []struct {
		name string
		in   []Range
		want []Range
	}{
		{"empty", nil, nil},
		{"single", []Range{{s(10), s(20)}}, []Range{{s(10), s(20)}}},
		{
			"sorts-by-start",
			[]Range{{s(30), s(40)}, {s(10), s(20)}},
			[]Range{{s(10), s(20)}, {s(30), s(40)}},
		},
		{
			"merges-overlap",
			[]Range{{s(10), s(25)}, {s(20), s(30)}},
			[]Range{{s(10), s(30)}},
		},
		{
			"merges-touching",
			[]Range{{s(10), s(20)}, {s(20), s(30)}},
			[]Range{{s(10), s(30)}},
		},
		{
			"keeps-disjoint",
			[]Range{{s(10), s(20)}, {s(30), s(40)}},
			[]Range{{s(10), s(20)}, {s(30), s(40)}},
		},
		{
			"clamps-to-bounds",
			[]Range{{-s(5), s(20)}, {s(90), s(150)}},
			[]Range{{0, s(20)}, {s(90), total}},
		},
		{"drops-empty-after-clamp", []Range{{s(50), s(50)}, {s(200), s(300)}}, nil},
		{
			"nested-absorbed",
			[]Range{{s(10), s(50)}, {s(20), s(30)}},
			[]Range{{s(10), s(50)}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Merge(c.in, total)
			if !rangesEqual(got, c.want) {
				t.Errorf("Merge(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestKeeps(t *testing.T) {
	const total = 100 * time.Second
	cases := []struct {
		name string
		in   []Range
		want []Range
	}{
		{"no-removals-keeps-all", nil, []Range{{0, total}}},
		{
			"middle-removal-splits",
			[]Range{{s(40), s(60)}},
			[]Range{{0, s(40)}, {s(60), total}},
		},
		{
			"leading-removal",
			[]Range{{0, s(10)}},
			[]Range{{s(10), total}},
		},
		{
			"trailing-removal",
			[]Range{{s(90), total}},
			[]Range{{0, s(90)}},
		},
		{
			"two-removals-three-keeps",
			[]Range{{s(10), s(20)}, {s(70), s(80)}},
			[]Range{{0, s(10)}, {s(20), s(70)}, {s(80), total}},
		},
		{"whole-track-removed", []Range{{0, total}}, nil},
		{"over-range-removed", []Range{{-s(10), s(200)}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Keeps(c.in, total)
			if !rangesEqual(got, c.want) {
				t.Errorf("Keeps(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestOutputDuration(t *testing.T) {
	keeps := []Range{{0, s(10)}, {s(20), s(40)}} // 10s + 20s = 30s
	if got := OutputDuration(keeps, 0); got != s(30) {
		t.Errorf("OutputDuration(no crossfade) = %v, want 30s", got)
	}
	// Two segments, one join: crossfade overlaps once.
	if got := OutputDuration(keeps, 2*time.Second); got != s(28) {
		t.Errorf("OutputDuration(2s crossfade) = %v, want 28s", got)
	}
	// A single span has no join, so crossfade does not shorten it.
	if got := OutputDuration([]Range{{0, s(10)}}, 2*time.Second); got != s(10) {
		t.Errorf("OutputDuration(single span) = %v, want 10s", got)
	}
}

func TestRangesFromSegments(t *testing.T) {
	segs := []sponsorblock.Segment{
		{Category: sponsorblock.CategoryMusicOffTopic, Start: s(5), End: s(15)},
		{Category: sponsorblock.CategoryIntro, Start: s(40), End: s(50)},
	}
	got := RangesFromSegments(segs)
	want := []Range{{s(5), s(15)}, {s(40), s(50)}}
	if !rangesEqual(got, want) {
		t.Errorf("RangesFromSegments = %v, want %v", got, want)
	}
	if RangesFromSegments(nil) != nil {
		t.Error("RangesFromSegments(nil) should be nil")
	}
}
