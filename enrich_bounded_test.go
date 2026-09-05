package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/waxerr"
)

var playerBodyID = regexp.MustCompile(`"videoId"\s*:\s*"([^"]+)"`)

// The three-entry playlist enrichWorld serves: two legacy-renderer entries and
// a continuation marker on the first page, one more entry on the second. The
// bodies are the minimum the browse parsers accept, owned here so a reshaped
// youtube fixture cannot silently change what these tests assert.
const (
	enrichBrowseJSON = `{
		"metadata": {"playlistMetadataRenderer": {"title": "Enrich Test"}},
		"header": {"playlistHeaderRenderer": {"ownerText": {"runs": [{"text": "Owner"}]}}},
		"contents": {"twoColumnBrowseResultsRenderer": {"tabs": [{"tabRenderer": {"content": {"sectionListRenderer": {"contents": [{"itemSectionRenderer": {"contents": [{"playlistVideoListRenderer": {"contents": [
			{"playlistVideoRenderer": {"videoId": "aaaaaaaaaaa", "title": {"runs": [{"text": "Song A"}]}, "shortBylineText": {"runs": [{"text": "Artist A"}]}, "lengthSeconds": "180"}},
			{"playlistVideoRenderer": {"videoId": "bbbbbbbbbbb", "title": {"runs": [{"text": "Song B"}]}, "shortBylineText": {"runs": [{"text": "Artist B"}]}, "lengthSeconds": "240"}},
			{"continuationItemRenderer": {"continuationEndpoint": {"continuationCommand": {"token": "ENRICH_CONT_1"}}}}
		]}}]}}]}}}}]}}
	}`
	enrichContinuationJSON = `{"onResponseReceivedActions": [{"appendContinuationItemsAction": {"continuationItems": [
		{"playlistVideoRenderer": {"videoId": "ccccccccccc", "title": {"runs": [{"text": "Song C"}]}, "shortBylineText": {"runs": [{"text": "Artist C"}]}, "lengthSeconds": "300"}}
	]}}]}`
)

// enrichWatchPage carries what the full-metadata pass reads, a WEB microformat
// with the publish date and listing state, and parses as a player response so
// it can also stand in for the watch-page extraction fallback.
const enrichWatchPage = `<html><script>var ytInitialPlayerResponse = {"playabilityStatus":{"status":"OK"},"streamingData":{"adaptiveFormats":[{"itag":251,"mimeType":"audio/webm; codecs=\"opus\"","bitrate":160000,"audioSampleRate":"48000","audioChannels":2,"approxDurationMs":"212000","url":"https://rr1---sn-test.googlevideo.com/videoplayback?itag=251"}]},"videoDetails":{"videoId":"testVideo01","title":"From Watch Page","lengthSeconds":"212"},"microformat":{"playerMicroformatRenderer":{"publishDate":"2021-05-20","isUnlisted":false}}};</script></html>`

// The throttle's measured disguise and the verdict a removed video answers.
const (
	enrichThrottleJSON = `{"playabilityStatus": {"status": "UNPLAYABLE", "reason": "Video unavailable"}}`
	enrichRemovedJSON  = `{"playabilityStatus": {"status": "ERROR", "reason": "Video unavailable"}}`
)

func enrichOKPlayerJSON(id string) string {
	return fmt.Sprintf(`{
		"responseContext": {},
		"playabilityStatus": {"status": "OK"},
		"streamingData": {
			"expiresInSeconds": "21540",
			"adaptiveFormats": [{
				"itag": 251,
				"mimeType": "audio/webm; codecs=\"opus\"",
				"bitrate": 160000,
				"averageBitrate": 130000,
				"contentLength": "3500000",
				"audioSampleRate": "48000",
				"audioChannels": 2,
				"approxDurationMs": "212000",
				"url": "https://rr1---sn-test.googlevideo.com/videoplayback?itag=251&id=%s"
			}]
		},
		"videoDetails": {
			"videoId": %q,
			"title": "Enriched %s",
			"lengthSeconds": "212",
			"channelId": "UCaaaaaaaaaaaaaaaaaaaaaa",
			"author": "Enriched Author",
			"shortDescription": "About %s",
			"thumbnail": {"thumbnails": [{"url": "https://i.ytimg.com/vi/%s/hqdefault.jpg", "width": 480, "height": 360}]}
		}
	}`, id, id, id, id, id)
}

