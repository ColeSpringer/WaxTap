package youtube

import (
	"sort"
	"time"
)

// chapterMarkerKey is the markersMap entry key for creator-authored chapters. It
// is preferred over AUTO_CHAPTERS when both are present.
const chapterMarkerKey = "DESCRIPTION_CHAPTERS"

// chapterMarkerEntry is one markersMap entry. A chapters entry carries a chapters
// array; the heatmap sibling carries none.
type chapterMarkerEntry struct {
	Key   string `json:"key"`
	Value struct {
		Chapters []chapterItem `json:"chapters"`
	} `json:"value"`
}

// chapterItem is one chapter's renderer.
type chapterItem struct {
	ChapterRenderer struct {
		Title                textRuns `json:"title"`
		TimeRangeStartMillis int64    `json:"timeRangeStartMillis"`
	} `json:"chapterRenderer"`
}

// buildChapters turns the page's chapter renderers into chapters in playback
// order. It returns nil when there are none.
//
// duration bounds the final chapter's End: End is set to duration only when
// duration is known and past the last chapter's start; otherwise the last
// chapter stays open-ended (End == 0) rather than emitting End < Start.
func buildChapters(items []chapterItem, duration time.Duration) []Chapter {
	chapters := make([]Chapter, 0, len(items))
	for _, it := range items {
		title := it.ChapterRenderer.Title.String()
		if title == "" {
			continue
		}
		chapters = append(chapters, Chapter{
			Title: title,
			Start: time.Duration(it.ChapterRenderer.TimeRangeStartMillis) * time.Millisecond,
		})
	}
	if len(chapters) == 0 {
		return nil
	}
	// Sort by start so End derivation and the last-chapter guard are correct even if
	// the source order is not strictly increasing. YouTube usually orders them, so
	// this is defensive; a stable sort keeps equal-start ties in source order.
	sort.SliceStable(chapters, func(i, j int) bool { return chapters[i].Start < chapters[j].Start })
	for i := 0; i < len(chapters)-1; i++ {
		chapters[i].End = chapters[i+1].Start
	}
	if last := len(chapters) - 1; duration > chapters[last].Start {
		chapters[last].End = duration
	}
	return chapters
}

// selectChapterItems picks the chapters from the markersMap, preferring the
// creator DESCRIPTION_CHAPTERS entry over AUTO_CHAPTERS and never selecting the
// heatmap sibling (its value carries no chapters).
func selectChapterItems(markersMap []chapterMarkerEntry) []chapterItem {
	for _, e := range markersMap {
		if e.Key == chapterMarkerKey && len(e.Value.Chapters) > 0 {
			return e.Value.Chapters
		}
	}
	for _, e := range markersMap {
		if len(e.Value.Chapters) > 0 {
			return e.Value.Chapters
		}
	}
	return nil
}
