package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/potoken"
)

func TestCapSuspected(t *testing.T) {
	forbidden := &potoken.HTTPFailure{StatusCode: http.StatusForbidden}
	cases := []struct {
		name    string
		failure *potoken.HTTPFailure
		expires time.Time
		want    bool
	}{
		{"403 unknown expiry", forbidden, time.Time{}, true},
		{"403 far from expiry", forbidden, time.Now().Add(6 * time.Hour), true},
		{"403 near expiry", forbidden, time.Now().Add(time.Minute), false},
		{"403 past expiry", forbidden, time.Now().Add(-time.Minute), false},
		{"403 with a body (proxy block page)", &potoken.HTTPFailure{StatusCode: http.StatusForbidden, Body: "<html>blocked</html>"}, time.Now().Add(6 * time.Hour), false},
		{"410 far from expiry", &potoken.HTTPFailure{StatusCode: http.StatusGone}, time.Now().Add(6 * time.Hour), false},
		{"nil failure", nil, time.Now().Add(6 * time.Hour), false},
	}
	for _, tc := range cases {
		if got := capSuspected(tc.failure, tc.expires); got != tc.want {
			t.Errorf("%s: capSuspected = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type rotationRT func(*http.Request) (*http.Response, error)

func (f rotationRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rotResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// rotationPlayerJSON mints a player response whose direct itag-251 URL carries
// the requesting session's visitorData, so the media handler can 403 the
// flagged identity and serve the fresh one. extraQuery appends raw query text
// (e.g. an expire= in the past for the no-rotation case).
func rotationPlayerJSON(vd string, clen int, extraQuery string) string {
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
				"contentLength": "%d",
				"audioSampleRate": "48000",
				"audioChannels": 2,
				"approxDurationMs": "212000",
				"url": "https://rr1---sn-test.googlevideo.com/videoplayback?itag=251&vd=%s%s"
			}]
		},
		"videoDetails": {"videoId": "dummyVideo0", "title": "Rotation Test", "lengthSeconds": "212", "author": "T"}
	}`, clen, vd, extraQuery)
}

var playerBodyVD = regexp.MustCompile(`"visitorData"\s*:\s*"([^"]+)"`)

// rotationWorld fakes homepage, /player, and googlevideo. Media requests for
// flaggedVD answer empty-body 403 (the delivery cap); any other visitorData is
// served in full.
type rotationWorld struct {
	mu           sync.Mutex
	media        string
	extraQuery   string
	flaggedVD    string // visitorData googlevideo caps; "*" caps every identity; defaults to the first
	homepageHits int
	vdServed     []string // visitorData observed on media requests, in order
	mediaCodes   []int    // status answered for each media request
}

func (w *rotationWorld) flagged() string {
	if w.flaggedVD != "" {
		return w.flaggedVD
	}
	return "ROT_VD_1"
}

// capped reports whether googlevideo rejects this identity. "*" caps all of them,
// for the case where no amount of rotating helps.
func (w *rotationWorld) capped(vd string) bool {
	return w.flaggedVD == "*" || vd == w.flagged()
}

func (w *rotationWorld) roundTrip(t *testing.T) rotationRT {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case r.URL.Path == "/" && strings.Contains(r.URL.Host, "youtube.com"):
			w.homepageHits++
			vd := fmt.Sprintf("ROT_VD_%d", w.homepageHits)
			resp := rotResp(http.StatusOK, `<html><script>ytcfg.set({"VISITOR_DATA":"`+vd+`"});</script></html>`)
			resp.Header.Add("Set-Cookie", "VISITOR_INFO1_LIVE=vi-"+vd+"; Domain=.youtube.com; Path=/; Max-Age=31536000")
			return resp, nil
		case strings.HasSuffix(r.URL.Path, "/player"):
			body, _ := io.ReadAll(r.Body)
			m := playerBodyVD.FindSubmatch(body)
			if m == nil {
				t.Errorf("player request without visitorData:\n%s", body)
				return rotResp(http.StatusBadRequest, ""), nil
			}
			return rotResp(http.StatusOK, rotationPlayerJSON(string(m[1]), len(w.media), w.extraQuery)), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			vd := r.URL.Query().Get("vd")
			w.vdServed = append(w.vdServed, vd)
			if w.capped(vd) {
				w.mediaCodes = append(w.mediaCodes, http.StatusForbidden)
				return rotResp(http.StatusForbidden, ""), nil
			}
			w.mediaCodes = append(w.mediaCodes, http.StatusOK)
			return rotResp(http.StatusOK, w.media), nil
		}
		return rotResp(http.StatusNotFound, ""), nil
	}
}