// enrichWorld fakes the homepage, the playlist browse, /player, and the watch
// page. It plays the metadata throttle as measured: each identity answers
// window player requests and refuses every later one with the throttle's
// UNPLAYABLE "Video unavailable" shape, and a rotation (a fresh homepage
// bootstrap) resets that budget.
type enrichWorld struct {
	mu             sync.Mutex
	window         int               // player answers per identity before the throttle; 0 = unlimited
	verdicts       map[string]string // videoID -> player JSON that overrides the OK answer
	serveWatchPage bool              // answer /watch with enrichWatchPage rather than a 404

	homepageHits int
	askedByVD    map[string]int
	playerAsked  []string // videoIDs asked about, in arrival order
	watchAsked   []string // videoIDs whose watch page was fetched
}

func (w *enrichWorld) roundTrip(t *testing.T) rotationRT {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case r.URL.Path == "/" && strings.Contains(r.URL.Host, "youtube.com"):
			w.homepageHits++
			return guestHomepage(fmt.Sprintf("ENR_VD_%d", w.homepageHits)), nil
		case strings.HasSuffix(r.URL.Path, "/browse"):
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "ENRICH_CONT_1") {
				return rotResp(http.StatusOK, enrichContinuationJSON), nil
			}
			return rotResp(http.StatusOK, enrichBrowseJSON), nil
		case strings.HasSuffix(r.URL.Path, "/player"):
			body, _ := io.ReadAll(r.Body)
			idm := playerBodyID.FindSubmatch(body)
			vdm := playerBodyVD.FindSubmatch(body)
			if idm == nil || vdm == nil {
				t.Errorf("player request without videoId or visitorData:\n%s", body)
				return rotResp(http.StatusBadRequest, ""), nil
			}
			id, vd := string(idm[1]), string(vdm[1])
			w.playerAsked = append(w.playerAsked, id)
			if w.askedByVD == nil {
				w.askedByVD = map[string]int{}
			}
			w.askedByVD[vd]++
			if w.window > 0 && w.askedByVD[vd] > w.window {
				return rotResp(http.StatusOK, enrichThrottleJSON), nil
			}
			if verdict, ok := w.verdicts[id]; ok {
				return rotResp(http.StatusOK, verdict), nil
			}
			return rotResp(http.StatusOK, enrichOKPlayerJSON(id)), nil
		case r.URL.Path == "/watch":
			w.watchAsked = append(w.watchAsked, r.URL.Query().Get("v"))
			if w.serveWatchPage {
				return rotResp(http.StatusOK, enrichWatchPage), nil
			}
			return rotResp(http.StatusNotFound, ""), nil
		}
		return rotResp(http.StatusNotFound, ""), nil
	}
}

func enrichClient(t *testing.T, w *enrichWorld) *Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{
		HTTPClient:       &http.Client{Jar: jar, Transport: w.roundTrip(t)},
		Client:           "android_vr",
		DisableDiskCache: true,
		Retry: RetryPolicy{
			MaxRetries:  1,
			BaseBackoff: time.Millisecond,
			MaxBackoff:  2 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const enrichPlaylistURL = "https://www.youtube.com/playlist?list=PLxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

func enrichedIDs(pl *Playlist) []string {
	var ids []string
	for _, e := range pl.Entries {
		if e.Video != nil {
			ids = append(ids, e.VideoID)
		}
	}
	return ids
}

// The ask itself: a caller's enrichment budget runs inside the loop that knows
// how to rotate. The identity throttles after one answer, the budget covers two
// entries, and both come back enriched under a rotated identity while the third
// entry, outside the budget, is left as listed. Calling Info per entry outside
// enumeration would have delivered one and mistaken the second for a removed
// video.
func TestEnumerateEnrich_BudgetRunsInsideRotation(t *testing.T) {
	w := &enrichWorld{window: 1}
	c := enrichClient(t, w)

	pl, err := c.Enumerate(context.Background(), enrichPlaylistURL, EnumerateOptions{Enrich: true, MaxEnrich: 2})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(pl.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(pl.Entries))
	}
	if got := enrichedIDs(pl); !slices.Equal(got, []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}) {
		t.Errorf("enriched = %v, want exactly the two entries inside the budget", got)
	}
	if len(pl.Errors) != 0 {
		t.Errorf("Errors = %v, want none: the rotation recovers the throttled entry", pl.Errors)
	}
	if w.homepageHits != 2 {
		t.Errorf("homepage hits = %d, want 2 (one bootstrap per identity)", w.homepageHits)
	}
	if len(w.playerAsked) != 3 {
		t.Errorf("player calls = %v, want 3: two in the first pass and one re-ask, never the third entry", w.playerAsked)
	}
	if slices.Contains(w.playerAsked, "ccccccccccc") {
		t.Errorf("player calls = %v; the entry past the budget must not be asked about", w.playerAsked)
	}
}

