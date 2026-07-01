package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// lockupItem builds a video-lockup playlist item; an empty title makes toEntry
// fail while itemVideoID still reports the video ID.
func lockupItem(t *testing.T, id, title string) playlistItem {
	t.Helper()
	body := fmt.Sprintf(`{"lockupViewModel":{"contentId":%q,"contentType":"LOCKUP_CONTENT_TYPE_VIDEO","metadata":{"lockupMetadataViewModel":{"title":{"content":%q}}}}}`, id, title)
	var it playlistItem
	if err := json.Unmarshal([]byte(body), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

// TestEnumerateIndexAdvancesOnParseError checks a video entry that fails toEntry
// still consumes a playlist position, so later entries keep their true Index.
func TestEnumerateIndexAdvancesOnParseError(t *testing.T) {
	c := newTestClient(nil) // appendPlaylistItems does no network work
	pl := &Playlist{}
	items := []playlistItem{
		lockupItem(t, "aaaaaaaaaaa", "A"),
		lockupItem(t, "bbbbbbbbbbb", ""), // no title -> toEntry error, but a video
		lockupItem(t, "ccccccccccc", "C"),
	}
	rawPos := 0
	c.appendPlaylistItems(pl, items, EnumOptions{}, &rawPos)
	if len(pl.Entries) != 2 {
		t.Fatalf("entries = %d, want 2 (the untitled one failed)", len(pl.Entries))
	}
	if pl.Entries[0].Index != 0 || pl.Entries[1].Index != 2 {
		t.Errorf("indices = [%d %d], want [0 2] (failed video kept position 1)", pl.Entries[0].Index, pl.Entries[1].Index)
	}
}

// pagedClient serves playlist_browse.json (entries aaa, bbb + CONT_TOKEN_1) then
// playlist_continuation.json (entry ccc, no further token). calls counts requests.
func pagedClient(t *testing.T, calls *int) *Client {
	t.Helper()
	browse := readFixture(t, "playlist_browse.json")
	cont := readFixture(t, "playlist_continuation.json")
	return newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		body, _ := readAll(r)
		if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
			return fixtureResp(http.StatusOK, cont), nil
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

func entryIDs(pl *Playlist) []string {
	ids := make([]string, len(pl.Entries))
	for i, e := range pl.Entries {
		ids[i] = e.VideoID
	}
	return ids
}

// TestEnumerateChannelIDFromByline reads the channel ID from the legacy byline's
// navigationEndpoint browseId, leaving entries without one empty.
func TestEnumerateChannelIDFromByline(t *testing.T) {
	var calls int
	c := pagedClient(t, &calls)
	pl, err := c.Enumerate(context.Background(), "PLtest", EnumOptions{MaxItems: 2})
	if err != nil {
		t.Fatal(err)
	}
	if pl.Entries[0].ChannelID != "UCaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("entry[0].ChannelID = %q, want the byline browseId", pl.Entries[0].ChannelID)
	}
	if pl.Entries[1].ChannelID != "" {
		t.Errorf("entry[1].ChannelID = %q, want empty (no byline browseId)", pl.Entries[1].ChannelID)
	}
}

// TestEnumerateSkipKeepsPagingAndIndex omits a matching entry, keeps paging, and
// leaves the surviving entries' Index at the true playlist position.
func TestEnumerateSkipKeepsPagingAndIndex(t *testing.T) {
	var calls int
	c := pagedClient(t, &calls)
	pl, err := c.Enumerate(context.Background(), "PLtest", EnumOptions{
		Skip: func(id string) bool { return id == "bbbbbbbbbbb" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := entryIDs(pl); len(got) != 2 || got[0] != "aaaaaaaaaaa" || got[1] != "ccccccccccc" {
		t.Fatalf("entries = %v, want [aaaaaaaaaaa ccccccccccc]", got)
	}
	// bbb (position 1) was skipped, so ccc keeps its true position 2.
	if pl.Entries[0].Index != 0 || pl.Entries[1].Index != 2 {
		t.Errorf("indices = [%d %d], want [0 2] (skip must not compact)", pl.Entries[0].Index, pl.Entries[1].Index)
	}
}

// TestEnumerateSkipCapCountsUnseen shows the MaxItems cap counts unseen entries:
// with the first entry skipped and MaxItems 1, the one returned entry is the
// second, not the skipped first.
func TestEnumerateSkipCapCountsUnseen(t *testing.T) {
	var calls int
	c := pagedClient(t, &calls)
	pl, err := c.Enumerate(context.Background(), "PLtest", EnumOptions{
		MaxItems: 1,
		Skip:     func(id string) bool { return id == "aaaaaaaaaaa" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := entryIDs(pl); len(got) != 1 || got[0] != "bbbbbbbbbbb" {
		t.Errorf("entries = %v, want [bbbbbbbbbbb] (cap counts unseen)", got)
	}
}

// TestEnumerateStopHaltsPaging halts at the first matching entry, excludes it and
// everything after, does not fetch the next page, and leaves Continuation empty.
func TestEnumerateStopHaltsPaging(t *testing.T) {
	var calls int
	c := pagedClient(t, &calls)
	pl, err := c.Enumerate(context.Background(), "PLtest", EnumOptions{
		Stop: func(id string) bool { return id == "bbbbbbbbbbb" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := entryIDs(pl); len(got) != 1 || got[0] != "aaaaaaaaaaa" {
		t.Fatalf("entries = %v, want [aaaaaaaaaaa] (stopped before bbb)", got)
	}
	if pl.Continuation != "" {
		t.Errorf("continuation = %q, want empty after an early stop", pl.Continuation)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no continuation fetch after halt)", calls)
	}
}

// TestEnumerateChannelStamp stamps a channel ID onto every entry.
func TestEnumerateChannelStamp(t *testing.T) {
	var calls int
	c := pagedClient(t, &calls)
	pl, err := c.Enumerate(context.Background(), "PLtest", EnumOptions{
		MaxItems:  2,
		ChannelID: "UCstampstampstampstamps0",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range pl.Entries {
		if e.ChannelID != "UCstampstampstampstamps0" {
			t.Errorf("entry[%d].ChannelID = %q, want the stamped channel ID", i, e.ChannelID)
		}
	}
}