func rotationClient(t *testing.T, w *rotationWorld) *Client {
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

// TestDownload_RotatesGuestSessionOnEarly403 is the delivery-cap escape end to
// end: the first identity's URL 403s at first byte while nowhere near expiry,
// the first refresh discards the guest identity instead of re-signing it, and
// the download completes under the fresh identity with a session-rotated
// warning.
func TestDownload_RotatesGuestSessionOnEarly403(t *testing.T) {
	w := &rotationWorld{media: strings.Repeat("M", 4096)}
	c := rotationClient(t, w)

	out := filepath.Join(t.TempDir(), "out.webm")
	res, err := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		ProcessSpec: ProcessSpec{Output: ToFile(out)},
	})
	if err != nil {
		t.Fatalf("download should recover by rotating the guest session: %v", err)
	}
	if res.OutputBytes != 4096 {
		t.Errorf("OutputBytes = %d, want 4096", res.OutputBytes)
	}

	if w.homepageHits != 2 {
		t.Errorf("homepage hits = %d, want 2 (one bootstrap per identity)", w.homepageHits)
	}
	var sawFlagged403, sawFreshOK bool
	for i, vd := range w.vdServed {
		if vd == "ROT_VD_1" && w.mediaCodes[i] == http.StatusForbidden {
			sawFlagged403 = true
		}
		if vd == "ROT_VD_2" && w.mediaCodes[i] == http.StatusOK {
			sawFreshOK = true
		}
	}
	if !sawFlagged403 || !sawFreshOK {
		t.Errorf("media sequence = %v %v; want a 403 for ROT_VD_1 then a 200 for ROT_VD_2", w.vdServed, w.mediaCodes)
	}

	var rotated, reResolved bool
	for _, warn := range res.Warnings {
		switch warn.Code {
		case WarnSessionRotated:
			rotated = true
		case WarnURLReResolved:
			reResolved = true
		}
	}
	if !rotated {
		t.Errorf("Warnings = %v, want WarnSessionRotated", res.Warnings)
	}
	if reResolved {
		t.Errorf("Warnings = %v; the rotated refresh must not also claim a plain re-resolve", res.Warnings)
	}
}

// TestDownload_NoRotationOnExpiry403 pins the guard: a 403 on a URL that is
// already past its expire time is treated as ordinary expiry, so every
// mid-download refresh stays on the same identity and re-resolves under it.
//
// The whole-chain retry is a separate mechanism, gated on the chain being
// exhausted rather than on one 403's classification, and it is what eventually
// delivers here. That the two do not collapse into one is the point: rotating on
// the first expiry 403 would pay a homepage bootstrap on every genuinely expired
// URL.
func TestDownload_NoRotationOnExpiry403(t *testing.T) {
	w := &rotationWorld{
		media:      strings.Repeat("M", 4096),
		extraQuery: fmt.Sprintf("&expire=%d", time.Now().Add(-time.Hour).Unix()),
	}
	c := rotationClient(t, w)

	out := filepath.Join(t.TempDir(), "out.webm")
	res, err := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		ProcessSpec: ProcessSpec{Output: ToFile(out)},
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	for _, warn := range res.Warnings {
		if warn.Code == WarnSessionRotated {
			t.Errorf("Warnings = %v; an expiry 403 must not rotate the identity mid-download", res.Warnings)
		}
	}
	// The refreshes stayed on the flagged identity and kept 403ing; the retry then
	// bootstrapped a second one, which delivered.
	onFlagged := 0
	for _, vd := range w.vdServed {
		if vd == w.flagged() {
			onFlagged++
		}
	}
	if onFlagged < 2 {
		t.Errorf("media requests on %s = %d, want the mid-download refreshes to have stayed on it (%v)", w.flagged(), onFlagged, w.vdServed)
	}
	if w.homepageHits != 2 {
		t.Errorf("homepage hits = %d, want 2: one bootstrap per chain pass, none from an expiry 403", w.homepageHits)
	}
}

// rotationSessionProvider is a WaxSeal-style session sidecar: it hands out
// numbered guest identities and retires the one a consumer reports.
type rotationSessionProvider struct {
	mu      sync.Mutex
	served  int
	reports []potoken.SessionInvalidation
}

func (p *rotationSessionProvider) ProvideSession(context.Context) (potoken.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.served++
	return potoken.Session{
		VisitorData: fmt.Sprintf("ADOPT_VD_%d", p.served),
		Generation:  uint64(p.served),
		Cookies:     []*http.Cookie{{Name: "VISITOR_INFO1_LIVE", Value: fmt.Sprintf("vi-%d", p.served), Domain: ".youtube.com", Path: "/"}},
	}, nil
}

func (p *rotationSessionProvider) InvalidateSession(_ context.Context, inv potoken.SessionInvalidation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reports = append(p.reports, inv)
	return nil
}

