package youtube

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/waxerr"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureResp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

func newTestClient(rt http.RoundTripper) *Client {
	return New(Config{HTTP: httpx.New(httpx.Config{
		HTTPClient:   &http.Client{Transport: rt},
		MaxRetries:   1,
		MaxRetryWait: 50 * time.Millisecond,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   2 * time.Millisecond,
	})})
}

func TestExtract_FirstClientSucceeds(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var playerCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			playerCalls++
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	ext, err := c.Extract(context.Background(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Video().Title != "Test Song" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	if playerCalls != 1 {
		t.Errorf("playerCalls = %d, want 1 (first client wins)", playerCalls)
	}

	// The resolver input is kept by index because itag is not unique across
	// multi-track audio.
	if rf, ok := ext.rawFormatByIndex(0); !ok || rf.Itag != 251 || rf.URL == "" {
		t.Errorf("index 0 raw = %+v, ok=%v; want itag 251 with a direct URL", rf, ok)
	}
	if rf, ok := ext.rawFormatByIndex(1); !ok || rf.Itag != 140 || rf.SignatureCipher == "" {
		t.Errorf("index 1 raw = %+v, ok=%v; want itag 140 with a signatureCipher", rf, ok)
	}
}

func TestExtract_FallsBackAcrossClients(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	login := readFixture(t, "player_login_required.json")
	var names []string
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/player") {
			t.Errorf("unexpected request: %s", r.URL)
			return fixtureResp(http.StatusNotFound, nil), nil
		}
		name := r.Header.Get("X-Youtube-Client-Name")
		names = append(names, name)
		if name == "28" || name == "5" { // ANDROID_VR and IOS are age-gated
			return fixtureResp(http.StatusOK, login), nil
		}
		return fixtureResp(http.StatusOK, ok), nil
	}))

	ext, err := c.Extract(context.Background(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Video().Title != "Test Song" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	if want := []string{"28", "5", "56"}; !slicesEqual(names, want) {
		t.Errorf("client order = %v, want %v", names, want)
	}
}

func TestExtract_PlayabilityErrorTriesAllClients(t *testing.T) {
	un := readFixture(t, "player_unavailable.json") // status ERROR
	var playerCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			playerCalls++
		}
		return fixtureResp(http.StatusOK, un), nil
	}))

	_, err := c.Extract(context.Background(), "dQw4w9WgXcQ")
	if !errors.Is(err, waxerr.ErrVideoUnavailable) {
		t.Fatalf("err = %v, want ErrVideoUnavailable", err)
	}
	// A generic ERROR is no longer terminal: every client in the chain is tried
	// before extraction gives up.
	if want := len(DefaultProfiles()); playerCalls != want {
		t.Errorf("player calls = %d, want %d (all clients tried past ERROR)", playerCalls, want)
	}
}

func TestExtract_WatchPageFallback(t *testing.T) {
	login := readFixture(t, "player_login_required.json")
	html := readFixture(t, "watch_page.html")
	var watchCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/player"):
			return fixtureResp(http.StatusOK, login), nil // every client age-gated
		case strings.Contains(r.URL.Path, "/watch"):
			watchCalls++
			return fixtureResp(http.StatusOK, html), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	ext, err := c.Extract(context.Background(), "dQw4w9WgXcQ")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Video().Title != "From Watch Page" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	if watchCalls != 1 {
		t.Errorf("watchCalls = %d, want 1", watchCalls)
	}
}

func TestExtract_RateLimitShortCircuits(t *testing.T) {
	var calls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		resp := fixtureResp(http.StatusTooManyRequests, nil)
		resp.Header.Set("Retry-After", "3600")
		return resp, nil
	}))

	_, err := c.Extract(context.Background(), "dQw4w9WgXcQ")
	if !errors.Is(err, waxerr.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (rate limit short-circuits chain + fallback)", calls)
	}
}

func TestExtract_DefaultLocale(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var body []byte
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ = io.ReadAll(r.Body)
		return fixtureResp(http.StatusOK, ok), nil
	}))
	if _, err := c.Extract(context.Background(), "dQw4w9WgXcQ"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"hl":"en"`)) || !bytes.Contains(body, []byte(`"gl":"US"`)) {
		t.Errorf("default locale not en/US in request: %s", body)
	}
}

func TestExtract_ConfiguredLocale(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	var body []byte
	var acceptLang string
	c := New(Config{
		HL: "de",
		GL: "DE",
		HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ = io.ReadAll(r.Body)
			acceptLang = r.Header.Get("Accept-Language")
			return fixtureResp(http.StatusOK, ok), nil
		})}}),
	})
	if _, err := c.Extract(context.Background(), "dQw4w9WgXcQ"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"hl":"de"`)) || !bytes.Contains(body, []byte(`"gl":"DE"`)) {
		t.Errorf("configured locale not in request body: %s", body)
	}
	if !strings.HasPrefix(acceptLang, "de") {
		t.Errorf("Accept-Language = %q, want it to lead with de", acceptLang)
	}
}

