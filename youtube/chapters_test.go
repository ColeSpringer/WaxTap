package youtube

import (
	"testing"
	"time"
)

// TestParseChaptersFromFixture proves the parser skips the decoy ytInitialData,
// prefers the creator DESCRIPTION_CHAPTERS entry over the AUTO_CHAPTERS and
// heatmap siblings, and closes the last chapter at the video duration.
func TestParseChaptersFromFixture(t *testing.T) {
	html := readFixture(t, "watch_page.html")
	chapters := parseChapters(html, 100*time.Second)
	want := []Chapter{
		{Title: "Intro", Start: 0, End: 30 * time.Second},
		{Title: "Verse", Start: 30 * time.Second, End: 90 * time.Second},
		{Title: "Outro", Start: 90 * time.Second, End: 100 * time.Second},
	}
	if len(chapters) != len(want) {
		t.Fatalf("got %d chapters, want %d: %+v", len(chapters), len(want), chapters)
	}
	for i, c := range chapters {
		if c != want[i] {
			t.Errorf("chapter %d = %+v, want %+v", i, c, want[i])
		}
	}
}

// TestParseChaptersLastChapterGuard checks the open-ended last chapter when the
// duration is unknown or not past the last chapter's start, so End is never < Start.
func TestParseChaptersLastChapterGuard(t *testing.T) {
	const markers = `{"playerOverlays":{"playerOverlayRenderer":{"decoratedPlayerBarRenderer":{"decoratedPlayerBarRenderer":{"playerBar":{"multiMarkersPlayerBarRenderer":{"markersMap":[{"key":"DESCRIPTION_CHAPTERS","value":{"chapters":[{"chapterRenderer":{"title":{"simpleText":"A"},"timeRangeStartMillis":0}},{"chapterRenderer":{"title":{"simpleText":"B"},"timeRangeStartMillis":60000}}]}}]}}}}}}}`
	html := []byte("var ytInitialData = " + markers + ";")

	t.Run("unknown duration leaves last open", func(t *testing.T) {
		chapters := parseChapters(html, 0)
		if len(chapters) != 2 {
			t.Fatalf("got %d chapters", len(chapters))
		}
		if chapters[0].End != 60*time.Second {
			t.Errorf("chapter 0 End = %s, want 1m0s", chapters[0].End)
		}
		if chapters[1].End != 0 {
			t.Errorf("last chapter End = %s, want 0 (open-ended)", chapters[1].End)
		}
	})

	t.Run("duration before last start leaves last open", func(t *testing.T) {
		chapters := parseChapters(html, 30*time.Second) // < 60s last start
		if chapters[len(chapters)-1].End != 0 {
			t.Errorf("last chapter End = %s, want 0", chapters[len(chapters)-1].End)
		}
	})

	t.Run("duration past last start closes it", func(t *testing.T) {
		chapters := parseChapters(html, 90*time.Second)
		if chapters[len(chapters)-1].End != 90*time.Second {
			t.Errorf("last chapter End = %s, want 1m30s", chapters[len(chapters)-1].End)
		}
	})
}

// TestParseChaptersHeatmapOnly returns no chapters when the only markersMap entry
// is a heatmap (no chapters array).
func TestParseChaptersHeatmapOnly(t *testing.T) {
	const html = `var ytInitialData = {"playerOverlays":{"playerOverlayRenderer":{"decoratedPlayerBarRenderer":{"decoratedPlayerBarRenderer":{"playerBar":{"multiMarkersPlayerBarRenderer":{"markersMap":[{"key":"HEATSEEKER","value":{"heatmap":{"heatmapRenderer":{"maxHeightDp":40}}}}]}}}}}}};`
	if got := parseChapters([]byte(html), 100*time.Second); got != nil {
		t.Errorf("parseChapters = %+v, want nil for a heatmap-only bar", got)
	}
}

// TestParseChaptersNone returns nil when the page carries no player bar at all.
func TestParseChaptersNone(t *testing.T) {
	if got := parseChapters([]byte(`<html><body>no data</body></html>`), time.Minute); got != nil {
		t.Errorf("parseChapters = %+v, want nil", got)
	}
}

// TestExtractJSONObjectFromIterates confirms the offset-capable extractor walks
// past a first (decoy) match to a later one.
func TestExtractJSONObjectFromIterates(t *testing.T) {
	const s = `ytInitialData = {"a":1}; more ytInitialData = {"b":2};`
	obj, end, ok := extractJSONObjectFrom(s, "ytInitialData", 0)
	if !ok || string(obj) != `{"a":1}` {
		t.Fatalf("first match = %q ok=%v", obj, ok)
	}
	obj2, _, ok := extractJSONObjectFrom(s, "ytInitialData", end)
	if !ok || string(obj2) != `{"b":2}` {
		t.Fatalf("second match = %q ok=%v", obj2, ok)
	}
}
