package waxtap

import (
	"context"
	"errors"
	"image/color"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/coverart"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/youtube"
)

const testVideoID = "dummyVideo0"

// coverRoundTrip answers each URL from a canned table and records the order in
// which they were requested.
type coverRoundTrip struct {
	// bodies maps a URL to the response it gets. A missing URL is a 404, which
	// httpx never retries, so a normal miss costs exactly one request.
	bodies map[string]coverResponse
	seen   []string
}

type coverResponse struct {
	status int
	body   []byte
}

func (rt *coverRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	url := r.URL.String()
	rt.seen = append(rt.seen, url)
	resp, ok := rt.bodies[url]
	if !ok {
		resp = coverResponse{status: http.StatusNotFound}
	}
	if resp.status == 0 {
		resp.status = http.StatusOK
	}
	return &http.Response{
		StatusCode: resp.status,
		Status:     http.StatusText(resp.status),
		Body:       io.NopCloser(strings.NewReader(string(resp.body))),
		Header:     make(http.Header),
	}, nil
}

// newCoverClient builds a Client whose HTTP transport is rt. MaxRetries of -1
// clamps httpx to exactly one attempt per URL, so a request count is exact.
func newCoverClient(t *testing.T, rt *coverRoundTrip) *Client {
	t.Helper()
	c, err := New(Options{
		HTTPClient: &http.Client{Transport: rt},
		Retry:      RetryPolicy{MaxRetries: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// coverJPEG returns encoded bytes of the given size, as the CDN would serve.
func coverJPEG(w, h int) []byte {
	return mediatest.JPEGBytes(mediatest.SolidCover(w, h, color.RGBA{R: 128, G: 128, B: 128, A: 255}), 80)
}

func ladder(id string, sizes ...[2]int) []youtube.Thumbnail {
	names := map[int]string{120: "default.jpg", 320: "mqdefault.jpg", 480: "hqdefault.jpg", 640: "sddefault.jpg", 1280: "maxresdefault.jpg"}
	out := make([]youtube.Thumbnail, 0, len(sizes))
	for _, s := range sizes {
		out = append(out, youtube.Thumbnail{
			URL:    "https://i.ytimg.com/vi/" + id + "/" + names[s[0]],
			Width:  s[0],
			Height: s[1],
		})
	}
	return out
}

func TestThumbnailCandidatesProbeWhenTheLadderFallsShort(t *testing.T) {
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480}, [2]int{480, 360})}
	got := thumbnailCandidates(v)
	want := []string{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/hq720.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/sddefault.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/hqdefault.jpg",
	}
	assertCandidateURLs(t, got, want)
	for i, c := range got {
		wantProbe := i < 2
		if c.probe != wantProbe {
			t.Errorf("candidate %d probe = %v, want %v", i, c.probe, wantProbe)
		}
		if wantProbe && c.minArea != 640*480 {
			t.Errorf("candidate %d minArea = %d, want the best listed area %d", i, c.minArea, 640*480)
		}
	}
}

func TestThumbnailCandidatesSkipProbesWhenTheLadderAlreadyReachesThem(t *testing.T) {
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{1280, 720}, [2]int{640, 480})}
	got := thumbnailCandidates(v)
	assertCandidateURLs(t, got, []string{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/sddefault.jpg",
	})
	for i, c := range got {
		if c.probe {
			t.Errorf("candidate %d should be a listed rung, not a probe", i)
		}
	}
}

func TestThumbnailCandidatesCapAttempts(t *testing.T) {
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID,
		[2]int{640, 480}, [2]int{480, 360}, [2]int{320, 180}, [2]int{120, 90})}
	// Assert which candidates survive, not just how many: the truncation keeps the
	// best rungs only because the ladder arrives largest-first, and a count alone
	// would pass just as happily with the wrong two.
	assertCandidateURLs(t, thumbnailCandidates(v), []string{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/hq720.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/sddefault.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/hqdefault.jpg",
	})
}

