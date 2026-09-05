package waxtap

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/youtube"
)

func TestMergeWatchPageMeta(t *testing.T) {
	published := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	chapters := []youtube.Chapter{{Title: "Intro"}}

	t.Run("fills what the client omitted", func(t *testing.T) {
		v := &youtube.Video{}
		mergeWatchPageMeta(v, youtube.WatchPageMeta{PublishDate: published, Chapters: chapters, Unlisted: true})
		if !v.PublishDate.Equal(published) {
			t.Errorf("PublishDate = %v, want %v", v.PublishDate, published)
		}
		if len(v.Chapters) != 1 {
			t.Errorf("Chapters = %v, want the watch-page chapters", v.Chapters)
		}
		if v.Availability != youtube.AvailabilityUnlisted {
			t.Errorf("Availability = %v, want unlisted", v.Availability)
		}
	})

	// The pass backfills and never replaces, so what extraction supplied survives
	// whether or not the watch page found its own.
	t.Run("keeps existing values", func(t *testing.T) {
		earlier := published.AddDate(-1, 0, 0)
		for name, meta := range map[string]youtube.WatchPageMeta{
			"watch page has none": {PublishDate: published},
			"watch page has its own": {
				PublishDate: published,
				Chapters:    []youtube.Chapter{{Title: "Sponsor"}, {Title: "Outro"}},
			},
		} {
			t.Run(name, func(t *testing.T) {
				v := &youtube.Video{PublishDate: earlier, Chapters: chapters}
				mergeWatchPageMeta(v, meta)
				if !v.PublishDate.Equal(earlier) {
					t.Errorf("PublishDate = %v, want the client's %v preserved", v.PublishDate, earlier)
				}
				if len(v.Chapters) != 1 || v.Chapters[0].Title != "Intro" {
					t.Errorf("Chapters = %v, want the client's chapters preserved", v.Chapters)
				}
			})
		}
	})
}

// TestEnrichEntriesCancellation covers the pre-canceled path: return
// context.Canceled without making per-entry calls.
func TestEnrichEntriesCancellation(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pl := &Playlist{Entries: []PlaylistEntry{{VideoID: "testVideo01"}, {VideoID: "testVideo02"}}}
	if err := c.enrichEntries(ctx, pl, EnumerateOptions{Enrich: true}); !errors.Is(err, context.Canceled) {
		t.Errorf("enrichEntries(canceled) = %v, want context.Canceled", err)
	}
	if len(pl.Errors) != 0 {
		t.Errorf("a canceled enrich should make no per-entry calls; got errors %v", pl.Errors)
	}
}

// TestEnrichEntriesProgressReachesTotal verifies that OnEnrichProgress fires once
// per entry and reaches (total, total). Invalid IDs make each Info call fail
// before network access, so the test exercises progress on item failures.
func TestEnrichEntriesProgressReachesTotal(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	pl := &Playlist{Entries: []PlaylistEntry{
		{VideoID: "!bad1"}, {VideoID: "!bad2"}, {VideoID: "!bad3"}, {VideoID: "!bad4"}, {VideoID: "!bad5"},
	}}
	total := len(pl.Entries)

	var mu sync.Mutex
	calls, maxDone, lastTotal := 0, 0, 0
	onProgress := func(done, tot int) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if done > maxDone {
			maxDone = done
		}
		lastTotal = tot
	}
	// Item failures are expected (invalid IDs); only progress accounting matters.
	_ = c.enrichEntries(context.Background(), pl, EnumerateOptions{Enrich: true, OnEnrichProgress: onProgress})

	if calls != total {
		t.Errorf("onProgress called %d times, want %d (once per entry)", calls, total)
	}
	if maxDone != total || lastTotal != total {
		t.Errorf("progress reached (%d, %d), want (%d, %d)", maxDone, lastTotal, total, total)
	}
}

// TestEnrichEntriesBoundedProgress pins the bound at the loop: with MaxEnrich
// set, only the leading entries are attempted, each failure names its entry,
// progress ends at (MaxEnrich, MaxEnrich), and the entries past the bound are
// neither asked about nor reported.
func TestEnrichEntriesBoundedProgress(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	pl := &Playlist{Entries: []PlaylistEntry{
		{VideoID: "!bad1", Index: 3}, {VideoID: "!bad2", Index: 4}, {VideoID: "!bad3", Index: 5}, {VideoID: "!bad4", Index: 6},
	}}

	var mu sync.Mutex
	var last [2]int
	calls := 0
	onProgress := func(done, tot int) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		last = [2]int{done, tot}
	}
	_ = c.enrichEntries(context.Background(), pl, EnumerateOptions{Enrich: true, MaxEnrich: 2, OnEnrichProgress: onProgress})

	if calls != 2 || last != [2]int{2, 2} {
		t.Errorf("progress: %d calls ending at %v, want 2 ending at (2, 2)", calls, last)
	}
	if len(pl.Errors) != 2 {
		t.Fatalf("Errors = %v, want one per attempted entry", pl.Errors)
	}
	var indexes []int
	for _, perr := range pl.Errors {
		ee, ok := errors.AsType[*EnrichError](perr)
		if !ok {
			t.Fatalf("error %v is a %T, want *EnrichError", perr, perr)
		}
		indexes = append(indexes, ee.Index)
	}
	slices.Sort(indexes)
	if !slices.Equal(indexes, []int{3, 4}) {
		t.Errorf("failed entry indexes = %v, want the playlist positions of the two attempted entries", indexes)
	}
}
