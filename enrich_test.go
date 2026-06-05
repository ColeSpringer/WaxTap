package waxtap

import (
	"context"
	"errors"
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
	if err := c.enrichEntries(ctx, pl); !errors.Is(err, context.Canceled) {
		t.Errorf("enrichEntries(canceled) = %v, want context.Canceled", err)
	}
	if len(pl.Errors) != 0 {
		t.Errorf("a canceled enrich should make no per-entry calls; got errors %v", pl.Errors)
	}
}
