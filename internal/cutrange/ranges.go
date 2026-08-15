package cutrange

import (
	"sort"
	"time"

	"github.com/colespringer/waxtap/v3/sponsorblock"
)

// Range is a half-open [Start, End) time span. End must be greater than Start to
// describe a non-empty span.
type Range struct {
	Start time.Duration // inclusive start offset
	End   time.Duration // exclusive end offset
}

// Duration returns the span length, or zero when the range is empty or inverted.
func (r Range) Duration() time.Duration {
	if r.End <= r.Start {
		return 0
	}
	return r.End - r.Start
}

// RangesFromSegments converts SponsorBlock skip segments into removal ranges.
// Merge/Keeps handle ordering, clamping, and overlap merging after the media
// duration is known.
func RangesFromSegments(segs []sponsorblock.Segment) []Range {
	if len(segs) == 0 {
		return nil
	}
	out := make([]Range, 0, len(segs))
	for _, s := range segs {
		out = append(out, Range{Start: s.Start, End: s.End})
	}
	return out
}

// Merge normalizes removal ranges against a known media duration: it clamps each
// range to [0, total], drops empty ones, sorts by start, and merges overlapping
// or touching ranges. The result is sorted and disjoint. total must be > 0.
func Merge(ranges []Range, total time.Duration) []Range {
	clamped := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		r.Start = max(r.Start, 0)
		r.End = min(r.End, total)
		if r.Start < r.End {
			clamped = append(clamped, r)
		}
	}
	if len(clamped) == 0 {
		return nil
	}
	sort.Slice(clamped, func(i, j int) bool {
		if clamped[i].Start != clamped[j].Start {
			return clamped[i].Start < clamped[j].Start
		}
		return clamped[i].End < clamped[j].End
	})

	merged := []Range{clamped[0]}
	for _, r := range clamped[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End { // overlapping or touching
			last.End = max(last.End, r.End)
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// Keeps returns the spans to retain: the complement of the (merged) removal
// ranges within [0, total], in order. It is what the cut filtergraph trims and
// concatenates. With no effective removals it returns the whole [0, total] span;
// when the removals cover everything it returns nil. total must be > 0.
func Keeps(removals []Range, total time.Duration) []Range {
	merged := Merge(removals, total)
	if len(merged) == 0 {
		if total <= 0 {
			return nil
		}
		return []Range{{Start: 0, End: total}}
	}

	var keeps []Range
	cursor := time.Duration(0)
	for _, r := range merged {
		if r.Start > cursor {
			keeps = append(keeps, Range{Start: cursor, End: r.Start})
		}
		cursor = r.End
	}
	if cursor < total {
		keeps = append(keeps, Range{Start: cursor, End: total})
	}
	return keeps
}

// MapTime maps a source-timeline offset onto the output timeline of a cut:
// the keep spans concatenated in order, each join overlapped by crossfade. An
// offset inside a removed span maps to the join where its surrounding audio
// meets; offsets before the first keep map to 0 and offsets past the last to
// the output duration. keeps must be sorted and disjoint, as Keeps returns.
func MapTime(keeps []Range, crossfade time.Duration, t time.Duration) time.Duration {
	var out time.Duration
	for i, k := range keeps {
		if i > 0 && crossfade > 0 {
			out = max(out-crossfade, 0)
		}
		if t < k.Start {
			return out
		}
		if t < k.End {
			return out + (t - k.Start)
		}
		out += k.Duration()
	}
	return out
}

// OutputDuration is the length of the rendered keep ranges. A crossfade overlaps
// adjacent spans, so it shortens the output by one crossfade per join.
func OutputDuration(keeps []Range, crossfade time.Duration) time.Duration {
	var total time.Duration
	for _, k := range keeps {
		total += k.Duration()
	}
	if crossfade > 0 && len(keeps) > 1 {
		total -= crossfade * time.Duration(len(keeps)-1)
	}
	return max(total, 0)
}