func TestThumbnailCandidatesDropARepeatedURL(t *testing.T) {
	// A rung that declares less than the endpoint it points at leaves the ladder
	// under the probe threshold and then names the URL the probe just built. Both
	// answer the same way, so the repeat would spend an attempt to learn nothing.
	v := &youtube.Video{ID: testVideoID, Thumbnails: []youtube.Thumbnail{
		{URL: "https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg", Width: 640, Height: 480},
		{URL: "https://i.ytimg.com/vi/" + testVideoID + "/hqdefault.jpg", Width: 480, Height: 360},
	}}
	assertCandidateURLs(t, thumbnailCandidates(v), []string{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/hq720.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/hqdefault.jpg",
	})
}

func TestThumbnailCandidatesRewriteTheLadderURL(t *testing.T) {
	// A signed sqp/rs query is issued for one size and cannot be carried onto
	// another rung, and the /vi_webp/ family has to normalize onto /vi/ so the
	// probe is decodable.
	v := &youtube.Video{ID: "notTheOne0", Thumbnails: []youtube.Thumbnail{{
		URL:    "https://i9.ytimg.com/vi_webp/realVideoID/sddefault.webp?sqp=-oaymwE&rs=AOn4CLDsigned",
		Width:  640,
		Height: 480,
	}}}
	got := thumbnailCandidates(v)
	if len(got) == 0 || got[0].url != "https://i.ytimg.com/vi/realVideoID/maxresdefault.jpg" {
		t.Fatalf("first candidate = %v, want the ladder's own ID on a plain /vi/ JPEG URL", got)
	}
}

func TestThumbnailCandidatesFallBackToTheVideoID(t *testing.T) {
	// An empty ladder is exactly where the probe is most valuable: without it
	// coverPicture reports "the video carries no thumbnail" and embeds nothing.
	v := &youtube.Video{ID: testVideoID}
	got := thumbnailCandidates(v)
	assertCandidateURLs(t, got, []string{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg",
		"https://i.ytimg.com/vi/" + testVideoID + "/hq720.jpg",
	})

	// A Video with neither a ytimg ladder nor an ID-shaped ID has nothing to
	// guess from.
	if got := thumbnailCandidates(&youtube.Video{ID: "not an id"}); len(got) != 0 {
		t.Errorf("candidates = %v, want none without an ID to build a probe from", got)
	}
}

func assertCandidateURLs(t *testing.T, got []coverCandidate, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidates = %d (%v), want %d", len(got), candidateURLs(got), len(want))
	}
	for i := range want {
		if got[i].url != want[i] {
			t.Errorf("candidate %d = %q, want %q", i, got[i].url, want[i])
		}
	}
}

func candidateURLs(cs []coverCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.url
	}
	return out
}

func TestCoverPictureWalksPastAMiss(t *testing.T) {
	big := coverJPEG(1280, 720)
	rt := &coverRoundTrip{bodies: map[string]coverResponse{
		"https://i.ytimg.com/vi/" + testVideoID + "/hq720.jpg": {body: big},
	}}
	c := newCoverClient(t, rt)
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480})}

	img, note, err := c.coverPicture(context.Background(), v, CoverArtFrame)
	if err != nil {
		t.Fatalf("coverPicture: %v", err)
	}
	if note != "" {
		t.Errorf("note = %q, want empty", note)
	}
	if len(img) != len(big) {
		t.Errorf("got %d bytes, want the hq720 body (%d)", len(img), len(big))
	}
	// The maxres 404 plus the hq720 hit: a miss is not retried.
	if len(rt.seen) != 2 {
		t.Errorf("requests = %v, want exactly 2", rt.seen)
	}
}