// MaxEnrich caps the listing from the front: the capped entries carry the
// fetched Video and refreshed listing fields, the rest keep what the listing
// said, and progress counts only the entries enrichment attempted.
func TestEnumerateEnrich_MaxEnrichCapsFromTheFront(t *testing.T) {
	w := &enrichWorld{}
	c := enrichClient(t, w)

	var mu sync.Mutex
	var progress [][2]int
	pl, err := c.Enumerate(context.Background(), enrichPlaylistURL, EnumerateOptions{
		Enrich:    true,
		MaxEnrich: 2,
		OnEnrichProgress: func(done, total int) {
			mu.Lock()
			progress = append(progress, [2]int{done, total})
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(pl.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", pl.Errors)
	}
	for i, want := range []string{"Enriched aaaaaaaaaaa", "Enriched bbbbbbbbbbb"} {
		e := pl.Entries[i]
		if e.Video == nil {
			t.Fatalf("entry %d: Video = nil, want the fetched metadata", i)
		}
		if e.Title != want || e.Video.Title != want {
			t.Errorf("entry %d: Title = %q, Video.Title = %q, want %q on both", i, e.Title, e.Video.Title, want)
		}
		if e.Video.Description != "About "+e.VideoID || len(e.Video.Thumbnails) != 1 {
			t.Errorf("entry %d: Video = %+v, want the description and thumbnail Info returns", i, e.Video)
		}
		if e.Author != "Enriched Author" || e.Duration != 212*time.Second {
			t.Errorf("entry %d: Author = %q, Duration = %v, want the listing fields refreshed", i, e.Author, e.Duration)
		}
	}
	third := pl.Entries[2]
	if third.Video != nil || third.Title != "Song C" || third.Duration != 300*time.Second {
		t.Errorf("entry 2 = %+v, want it left exactly as listed", third)
	}
	if got := slices.Sorted(slices.Values(w.playerAsked)); !slices.Equal(got, []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}) {
		t.Errorf("player calls = %v, want one per capped entry", w.playerAsked)
	}
	if n := len(progress); n != 2 || progress[n-1] != [2]int{2, 2} {
		t.Errorf("progress = %v, want two calls ending at (2, 2): total is the entries attempted, not listed", progress)
	}
}

// A failed enrichment names its entry. The ERROR verdict a removed video answers
// is reported as an EnrichError carrying the entry's ID and position, unwrapping
// to the availability sentinel, and it costs no rotation.
func TestEnumerateEnrich_FailuresNameTheirEntry(t *testing.T) {
	w := &enrichWorld{verdicts: map[string]string{"bbbbbbbbbbb": enrichRemovedJSON}}
	c := enrichClient(t, w)

	pl, err := c.Enumerate(context.Background(), enrichPlaylistURL, EnumerateOptions{Enrich: true})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if got := enrichedIDs(pl); !slices.Equal(got, []string{"aaaaaaaaaaa", "ccccccccccc"}) {
		t.Errorf("enriched = %v, want the two entries that answered", got)
	}
	if len(pl.Errors) != 1 {
		t.Fatalf("Errors = %v, want exactly the removed entry's", pl.Errors)
	}
	ee, ok := errors.AsType[*EnrichError](pl.Errors[0])
	if !ok {
		t.Fatalf("Errors[0] = %T %v, want *EnrichError", pl.Errors[0], pl.Errors[0])
	}
	if ee.VideoID != "bbbbbbbbbbb" || ee.Index != 1 {
		t.Errorf("EnrichError = {VideoID %q, Index %d}, want the second entry", ee.VideoID, ee.Index)
	}
	if !errors.Is(pl.Errors[0], ErrVideoUnavailable) {
		t.Errorf("Errors[0] = %v, want it to unwrap to ErrVideoUnavailable", pl.Errors[0])
	}
	if errors.Is(pl.Errors[0], ErrTemporarilyUnavailable) {
		t.Errorf("Errors[0] = %v; an ERROR verdict is final, not temporary", pl.Errors[0])
	}
	if !strings.HasPrefix(pl.Errors[0].Error(), "enrich bbbbbbbbbbb: ") {
		t.Errorf("Errors[0].Error() = %q, want the listing's enrich prefix unchanged", pl.Errors[0].Error())
	}
	if w.homepageHits != 1 {
		t.Errorf("homepage hits = %d, want 1: a verdict never rotates", w.homepageHits)
	}
}

// EnrichOptions reach each entry's Info call: WithFullMetadata runs the
// watch-page pass, and only for the entries inside the budget.
func TestEnumerateEnrich_ForwardsReadOptions(t *testing.T) {
	w := &enrichWorld{serveWatchPage: true}
	c := enrichClient(t, w)

	pl, err := c.Enumerate(context.Background(), enrichPlaylistURL, EnumerateOptions{
		Enrich:        true,
		MaxEnrich:     2,
		EnrichOptions: []ReadOption{WithFullMetadata()},
	})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(pl.Errors) != 0 {
		t.Fatalf("Errors = %v, want none", pl.Errors)
	}
	for i := range 2 {
		v := pl.Entries[i].Video
		if v == nil {
			t.Fatalf("entry %d: Video = nil", i)
		}
		if v.PublishDate.Year() != 2021 || v.Availability != AvailabilityPublic {
			t.Errorf("entry %d: PublishDate = %v, Availability = %v, want the watch page's 2021 date and public listing", i, v.PublishDate, v.Availability)
		}
	}
	if got := slices.Sorted(slices.Values(w.watchAsked)); !slices.Equal(got, []string{"aaaaaaaaaaa", "bbbbbbbbbbb"}) {
		t.Errorf("watch pages fetched = %v, want exactly the capped entries", w.watchAsked)
	}
}

// The enrichment tuners are rejected as invalid config when they cannot apply,
// before any network work, so a caller learns at the first call rather than
// from an enrichment that silently never ran.
func TestEnumerateRejectsInapplicableEnrichOptions(t *testing.T) {
	c := newOfflineClient(t)
	cases := map[string]EnumerateOptions{
		"negative MaxEnrich":           {Enrich: true, MaxEnrich: -1},
		"MaxEnrich without Enrich":     {MaxEnrich: 3},
		"EnrichOptions without Enrich": {EnrichOptions: []ReadOption{WithFullMetadata()}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Enumerate(context.Background(), enrichPlaylistURL, opts)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("Enumerate(%+v) = %v, want ErrInvalidConfig", opts, err)
			}
		})
	}
}

