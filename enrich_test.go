package waxtap

import (
	"context"
	"errors"
	"sync"
	"testing"
)

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
	if err := c.enrichEntries(ctx, pl, nil); !errors.Is(err, context.Canceled) {
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
	_ = c.enrichEntries(context.Background(), pl, onProgress)

	if calls != total {
		t.Errorf("onProgress called %d times, want %d (once per entry)", calls, total)
	}
	if maxDone != total || lastTotal != total {
		t.Errorf("progress reached (%d, %d), want (%d, %d)", maxDone, lastTotal, total, total)
	}
}