func TestCoverPictureDiscardsThePlaceholder(t *testing.T) {
	// i.ytimg.com answers a missing rung with 200 and a small grey placeholder,
	// so status alone is not proof the probe landed.
	listed := coverJPEG(640, 480)
	rt := &coverRoundTrip{bodies: map[string]coverResponse{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg": {body: coverJPEG(120, 90)},
		"https://i.ytimg.com/vi/" + testVideoID + "/hq720.jpg":         {body: coverJPEG(120, 90)},
		"https://i.ytimg.com/vi/" + testVideoID + "/sddefault.jpg":     {body: listed},
	}}
	c := newCoverClient(t, rt)
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480})}

	img, _, err := c.coverPicture(context.Background(), v, CoverArtFrame)
	if err != nil {
		t.Fatalf("coverPicture: %v", err)
	}
	if len(img) != len(listed) {
		t.Errorf("got %d bytes, want the ladder's own sddefault (%d)", len(img), len(listed))
	}
}

func TestCoverPictureDiscardsThePlaceholderWithNoLadder(t *testing.T) {
	// The WEB player-context path extracts a Video with no Thumbnails at all, so
	// the probes there are guesses with nothing to compare against. A guard keyed
	// only to the best listed area would be "beat zero" and would embed the grey
	// placeholder as front cover art, which is worse than the pre-probe behavior
	// of embedding nothing.
	rt := &coverRoundTrip{bodies: map[string]coverResponse{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg": {body: coverJPEG(120, 90)},
		"https://i.ytimg.com/vi/" + testVideoID + "/hq720.jpg":         {body: coverJPEG(120, 90)},
	}}
	c := newCoverClient(t, rt)

	if _, _, err := c.coverPicture(context.Background(), &youtube.Video{ID: testVideoID}, CoverArtFrame); err == nil {
		t.Fatal("a placeholder must not be accepted as cover art on an empty ladder")
	}
}

func TestCoverPictureAcceptsARealRenderWithNoLadder(t *testing.T) {
	// The floor must not reject a genuine render. Some videos carry 1278x720
	// rather than a full 1280, so it cannot key off the probe's nominal size.
	real := coverJPEG(1278, 720)
	rt := &coverRoundTrip{bodies: map[string]coverResponse{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg": {body: real},
	}}
	c := newCoverClient(t, rt)

	img, _, err := c.coverPicture(context.Background(), &youtube.Video{ID: testVideoID}, CoverArtFrame)
	if err != nil {
		t.Fatalf("coverPicture: %v", err)
	}
	if len(img) != len(real) {
		t.Errorf("got %d bytes, want the probed render (%d)", len(img), len(real))
	}
}

func TestCoverPictureSkipsANonImageBody(t *testing.T) {
	listed := coverJPEG(640, 480)
	rt := &coverRoundTrip{bodies: map[string]coverResponse{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg": {body: []byte("<html>nope</html>")},
		"https://i.ytimg.com/vi/" + testVideoID + "/sddefault.jpg":     {body: listed},
	}}
	c := newCoverClient(t, rt)
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480})}

	img, _, err := c.coverPicture(context.Background(), v, CoverArtFrame)
	if err != nil {
		t.Fatalf("coverPicture: %v", err)
	}
	if len(img) != len(listed) {
		t.Errorf("got %d bytes, want the sddefault body (%d)", len(img), len(listed))
	}
}

func TestCoverPictureReportsTheLastCause(t *testing.T) {
	rt := &coverRoundTrip{}
	c := newCoverClient(t, rt)
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480}, [2]int{480, 360})}

	_, _, err := c.coverPicture(context.Background(), v, CoverArtFrame)
	if err == nil {
		t.Fatal("want an error when every candidate misses")
	}
	if !strings.Contains(err.Error(), "tried 4") {
		t.Errorf("error = %q, want it to name the attempt count", err)
	}
	// Candidates run best-first, so the last one is the humblest and the most
	// likely to exist; naming it is more useful than naming the first guess.
	if !strings.Contains(err.Error(), "hqdefault.jpg") {
		t.Errorf("error = %q, want the last candidate as the cause", err)
	}
}