func TestEnumerate(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	cont := readFixture(t, "playlist_continuation.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
			return fixtureResp(http.StatusOK, cont), nil
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 0)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Title != "My Playlist" {
		t.Errorf("title = %q", pl.Title)
	}
	if pl.Author != "Owner Name" {
		t.Errorf("author = %q", pl.Author)
	}
	if len(pl.Errors) != 0 {
		t.Errorf("errors = %v", pl.Errors)
	}

	wantIDs := []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc"}
	if len(pl.Entries) != len(wantIDs) {
		t.Fatalf("entries = %d, want %d", len(pl.Entries), len(wantIDs))
	}
	for i, e := range pl.Entries {
		if e.VideoID != wantIDs[i] {
			t.Errorf("entry[%d].VideoID = %q, want %q", i, e.VideoID, wantIDs[i])
		}
		if e.Index != i {
			t.Errorf("entry[%d].Index = %d, want %d", i, e.Index, i)
		}
	}
	if pl.Entries[0].Title != "Song A" || pl.Entries[0].Author != "Artist A" {
		t.Errorf("entry0 = %+v", pl.Entries[0])
	}
	if pl.Entries[2].Duration != 300*time.Second {
		t.Errorf("entry2 duration = %v, want 5m", pl.Entries[2].Duration)
	}
}

func TestEnumerate_MaxItemsAtPageBoundary(t *testing.T) {
	// maxItems equals the page's entry count: a clean page boundary, so the
	// next-page token is a valid resume point.
	browse := readFixture(t, "playlist_browse.json") // 2 entries + token
	var calls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(pl.Entries))
	}
	if pl.Continuation != "CONT_TOKEN_1" {
		t.Errorf("continuation = %q, want CONT_TOKEN_1 (page boundary is resumable)", pl.Continuation)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (stopped at limit, no continuation fetch)", calls)
	}
}

func TestEnumerate_MaxItemsMidPageNoResume(t *testing.T) {
	// maxItems falls in the middle of the first page. There is no page-granular
	// resume point for the unreturned entries.
	browse := readFixture(t, "playlist_browse.json") // 2 entries + token
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(pl.Entries))
	}
	if pl.Continuation != "" {
		t.Errorf("continuation = %q, want empty (mid-page cutoff must not skip entries)", pl.Continuation)
	}
}

func TestEnumerate_LegacyContinuationShape(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	legacy := readFixture(t, "playlist_continuation_legacy.json")
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
			return fixtureResp(http.StatusOK, legacy), nil // continuationContents shape
		}
		return fixtureResp(http.StatusOK, browse), nil
	}))

	pl, err := c.Enumerate(context.Background(), "PLtest", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 2 from the initial page + 1 from the legacy continuation page.
	wantIDs := []string{"aaaaaaaaaaa", "bbbbbbbbbbb", "ddddddddddd"}
	if len(pl.Entries) != len(wantIDs) {
		t.Fatalf("entries = %d, want %d", len(pl.Entries), len(wantIDs))
	}
	for i, e := range pl.Entries {
		if e.VideoID != wantIDs[i] {
			t.Errorf("entry[%d].VideoID = %q, want %q", i, e.VideoID, wantIDs[i])
		}
	}
}

func TestEnumerate_HonorsConfiguredProfile(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	cont := readFixture(t, "playlist_continuation.json")

	// A caller can replace the WEB client version/key. Enumerate should use the
	// configured playlist-capable profile.
	custom := makeProfile(ClientProfile{
		Name: "WEB", InnerTubeName: "WEB", InnerTubeID: 1,
		Version: "9.99", APIKey: "KEYX", SupportsPlaylists: true,
	})
	var versions []string
	c := New(Config{
		Profiles: []ClientProfile{custom},
		HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			versions = append(versions, r.Header.Get("X-Youtube-Client-Version"))
			if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
				return fixtureResp(http.StatusOK, cont), nil
			}
			return fixtureResp(http.StatusOK, browse), nil
		})}}),
	})

	pl, err := c.Enumerate(context.Background(), "PLtest", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(pl.Entries))
	}
	if len(versions) == 0 {
		t.Fatal("no browse requests recorded")
	}
	for _, v := range versions {
		if v != "9.99" {
			t.Errorf("browse used client version %q, want 9.99 (configured profile honored)", v)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
