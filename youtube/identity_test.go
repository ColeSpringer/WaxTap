package youtube

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/internal/clientident"
	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/youtube/internal/resolver"
)

// fastTransport wraps rt in an httpx.Client configured for short retries.
func fastTransport(rt http.RoundTripper) *httpx.Client {
	return httpx.New(httpx.Config{
		HTTPClient:   &http.Client{Transport: rt},
		MaxRetries:   1,
		MaxRetryWait: 50 * time.Millisecond,
		BaseBackoff:  time.Millisecond,
		MaxBackoff:   2 * time.Millisecond,
	})
}

// recordingProvider captures every PO-token request so a test can compare the
// identity presented across scopes.
type recordingProvider struct {
	reqs []potoken.Request
	resp potoken.Response
}

func (p *recordingProvider) ProvidePOToken(_ context.Context, req potoken.Request) (potoken.Response, error) {
	p.reqs = append(p.reqs, req)
	return p.resp, nil
}

func (p *recordingProvider) byScope(s potoken.Scope) *potoken.Request {
	for i := range p.reqs {
		if p.reqs[i].Scope == s {
			return &p.reqs[i]
		}
	}
	return nil
}

// TestExtractResolve_IdentityContract verifies that the winning WEB client uses
// the configured identity for player tokens, GVS tokens, and stream requests.
// Using the default profile chain also exercises buildDefaultProfiles.
func TestExtractResolve_IdentityContract(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	errBody := readFixture(t, "player_unavailable.json") // generic ERROR -> playability failure

	rp := &recordingProvider{resp: potoken.Response{Token: "TOK"}}
	fr := &fakeResolver{stream: resolver.Stream{URL: "https://signed/"}}

	const major = 151
	c := New(Config{
		ChromeMajor:     major,
		Resolver:        fr,
		POTokenProvider: rp,
		HTTP: fastTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if !strings.Contains(r.URL.Path, "/player") {
				t.Errorf("unexpected request: %s", r.URL)
				return fixtureResp(http.StatusNotFound, nil), nil
			}
			// Force the first three default profiles to fail so WEB wins.
			if r.Header.Get("X-Youtube-Client-Name") == "1" {
				return fixtureResp(http.StatusOK, ok), nil
			}
			return fixtureResp(http.StatusOK, errBody), nil
		})),
	})

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if ext.profile.Name != "WEB" {
		t.Fatalf("winning profile = %q, want WEB", ext.profile.Name)
	}
	if _, err := c.Resolve(context.Background(), ext, 0); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	player := rp.byScope(potoken.ScopePlayer)
	gvs := rp.byScope(potoken.ScopeGVS)
	if player == nil || gvs == nil {
		t.Fatalf("want both player and gvs token requests, got %d: %+v", len(rp.reqs), rp.reqs)
	}

	// Both token scopes must use the User-Agent for the configured major.
	wantUA := clientident.UserAgent(major)
	if player.UserAgent != wantUA {
		t.Errorf("player UA = %q, want %q", player.UserAgent, wantUA)
	}
	if gvs.UserAgent != wantUA {
		t.Errorf("gvs UA = %q, want %q", gvs.UserAgent, wantUA)
	}
	if player.ClientName != "WEB" || gvs.ClientName != "WEB" {
		t.Errorf("ClientName: player=%q gvs=%q, want WEB on both", player.ClientName, gvs.ClientName)
	}
	if player.ClientVersion != clientident.WebVersion || gvs.ClientVersion != clientident.WebVersion {
		t.Errorf("ClientVersion: player=%q gvs=%q, want %q on both", player.ClientVersion, gvs.ClientVersion, clientident.WebVersion)
	}

	// The stream request carries the same identity.
	if got := fr.gotCtx.Headers.Get("User-Agent"); got != wantUA {
		t.Errorf("stream-header UA = %q, want %q", got, wantUA)
	}

	// The player token pins visitorData, so the GVS token must use the same value
	// even when /player returns a different one.
	if player.VisitorData == "" {
		t.Error("player VisitorData should carry the value the token was minted under")
	}
	if gvs.VisitorData != player.VisitorData {
		t.Errorf("gvs VisitorData = %q, want it pinned to the player value %q", gvs.VisitorData, player.VisitorData)
	}
	if gvs.VisitorData == "CgtleGFtcGxlVmlz" {
		t.Error("gvs VisitorData adopted the /player value after the player token was minted")
	}
}

