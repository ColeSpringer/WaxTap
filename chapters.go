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