// TestDownload_RotatesAdoptedSessionOnEarly403 is the delivery-cap escape for a
// session-mediated download: the adopted identity's URL 403s while nowhere near
// expiry, the refresh reports it to the provider instead of re-signing it, and
// the download completes under the replacement the provider hands out.
func TestDownload_RotatesAdoptedSessionOnEarly403(t *testing.T) {
	w := &rotationWorld{media: strings.Repeat("M", 4096), flaggedVD: "ADOPT_VD_1"}
	p := &rotationSessionProvider{}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Options{
		HTTPClient:       &http.Client{Jar: jar, Transport: w.roundTrip(t)},
		Client:           "android_vr",
		DisableDiskCache: true,
		SessionProvider:  p,
		Retry:            RetryPolicy{MaxRetries: 1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "out.webm")
	res, err := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		ProcessSpec: ProcessSpec{Output: ToFile(out)},
	})
	if err != nil {
		t.Fatalf("download should recover by rotating the adopted session: %v", err)
	}
	if res.OutputBytes != 4096 {
		t.Errorf("OutputBytes = %d, want 4096", res.OutputBytes)
	}

	if w.homepageHits != 0 {
		t.Errorf("homepage hits = %d, want 0 (an adopted session must never bootstrap)", w.homepageHits)
	}
	if p.served != 2 {
		t.Errorf("sessions served = %d, want 2 (one per identity)", p.served)
	}
	if len(p.reports) != 1 {
		t.Fatalf("invalidations = %d, want 1", len(p.reports))
	}
	if got := p.reports[0]; got.Generation != 1 || got.VideoID != "dummyVideo0" {
		t.Errorf("invalidation = %+v, want generation 1 for dummyVideo0", got)
	}

	var sawFlagged403, sawFreshOK bool
	for i, vd := range w.vdServed {
		if vd == "ADOPT_VD_1" && w.mediaCodes[i] == http.StatusForbidden {
			sawFlagged403 = true
		}
		if vd == "ADOPT_VD_2" && w.mediaCodes[i] == http.StatusOK {
			sawFreshOK = true
		}
	}
	if !sawFlagged403 || !sawFreshOK {
		t.Errorf("media sequence = %v %v; want a 403 for ADOPT_VD_1 then a 200 for ADOPT_VD_2", w.vdServed, w.mediaCodes)
	}

	var rotated bool
	for _, warn := range res.Warnings {
		if warn.Code == WarnSessionRotated {
			rotated = true
		}
	}
	if !rotated {
		t.Errorf("Warnings = %v, want WarnSessionRotated", res.Warnings)
	}
}

// TestDownload_ChainRetryIsBounded: when every identity is capped, the chain
// retries once on a fresh session and then stops. Unbounded retrying would turn a
// video that is simply not deliverable into a loop, and each pass costs a
// bootstrap, a re-extract, and a re-resolve per client.
func TestDownload_ChainRetryIsBounded(t *testing.T) {
	w := &rotationWorld{media: strings.Repeat("M", 4096), flaggedVD: "*"}
	c := rotationClient(t, w)

	retries := 0
	out := filepath.Join(t.TempDir(), "out.webm")
	_, err := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		ProcessSpec: ProcessSpec{Output: ToFile(out), Events: countChainRetries(&retries)},
	})
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("Download = %v, want ErrIncompleteStream when no identity can deliver", err)
	}
	if retries != maxChainPasses-1 {
		t.Errorf("chain retries = %d, want %d and no more", retries, maxChainPasses-1)
	}
	// The terminal message names each attempt and how it ended, which is what the
	// bare sentence could not.
	ide, ok := errors.AsType[*IncompleteDeliveryError](err)
	if !ok {
		t.Fatalf("err = %T, want *IncompleteDeliveryError", err)
	}
	if len(ide.Attempts) < 2 {
		t.Errorf("Attempts = %v, want one entry per attempt across both passes", ide.Attempts)
	}
	for _, a := range ide.Attempts {
		if strings.Contains(a, "googlevideo.com/videoplayback?") {
			t.Errorf("attempt line carries a signed URL: %q", a)
		}
	}
}

// TestDownload_NoChainRetryUnderNoFallback: --no-fallback means one client, one
// try. The retry is a second pass over the chain, so it is fallback by any
// reading.
func TestDownload_NoChainRetryUnderNoFallback(t *testing.T) {
	w := &rotationWorld{media: strings.Repeat("M", 4096), flaggedVD: "*"}
	c := rotationClient(t, w)

	retries := 0
	out := filepath.Join(t.TempDir(), "out.webm")
	_, err := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		NoFallback:  true,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Events: countChainRetries(&retries)},
	})
	if err == nil {
		t.Fatal("download should fail when no identity can deliver")
	}
	if retries != 0 {
		t.Errorf("chain retries = %d, want 0: --no-fallback takes no second pass", retries)
	}
}

// countChainRetries counts the whole-chain retry warnings a run emits. The retry
// fires on a failed download, which returns no Result to read Warnings off.
func countChainRetries(n *int) func(Event) {
	return func(ev Event) {
		if ev.Stage == StageWarning && ev.Warning != nil &&
			strings.Contains(ev.Warning.Detail, "retrying the chain once on a fresh session") {
			*n++
		}
	}
}
