package waxtap

import (
	"context"
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
	homepageHits int
	vdServed     []string // visitorData observed on media requests, in order
	mediaCodes   []int    // status answered for each media request
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
			if vd == "ROT_VD_1" {
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
// already past its expire time is treated as ordinary expiry, so the identity
// is kept (no extra bootstrap) even though the download ultimately fails here.
func TestDownload_NoRotationOnExpiry403(t *testing.T) {
	w := &rotationWorld{
		media:      strings.Repeat("M", 4096),
		extraQuery: fmt.Sprintf("&expire=%d", time.Now().Add(-time.Hour).Unix()),
	}
	c := rotationClient(t, w)

	out := filepath.Join(t.TempDir(), "out.webm")
	_, err := c.Download(context.Background(), Request{
		URL:         "dummyVideo0",
		ProcessSpec: ProcessSpec{Output: ToFile(out)},
	})
	if err == nil {
		t.Fatal("download should fail when every URL 403s at expiry")
	}
	if w.homepageHits != 1 {
		t.Errorf("homepage hits = %d, want 1 (an expiry 403 must not rotate the identity)", w.homepageHits)
	}
	for _, vd := range w.vdServed {
		if vd != "ROT_VD_1" {
			t.Errorf("media request carried %q; the identity must stay ROT_VD_1 without rotation", vd)
		}
	}
}
