package youtube

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v2/waxerr"
)

const testChannelID = "UCabcdefghijklmnopqrstuv" // UC + 22 chars

func TestExtractChannelRef(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantID  string
		wantURL string
		wantErr bool
	}{
		{"bare channel id", testChannelID, testChannelID, "", false},
		{"/channel/ url", "https://www.youtube.com/channel/" + testChannelID, testChannelID, "", false},
		{"/channel/ with tab", "https://www.youtube.com/channel/" + testChannelID + "/videos", testChannelID, "", false},
		{"handle url", "https://www.youtube.com/@SomeHandle", "", "https://www.youtube.com/@SomeHandle", false},
		{"handle url with tab", "https://www.youtube.com/@SomeHandle/streams", "", "https://www.youtube.com/@SomeHandle", false},
		{"bare handle", "@SomeHandle", "", "https://www.youtube.com/@SomeHandle", false},
		{"/c/ vanity", "https://www.youtube.com/c/SomeName", "", "https://www.youtube.com/c/SomeName", false},
		{"/user/ vanity with tab", "https://www.youtube.com/user/SomeName/featured", "", "https://www.youtube.com/user/SomeName", false},
		{"watch url", "https://www.youtube.com/watch?v=dummyVideo0", "", "", true},
		{"playlist url", "https://www.youtube.com/playlist?list=PLabcdefghij", "", "", true},
		{"bare playlist id", "PLabcdefghijklmno", "", "", true},
		{"empty", "", "", "", true},
		{"non-youtube host", "https://example.com/@SomeHandle", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ExtractChannelRef(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ExtractChannelRef(%q) = %+v, want error", tc.input, ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractChannelRef(%q): %v", tc.input, err)
			}
			if ref.ID != tc.wantID || ref.URL != tc.wantURL {
				t.Errorf("ExtractChannelRef(%q) = {ID:%q URL:%q}, want {ID:%q URL:%q}", tc.input, ref.ID, ref.URL, tc.wantID, tc.wantURL)
			}
		})
	}
}

// TestResolveUploadsPlaylistDirectID needs no network: a channel ID is a pure
// UC-to-UU transform.
func TestResolveUploadsPlaylistDirectID(t *testing.T) {
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected network call: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	uploads, channelID, err := c.ResolveUploadsPlaylist(context.Background(), ChannelRef{ID: testChannelID})
	if err != nil {
		t.Fatal(err)
	}
	if want := "UU" + testChannelID[2:]; uploads != want {
		t.Errorf("uploads = %q, want %q", uploads, want)
	}
	if channelID != testChannelID {
		t.Errorf("channelID = %q, want %q", channelID, testChannelID)
	}
}

// TestResolveUploadsPlaylistHandleInnerTube resolves a handle via the
// navigation/resolve_url endpoint.
func TestResolveUploadsPlaylistHandleInnerTube(t *testing.T) {
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/navigation/resolve_url") {
			return fixtureResp(http.StatusOK, []byte(`{"endpoint":{"browseEndpoint":{"browseId":"`+testChannelID+`"}}}`)), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	uploads, channelID, err := c.ResolveUploadsPlaylist(context.Background(), ChannelRef{URL: "https://www.youtube.com/@SomeHandle"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "UU" + testChannelID[2:]; uploads != want {
		t.Errorf("uploads = %q, want %q", uploads, want)
	}
	if channelID != testChannelID {
		t.Errorf("channelID = %q, want %q", channelID, testChannelID)
	}
}

// TestResolveUploadsPlaylistHandleScrapeFallback falls back to a channel-page
// scrape when resolve_url does not return a channel.
func TestResolveUploadsPlaylistHandleScrapeFallback(t *testing.T) {
	page := `<html><head><link rel="canonical" href="https://www.youtube.com/channel/` + testChannelID + `"></head></html>`
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/navigation/resolve_url"):
			return fixtureResp(http.StatusOK, []byte(`{"endpoint":{}}`)), nil // no browseId
		case r.URL.Path == "/@SomeHandle":
			return fixtureResp(http.StatusOK, []byte(page)), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))
	uploads, channelID, err := c.ResolveUploadsPlaylist(context.Background(), ChannelRef{URL: "https://www.youtube.com/@SomeHandle"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "UU" + testChannelID[2:]; uploads != want {
		t.Errorf("uploads = %q, want %q", uploads, want)
	}
	if channelID != testChannelID {
		t.Errorf("channelID = %q, want %q", channelID, testChannelID)
	}
}

// TestResolveChannelIDNotFound maps a channel that neither path can resolve to
// ErrVideoUnavailable (exit 3), not an extractor-maintenance signal.
func TestResolveChannelIDNotFound(t *testing.T) {
	t.Run("scrape 200 without id", func(t *testing.T) {
		c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/navigation/resolve_url"):
				return fixtureResp(http.StatusOK, []byte(`{"endpoint":{}}`)), nil
			case r.URL.Path == "/@ghost":
				return fixtureResp(http.StatusOK, []byte(`<html>no channel id</html>`)), nil
			}
			return fixtureResp(http.StatusNotFound, nil), nil
		}))
		_, _, err := c.ResolveUploadsPlaylist(context.Background(), ChannelRef{URL: "https://www.youtube.com/@ghost"})
		if !errors.Is(err, waxerr.ErrVideoUnavailable) {
			t.Errorf("err = %v, want ErrVideoUnavailable", err)
		}
	})

	t.Run("scrape 404", func(t *testing.T) {
		c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/navigation/resolve_url") {
				return fixtureResp(http.StatusOK, []byte(`{"endpoint":{}}`)), nil
			}
			return fixtureResp(http.StatusNotFound, nil), nil // channel page 404
		}))
		_, _, err := c.ResolveUploadsPlaylist(context.Background(), ChannelRef{URL: "https://www.youtube.com/@ghost"})
		if !errors.Is(err, waxerr.ErrVideoUnavailable) {
			t.Errorf("err = %v, want ErrVideoUnavailable for a 404 channel page", err)
		}
	})
}