// TestExtract_WatchPageFallbackUsesChromeMajor verifies that watch-page fallback
// uses the configured Chrome major.
func TestExtract_WatchPageFallbackUsesChromeMajor(t *testing.T) {
	login := readFixture(t, "player_login_required.json")
	html := readFixture(t, "watch_page.html")
	const major = 151
	var watchUA string
	c := New(Config{
		ChromeMajor: major,
		HTTP: fastTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if resp, ok := discoveryResp(r); ok {
				return resp, nil // signature timestamp lookup uses the embed page
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/v1/player"):
				return fixtureResp(http.StatusOK, login), nil // every InnerTube client age-gated
			case strings.Contains(r.URL.Path, "/watch"):
				watchUA = r.Header.Get("User-Agent")
				return fixtureResp(http.StatusOK, html), nil
			}
			t.Errorf("unexpected request: %s", r.URL)
			return fixtureResp(http.StatusNotFound, nil), nil
		})),
	})

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	if ext.profile.Name != "WEB" {
		t.Errorf("watch-page fallback profile = %q, want WEB", ext.profile.Name)
	}
	if want := clientident.UserAgent(major); watchUA != want {
		t.Errorf("watch-page User-Agent = %q, want %q", watchUA, want)
	}
}

// TestEnumerate_FallbackProfileUsesChromeMajor verifies that playlist fallback
// uses the configured Chrome major for the initial request and continuations.
func TestEnumerate_FallbackProfileUsesChromeMajor(t *testing.T) {
	browse := readFixture(t, "playlist_browse.json")
	cont := readFixture(t, "playlist_continuation.json")
	const major = 151
	// A chain with no playlist-capable profile forces the built-in WEB fallback.
	nonPlaylist := makeProfile(ClientProfile{
		Name: "ANDROID_VR", InnerTubeName: "ANDROID_VR", InnerTubeID: 28,
		Version: "1.0", SupportsPlaylists: false,
	})
	var uas []string
	c := New(Config{
		ChromeMajor: major,
		Profiles:    []ClientProfile{nonPlaylist},
		HTTP: fastTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			uas = append(uas, r.Header.Get("User-Agent"))
			if bytes.Contains(body, []byte("CONT_TOKEN_1")) {
				return fixtureResp(http.StatusOK, cont), nil
			}
			return fixtureResp(http.StatusOK, browse), nil
		})),
	})

	pl, err := c.Enumerate(context.Background(), "PLtest", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.Entries) != 3 {
		t.Fatalf("entries = %d, want 3 (initial + continuation)", len(pl.Entries))
	}
	want := clientident.UserAgent(major)
	if len(uas) < 2 {
		t.Fatalf("browse requests = %d, want >= 2 (initial + continuation)", len(uas))
	}
	for i, ua := range uas {
		if ua != want {
			t.Errorf("browse request %d User-Agent = %q, want %q", i, ua, want)
		}
	}
}

// TestBootstrap_HomepageUsesChromeMajor verifies that guest-session bootstrap
// uses the configured Chrome major.
func TestBootstrap_HomepageUsesChromeMajor(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	const major = 151
	var homepageUA string
	jar, _ := cookiejar.New(nil)
	c := New(Config{
		ChromeMajor: major,
		Profiles:    []ClientProfile{makeProfile(profileAndroidVR)}, // needs no PO token
		HTTP: httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				homepageUA = r.Header.Get("User-Agent")
				return fixtureResp(http.StatusOK, []byte(`<script>var x = {"VISITOR_DATA":"BOOTVIS000"};</script>`)), nil
			}
			return fixtureResp(http.StatusOK, ok), nil
		})}}),
	})

	if _, err := c.Extract(context.Background(), "testVideo01"); err != nil {
		t.Fatal(err)
	}
	if homepageUA == "" {
		t.Fatal("bootstrap homepage was not fetched")
	}
	if want := clientident.UserAgent(major); homepageUA != want {
		t.Errorf("bootstrap homepage User-Agent = %q, want %q", homepageUA, want)
	}
}