func TestCoverPictureStopsOnRateLimit(t *testing.T) {
	rt := &coverRoundTrip{bodies: map[string]coverResponse{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg": {status: http.StatusTooManyRequests},
	}}
	c := newCoverClient(t, rt)
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480})}

	_, _, err := c.coverPicture(context.Background(), v, CoverArtFrame)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("coverPicture = %v, want ErrRateLimited", err)
	}
	// The next candidate is the same host, so it would answer the same way.
	if len(rt.seen) != 1 {
		t.Errorf("requests = %v, want exactly 1", rt.seen)
	}
}

func TestCoverPictureHonorsACanceledContext(t *testing.T) {
	rt := &coverRoundTrip{}
	c := newCoverClient(t, rt)
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := c.coverPicture(ctx, v, CoverArtFrame)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("coverPicture = %v, want context.Canceled", err)
	}
	if len(rt.seen) != 0 {
		t.Errorf("requests = %v, want none", rt.seen)
	}
}

func TestCoverPictureSquares(t *testing.T) {
	art := mediatest.JPEGBytes(mediatest.ArtTrackCover(1280, 720, 720, color.RGBA{A: 255}), 90)
	rt := &coverRoundTrip{bodies: map[string]coverResponse{
		"https://i.ytimg.com/vi/" + testVideoID + "/maxresdefault.jpg": {body: art},
	}}
	c := newCoverClient(t, rt)
	v := &youtube.Video{ID: testVideoID, Thumbnails: ladder(testVideoID, [2]int{640, 480})}

	img, note, err := c.coverPicture(context.Background(), v, CoverArtSquare)
	if err != nil || note != "" {
		t.Fatalf("coverPicture = %v, %q; want no error and no note", err, note)
	}
	w, h, derr := coverart.Dimensions(img)
	if derr != nil {
		t.Fatal(derr)
	}
	if w != h {
		t.Errorf("shaped cover = %dx%d, want a square", w, h)
	}
	if w < 716 || w > 724 {
		t.Errorf("shaped cover side = %d, want within 4px of the 720px art", w)
	}
}

func TestLooksLikeVideoID(t *testing.T) {
	for _, ok := range []string{"dummyVideo0", "aBcD-_12345"} {
		if !looksLikeVideoID(ok) {
			t.Errorf("looksLikeVideoID(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "short", "twelvechars1", "has space01", "has+plus001"} {
		if looksLikeVideoID(bad) {
			t.Errorf("looksLikeVideoID(%q) = true, want false", bad)
		}
	}
}

func TestYtimgVideoID(t *testing.T) {
	cases := map[string]string{
		"https://i.ytimg.com/vi/dummyVideo0/sddefault.jpg":            "dummyVideo0",
		"https://i9.ytimg.com/vi_webp/dummyVideo0/hqdefault.webp?a=1": "dummyVideo0",
		"https://example.com/vi/dummyVideo0/sddefault.jpg":            "",
		"https://i.ytimg.com/an/dummyVideo0/sddefault.jpg":            "",
		"https://i.ytimg.com/vi/short/sddefault.jpg":                  "",
		"https://notytimg.com/vi/dummyVideo0/sddefault.jpg":           "",
		"": "",
	}
	for in, want := range cases {
		if got := ytimgVideoID(in); got != want {
			t.Errorf("ytimgVideoID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBestListedArea(t *testing.T) {
	// Area, not width: a square candidate can hold more pixels than a wider one.
	ts := []youtube.Thumbnail{{Width: 1280, Height: 720}, {Width: 1000, Height: 1000}, {Width: 0, Height: 0}}
	if got := bestListedArea(ts); got != 1000*1000 {
		t.Errorf("bestListedArea = %d, want %d", got, 1000*1000)
	}
	if got := bestListedArea(nil); got != 0 {
		t.Errorf("bestListedArea(nil) = %d, want 0", got)
	}
}