// TestResolveChannelIDRateLimitedNoScrape checks a rate-limited resolve does not
// fire the fallback scrape and surfaces the throttle.
func TestResolveChannelIDRateLimitedNoScrape(t *testing.T) {
	var scrapeCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/navigation/resolve_url") {
			return fixtureResp(http.StatusTooManyRequests, nil), nil
		}
		scrapeCalls++
		return fixtureResp(http.StatusOK, []byte(`<html></html>`)), nil
	}))
	_, _, err := c.ResolveUploadsPlaylist(context.Background(), ChannelRef{URL: "https://www.youtube.com/@handle"})
	if !errors.Is(err, waxerr.ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	if scrapeCalls != 0 {
		t.Errorf("scrapeCalls = %d, want 0 (no fallback after a rate limit)", scrapeCalls)
	}
}

// TestResolveChannelIDCanceledNoScrape checks a canceled context stops before the
// fallback scrape.
func TestResolveChannelIDCanceledNoScrape(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var scrapeCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/navigation/resolve_url") {
			cancel() // cancel during the first request
			return nil, ctx.Err()
		}
		scrapeCalls++
		return fixtureResp(http.StatusOK, nil), nil
	}))
	_, _, err := c.ResolveUploadsPlaylist(ctx, ChannelRef{URL: "https://www.youtube.com/@handle"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if scrapeCalls != 0 {
		t.Errorf("scrapeCalls = %d, want 0 (no fallback after cancellation)", scrapeCalls)
	}
}

// TestIsHandleLength rejects handles outside YouTube's 3-30 character range.
func TestIsHandleLength(t *testing.T) {
	if isHandle("@ab") {
		t.Error("2-char handle should be rejected")
	}
	if !isHandle("@abc") {
		t.Error("3-char handle should be accepted")
	}
	if isHandle("@" + strings.Repeat("a", 31)) {
		t.Error("31-char handle should be rejected")
	}
}

func TestChannelIDFromHTML(t *testing.T) {
	cases := []struct {
		name string
		html string
	}{
		{"canonical link", `<link rel="canonical" href="https://www.youtube.com/channel/` + testChannelID + `">`},
		{"meta channelId", `<meta itemprop="channelId" content="` + testChannelID + `">`},
		{"meta identifier", `<meta itemprop="identifier" content="` + testChannelID + `">`},
		{"externalId json", `...,"externalId":"` + testChannelID + `",...`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelIDFromHTML([]byte(tc.html)); got != testChannelID {
				t.Errorf("channelIDFromHTML = %q, want %q", got, testChannelID)
			}
		})
	}
	if got := channelIDFromHTML([]byte(`<html>no id here</html>`)); got != "" {
		t.Errorf("channelIDFromHTML(no id) = %q, want empty", got)
	}
}