// OnEnrichProgress predates the enrichment guards and stays ignored without
// Enrich: a caller that wires progress unconditionally and toggles Enrich per
// run must not start failing. The canceled context stops the call at its first
// network step, so what comes back is the cancellation, never invalid config.
func TestEnumerateIgnoresProgressCallbackWithoutEnrich(t *testing.T) {
	c := newOfflineClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := c.Enumerate(ctx, enrichPlaylistURL, EnumerateOptions{OnEnrichProgress: func(int, int) { called = true }})
	if errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Enumerate = %v; a progress callback without Enrich is ignored, not rejected", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Enumerate = %v, want the cancellation", err)
	}
	if called {
		t.Error("the progress callback ran without Enrich")
	}
}

// EnrichError keeps the wrapped chain reachable and the listing's message.
func TestEnrichError(t *testing.T) {
	throttle := &waxerr.PlayabilityError{Status: "UNPLAYABLE", Reason: "Video unavailable", Sentinel: ErrVideoUnavailable}
	cause := fmt.Errorf("%w: %w", ErrTemporarilyUnavailable, throttle)
	var err error = &EnrichError{VideoID: "dummyVideo0", Index: 7, Err: cause}

	if got, want := err.Error(), "enrich dummyVideo0: "+cause.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrTemporarilyUnavailable) || !errors.Is(err, ErrVideoUnavailable) {
		t.Errorf("errors.Is through EnrichError lost the sentinels: %v", err)
	}
	if pe, ok := errors.AsType[*PlayabilityError](err); !ok || pe.Status != "UNPLAYABLE" {
		t.Errorf("errors.AsType through EnrichError = %v, %v; want the PlayabilityError", pe, ok)
	}
	if ee, ok := errors.AsType[*EnrichError](fmt.Errorf("outer: %w", err)); !ok || ee.Index != 7 {
		t.Errorf("errors.AsType[*EnrichError] through a wrap = %v, %v", ee, ok)
	}
}
