package youtube

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2/waxerr"
)

// TestParseBrowseInitial_LockupShape covers the 2025 A/B layout: items served
// directly in the item section as lockupViewModel, one level shallower than
// the legacy playlistVideoListRenderer wrapper.
func TestParseBrowseInitial_LockupShape(t *testing.T) {
	meta, items, token, err := parseBrowseInitial(readFixture(t, "playlist_browse_lockup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.title != "Lockup Playlist" {
		t.Errorf("title = %q", meta.title)
	}
	if meta.author != "Owner Name" {
		t.Errorf("author = %q", meta.author)
	}
	if token != "LOCKUP_CONT_1" {
		t.Errorf("token = %q, want LOCKUP_CONT_1", token)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	e0, err := items[0].toEntry(0)
	if err != nil {
		t.Fatal(err)
	}
	want := PlaylistEntry{VideoID: "dummyVideo0", Title: "Song A", Author: "Artist A", Duration: 3 * time.Minute, Index: 0}
	if e0 != want {
		t.Errorf("entry0 = %+v, want %+v", e0, want)
	}

	// The second lockup has no metadata rows or badge: author and duration are
	// best-effort and stay zero rather than failing the entry.
	e1, err := items[1].toEntry(1)
	if err != nil {
		t.Fatal(err)
	}
	if e1.VideoID != "dummyVideo1" || e1.Title != "Song B" {
		t.Errorf("entry1 = %+v", e1)
	}
	if e1.Author != "" || e1.Duration != time.Hour+2*time.Minute+3*time.Second {
		t.Errorf("entry1 author/duration = %q/%v", e1.Author, e1.Duration)
	}
}

// TestParseBrowseContinuation_LockupShape covers lockup items and the
// view-model continuation marker on a continuation page.
func TestParseBrowseContinuation_LockupShape(t *testing.T) {
	items, token, err := parseBrowseContinuation(readFixture(t, "playlist_continuation_lockup.json"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "LOCKUP_CONT_2" {
		t.Errorf("token = %q, want LOCKUP_CONT_2 (continuationItemViewModel)", token)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	e, err := items[0].toEntry(0)
	if err != nil {
		t.Fatal(err)
	}
	if e.VideoID != "dummyVideo2" || e.Title != "Song C" || e.Author != "Artist C" {
		t.Errorf("entry = %+v", e)
	}
}

// TestParseBrowseInitial_UnknownShapeIsParseError is the Finding 3 case: a
// valid playlist (header parses) whose items use a renderer WaxTap has never
// seen must read as a stale parser, not as a bad or empty playlist id.
func TestParseBrowseInitial_UnknownShapeIsParseError(t *testing.T) {
	_, _, _, err := parseBrowseInitial(readFixture(t, "playlist_browse_unknown.json"))
	if !errors.Is(err, waxerr.ErrPlaylistParse) {
		t.Fatalf("err = %v, want ErrPlaylistParse", err)
	}
	if errors.Is(err, waxerr.ErrInvalidPlaylistID) {
		t.Error("an unrecognized shape must not read as an invalid playlist id")
	}
}

// TestParseBrowseInitial_EmptyPlaylist is the opposite misreport: a valid but
// empty playlist (recognized container, zero items) must not read as a stale
// parser.
func TestParseBrowseInitial_EmptyPlaylist(t *testing.T) {
	_, _, _, err := parseBrowseInitial(readFixture(t, "playlist_browse_empty.json"))
	if !errors.Is(err, waxerr.ErrPlaylistEmpty) {
		t.Fatalf("err = %v, want ErrPlaylistEmpty", err)
	}
	if errors.Is(err, waxerr.ErrPlaylistParse) {
		t.Error("an empty playlist must not read as a stale parser")
	}
	if errors.Is(err, waxerr.ErrInvalidPlaylistID) {
		t.Error("an empty playlist must not read as an invalid playlist id")
	}
}

// TestParseBrowseInitial_MessageOnlyIsEmpty covers the "no videos" notice
// shape: a messageRenderer in place of items marks an empty playlist.
func TestParseBrowseInitial_MessageOnlyIsEmpty(t *testing.T) {
	body := []byte(`{
		"metadata": {"playlistMetadataRenderer": {"title": "Empty Playlist"}},
		"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [{"tabRenderer": {"content": {"sectionListRenderer": {"contents": [
			{"itemSectionRenderer": {"contents": [
				{"messageRenderer": {"text": {"simpleText": "No videos in this playlist yet"}}}
			]}}
		]}}}}]}}
	}`)
	_, _, _, err := parseBrowseInitial(body)
	if !errors.Is(err, waxerr.ErrPlaylistEmpty) || errors.Is(err, waxerr.ErrPlaylistParse) {
		t.Fatalf("err = %v, want ErrPlaylistEmpty without ErrPlaylistParse", err)
	}
}

func TestParseBrowseInitial_ErrorAlertIsUnavailable(t *testing.T) {
	body := []byte(`{
		"alerts": [{"alertRenderer": {"type": "ERROR", "text": {"simpleText": "This playlist does not exist."}}}]
	}`)
	_, _, _, err := parseBrowseInitial(body)
	if !errors.Is(err, waxerr.ErrPlaylistUnavailable) {
		t.Fatalf("err = %v, want ErrPlaylistUnavailable", err)
	}
	if errors.Is(err, waxerr.ErrInvalidPlaylistID) || errors.Is(err, waxerr.ErrPlaylistEmpty) {
		t.Error("a private or deleted playlist must not read as an invalid or empty ID")
	}
	if _, ok := errors.AsType[*waxerr.PlaylistUnavailableError](err); !ok {
		t.Fatalf("err = %#v, want *PlaylistUnavailableError", err)
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %q, want YouTube's reason preserved", err)
	}
}

// TestParseBrowseInitial_NothingRecognizedIsParseError covers a response with
// no header and no known container at all.
func TestParseBrowseInitial_NothingRecognizedIsParseError(t *testing.T) {
	_, _, _, err := parseBrowseInitial([]byte(`{"contents": {}}`))
	if !errors.Is(err, waxerr.ErrPlaylistParse) {
		t.Fatalf("err = %v, want ErrPlaylistParse", err)
	}
}

// TestParseBrowseInitial_TitleWithoutSectionsIsParseError covers a renamed or
// relocated item container: the playlist header still parses but no item
// section is found. That is indistinguishable from layout drift, so it must
// read as a stale parser (retried, maintainer-visible), never as "playlist is
// empty": emptiness needs explicit evidence (a notice or an empty wrapper).
func TestParseBrowseInitial_TitleWithoutSectionsIsParseError(t *testing.T) {
	body := []byte(`{
		"metadata": {"playlistMetadataRenderer": {"title": "Real Playlist"}},
		"contents": {"someNewLayoutRenderer": {"items": [{"videoId": "dummyVideo0"}]}}
	}`)
	_, _, _, err := parseBrowseInitial(body)
	if !errors.Is(err, waxerr.ErrPlaylistParse) {
		t.Fatalf("err = %v, want ErrPlaylistParse", err)
	}
	if errors.Is(err, waxerr.ErrInvalidPlaylistID) {
		t.Error("a relocated container must not read as an empty/invalid playlist")
	}
}

// TestParseBrowseContinuation_UnrecognizedIsParseError covers continuation
// pages whose shape drifted: parsing to zero entries and no token must surface
// ErrPlaylistParse, not silently end the enumeration as if exhausted.
func TestParseBrowseContinuation_UnrecognizedIsParseError(t *testing.T) {
	bodies := map[string]string{
		"unknown action":  `{"onResponseReceivedActions": [{"reloadContinuationItemsCommand": {"slot": "RELOAD_CONTINUATION_SLOT_BODY"}}]}`,
		"nothing at all":  `{}`,
		"renamed wrapper": `{"continuationContents": {"someNewContinuation": {"contents": []}}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseBrowseContinuation([]byte(body))
			if !errors.Is(err, waxerr.ErrPlaylistParse) {
				t.Fatalf("err = %v, want ErrPlaylistParse", err)
			}
		})
	}
}

// TestToEntry_NonVideoLockupIsError keeps playlist/mix/podcast lockups (which
// share the lockupViewModel but whose contentId is not a video ID) out of the
// entry list; they soft-fail into Playlist.Errors instead.
func TestToEntry_NonVideoLockupIsError(t *testing.T) {
	var it playlistItem
	body := `{"lockupViewModel": {
		"contentId": "PLdummyPlaylist000000000000000000",
		"contentType": "LOCKUP_CONTENT_TYPE_PLAYLIST",
		"metadata": {"lockupMetadataViewModel": {"title": {"content": "A nested playlist"}}}
	}}`
	if err := json.Unmarshal([]byte(body), &it); err != nil {
		t.Fatal(err)
	}
	if _, err := it.toEntry(0); err == nil || !strings.Contains(err.Error(), "not a video") {
		t.Fatalf("err = %v, want a not-a-video error", err)
	}
}

// TestItemVideoIDGatesNonVideoLockup verifies itemVideoID only exposes a video
// lockup's contentId to the Skip/Stop predicates, mirroring toEntry's gate.
func TestItemVideoIDGatesNonVideoLockup(t *testing.T) {
	video := playlistItem{LockupViewModel: &lockupViewModel{ContentID: "vidvidvid00", ContentType: "LOCKUP_CONTENT_TYPE_VIDEO"}}
	if got := video.itemVideoID(); got != "vidvidvid00" {
		t.Errorf("video lockup itemVideoID = %q, want vidvidvid00", got)
	}
	playlist := playlistItem{LockupViewModel: &lockupViewModel{ContentID: "PLnotavideo0", ContentType: "LOCKUP_CONTENT_TYPE_PLAYLIST"}}
	if got := playlist.itemVideoID(); got != "" {
		t.Errorf("playlist lockup itemVideoID = %q, want empty (gated)", got)
	}
	legacy := playlistItem{LockupViewModel: &lockupViewModel{ContentID: "oldvidvid00"}} // no ContentType
	if got := legacy.itemVideoID(); got != "oldvidvid00" {
		t.Errorf("typeless lockup itemVideoID = %q, want oldvidvid00 (accepted)", got)
	}
}

// TestSplitItems_ContinuationMarkerVariants covers every observed home of the
// continuation token: the legacy direct endpoint, the live
// commandExecutorCommand nesting, and the view-model innertubeCommand form
// (plus the endpoint form on a view model, in case YouTube mixes them).
func TestSplitItems_ContinuationMarkerVariants(t *testing.T) {
	variants := []struct {
		name string
		json string
	}{
		{"renderer direct", `{"continuationItemRenderer": {"continuationEndpoint": {"continuationCommand": {"token": "TOK"}}}}`},
		{"renderer command executor", `{"continuationItemRenderer": {"continuationEndpoint": {"commandExecutorCommand": {"commands": [
			{"playlistVotingRefreshPopupCommand": {}},
			{"continuationCommand": {"token": "TOK"}}
		]}}}}`},
		{"view model innertube command", `{"continuationItemViewModel": {"continuationCommand": {"innertubeCommand": {"continuationCommand": {"token": "TOK"}}}}}`},
		{"view model endpoint", `{"continuationItemViewModel": {"continuationEndpoint": {"continuationCommand": {"token": "TOK"}}}}`},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			var it playlistItem
			if err := json.Unmarshal([]byte(v.json), &it); err != nil {
				t.Fatal(err)
			}
			entries, token := splitItems([]playlistItem{it})
			if token != "TOK" {
				t.Errorf("token = %q, want TOK", token)
			}
			if len(entries) != 0 {
				t.Errorf("entries = %d, want 0 (marker is not an entry)", len(entries))
			}
		})
	}
}

func TestParseBadgeDuration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"3:05", 3*time.Minute + 5*time.Second, true},
		{"1:02:03", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"0:59", 59 * time.Second, true},
		{" 10:00 ", 10 * time.Minute, true},
		{"LIVE", 0, false},
		{"SHORTS", 0, false},
		{"12", 0, false},
		{"1:2:3:4", 0, false},
		{"1:-2", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseBadgeDuration(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseBadgeDuration(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestShortsOrParseError(t *testing.T) {
	const (
		shortsID  = "UUSHabcdefghijklmnopqrstuv" // UUSH + 22-char body (26 chars)
		uploadsID = "UUabcdefghijklmnopqrstuv"   // UU + 22-char body (24 chars)
	)
	parseErr := func() error {
		return fmt.Errorf("no recognized playlist contents: %w", waxerr.ErrPlaylistParse)
	}

	// Parse failures for Shorts shelf IDs map to ErrShortsPlaylist and are not
	// retried.
	got := shortsOrParseError(shortsID, parseErr())
	if !errors.Is(got, waxerr.ErrShortsPlaylist) {
		t.Fatalf("shortsOrParseError(shorts, parse) = %v, want ErrShortsPlaylist", got)
	}
	if !errors.Is(got, waxerr.ErrUnsupportedInput) {
		t.Errorf("ErrShortsPlaylist must unwrap to ErrUnsupportedInput")
	}
	if retryableBrowse(got) {
		t.Errorf("a Shorts playlist error must not be retried")
	}

	// Other playlist IDs retain ErrPlaylistParse and remain retryable.
	if got := shortsOrParseError(uploadsID, parseErr()); !errors.Is(got, waxerr.ErrPlaylistParse) || errors.Is(got, waxerr.ErrShortsPlaylist) {
		t.Errorf("shortsOrParseError(uploads, parse) = %v, want unchanged ErrPlaylistParse", got)
	}

	// Non-parse errors pass through unchanged, even for a Shorts shelf ID.
	other := fmt.Errorf("boom: %w", waxerr.ErrPlaylistUnavailable)
	if got := shortsOrParseError(shortsID, other); !errors.Is(got, waxerr.ErrPlaylistUnavailable) || errors.Is(got, waxerr.ErrShortsPlaylist) {
		t.Errorf("shortsOrParseError(shorts, unavailable) = %v, want unchanged", got)
	}
}
