package waxtap

import (
	"cmp"
	"slices"
	"time"

	"github.com/colespringer/waxlabel"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
)

// appliedCut describes a rendered timeline cut, for remapping source-timeline
// metadata (chapter marks) onto the delivered audio.
type appliedCut struct {
	keeps     []cutrange.Range
	crossfade time.Duration
	total     time.Duration // source duration, resolving until-next chapter ends
}

// appliedCutFrom extracts the cut a pipeline run applied, or nil when the
// delivered timeline matches the source.
func appliedCutFrom(pres pipeline.Result) *appliedCut {
	if !pres.Cut || len(pres.Keeps) == 0 {
		return nil
	}
	return &appliedCut{keeps: pres.Keeps, crossfade: pres.Crossfade, total: pres.SourceDuration}
}

// remapChapters maps chapters from the source timeline onto a cut's output
// timeline. A chapter whose content was entirely removed is dropped; the rest
// shift by the audio removed before them.
//
// A zero End means "until the next chapter" and stays zero: after the shift
// the next surviving chapter still starts exactly where this one's content
// ends, so the convention remaps itself. The span is still resolved (against
// the next chapter, or total for the last) to decide whether any content
// survived.
func remapChapters(chs []waxlabel.Chapter, cut *appliedCut) []waxlabel.Chapter {
	// Sort a copy by start first: until-next resolution reads the following
	// chapter, and containers do not promise file order is timeline order. An
	// out-of-order list would resolve inverted spans and drop live chapters.
	chs = slices.Clone(chs)
	slices.SortStableFunc(chs, func(a, b waxlabel.Chapter) int { return cmp.Compare(a.Start, b.Start) })
	out := make([]waxlabel.Chapter, 0, len(chs))
	for i, ch := range chs {
		end := ch.End
		if end == 0 {
			if i+1 < len(chs) {
				end = chs[i+1].Start
			} else {
				end = cut.total
			}
		}
		ms := cutrange.MapTime(cut.keeps, cut.crossfade, ch.Start)
		me := cutrange.MapTime(cut.keeps, cut.crossfade, end)
		if me <= ms {
			continue
		}
		nc := waxlabel.Chapter{Start: ms, Title: ch.Title}
		if ch.End > 0 {
			nc.End = me
		}
		out = append(out, nc)
	}
	return out
}

// remapSyncedLyrics maps lyric sets from the source timeline onto a cut's
// output timeline. A line whose instant lies in a removed span is dropped,
// because its timestamp would otherwise point at audio that is no longer there;
// the rest shift by the audio removed before them. Sets left with no lines are
// removed, and dropped counts every line that went.
//
// The rule is the line's instant, not a span, because a lyric line has no
// duration of its own: it is the moment its text appears. That makes one case
// worth stating plainly: a cut starting at zero removes a line at 0:00, since
// the instant it marks is exactly the audio the cut took.
//
// Only instants inside the source ([0, total)) are the cut's to judge. A line
// at or past the declared end marks no removed audio whatever the cut did, so
// it survives and shifts like everything after the removals, which lands it at
// the output's end.
func remapSyncedLyrics(sls []waxlabel.SyncedLyrics, cut *appliedCut) (out []waxlabel.SyncedLyrics, dropped int) {
	removed := func(t time.Duration) bool {
		if t < 0 || t >= cut.total {
			return false // outside the source: nothing there was cut
		}
		for _, k := range cut.keeps {
			if t >= k.Start && t < k.End {
				return false
			}
		}
		return true
	}
	for _, sl := range sls {
		lines := make([]waxlabel.SyncedLine, 0, len(sl.Lines))
		for _, l := range sl.Lines {
			if removed(l.Time) {
				dropped++
				continue
			}
			l.Time = cutrange.MapTime(cut.keeps, cut.crossfade, l.Time)
			lines = append(lines, l)
		}
		if len(lines) == 0 {
			// WaxLabel's SetSyncedLyrics silently skips an empty set, so an
			// emptied one is dropped here where its lines can still be counted.
			continue
		}
		sl.Lines = lines
		out = append(out, sl)
	}
	return out, dropped
}
