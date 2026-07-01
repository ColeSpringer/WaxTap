package youtube

import (
	"encoding/json"
	"sort"
	"time"
)

// chapterMarkerKey is the markersMap entry key for creator-authored chapters. It
// is preferred over AUTO_CHAPTERS when both are present.
const chapterMarkerKey = "DESCRIPTION_CHAPTERS"

// ytInitialData is the subset of the watch page's ytInitialData that carries
// chapter markers, nested under the decorated player bar (the renderer wraps a
// same-named renderer). The heatmap lives in a sibling markersMap entry whose
// value has no chapters array, so it is skipped by selection.
type ytInitialData struct {
	PlayerOverlays struct {
		PlayerOverlayRenderer struct {
			DecoratedPlayerBarRenderer struct {
				DecoratedPlayerBarRenderer struct {
					PlayerBar struct {
						MultiMarkersPlayerBarRenderer struct {
							MarkersMap []chapterMarkerEntry `json:"markersMap"`
						} `json:"multiMarkersPlayerBarRenderer"`
					} `json:"playerBar"`
				} `json:"decoratedPlayerBarRenderer"`
			} `json:"decoratedPlayerBarRenderer"`
		} `json:"playerOverlayRenderer"`
	} `json:"playerOverlays"`
}

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

// parseChapters extracts chapter markers from watch-page HTML. Chapters live in
// ytInitialData, not the /player response, so this is reachable only when the
// watch page has been fetched. It returns nil when the page carries no chapters.
//
// duration bounds the final chapter's End: End is set to duration only when
// duration is known and past the last chapter's start; otherwise the last
// chapter stays open-ended (End == 0) rather than emitting End < Start.
func parseChapters(body []byte, duration time.Duration) []Chapter {
	items := findChapterItems(string(body))
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

// findChapterItems scans successive ytInitialData objects, since the first match
// in a watch page can be a decoy assignment, and returns the chapters from the
// best markersMap entry of the first object that actually carries a player bar.
func findChapterItems(html string) []chapterItem {
	const marker = "ytInitialData"
	for start := 0; ; {
		obj, end, ok := extractJSONObjectFrom(html, marker, start)
		if !ok {
			return nil
		}
		start = end
		var data ytInitialData
		if err := json.Unmarshal(obj, &data); err != nil {
			continue // a decoy or unrelated object; try the next match
		}
		markersMap := data.PlayerOverlays.PlayerOverlayRenderer.
			DecoratedPlayerBarRenderer.DecoratedPlayerBarRenderer.
			PlayerBar.MultiMarkersPlayerBarRenderer.MarkersMap
		if len(markersMap) == 0 {
			continue // parsed, but not the object holding the player bar
		}
		// The player bar was found; this object is authoritative. Return its
		// chapters (possibly none, e.g. a heatmap-only bar).
		return selectChapterItems(markersMap)
	}
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
