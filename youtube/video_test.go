package youtube

import (
	"testing"
	"time"
)

// TestLiveStatusDerivation checks the upcoming -> live -> was_live -> none
// precedence toVideo derives from the player response.
func TestLiveStatusDerivation(t *testing.T) {
	mk := func(live, upcoming, liveContent bool) *playerResponse {
		pr := &playerResponse{}
		pr.VideoDetails.IsUpcoming = upcoming
		pr.VideoDetails.IsLiveContent = liveContent
		pr.Microformat.PlayerMicroformatRenderer.LiveBroadcastDetails.IsLiveNow = live
		return pr
	}
	cases := []struct {
		name                        string
		live, upcoming, liveContent bool
		want                        LiveStatus
	}{
		{"normal", false, false, false, LiveNone},
		{"was live VOD", false, false, true, LiveWasLive},
		{"live now", true, false, true, LiveNow},
		{"upcoming wins over live", true, true, true, LiveUpcoming},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mk(tc.live, tc.upcoming, tc.liveContent).liveStatus(); got != tc.want {
				t.Errorf("liveStatus = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestToVideoSetsURLAndSortsThumbnails checks the canonical watch URL and the
// largest-first thumbnail order (YouTube returns them ascending).
func TestToVideoSetsURLAndSortsThumbnails(t *testing.T) {
	pr := &playerResponse{}
	pr.VideoDetails.Title = "T"
	pr.VideoDetails.Thumbnail.Thumbnails = []rawThumbnail{
		{URL: "small", Width: 120, Height: 90},
		{URL: "large", Width: 1280, Height: 720},
		{URL: "medium", Width: 480, Height: 360},
	}
	pr.StreamingData.AdaptiveFormats = []rawFormat{{Itag: 251, MimeType: `audio/webm; codecs="opus"`}}

	v, _, err := pr.toVideo("dummyVideo0")
	if err != nil {
		t.Fatal(err)
	}
	if v.URL != "https://www.youtube.com/watch?v=dummyVideo0" {
		t.Errorf("URL = %q", v.URL)
	}
	wantOrder := []string{"large", "medium", "small"}
	if len(v.Thumbnails) != 3 {
		t.Fatalf("got %d thumbnails", len(v.Thumbnails))
	}
	for i, w := range wantOrder {
		if v.Thumbnails[i].URL != w {
			t.Errorf("thumbnail %d = %q, want %q", i, v.Thumbnails[i].URL, w)
		}
	}
}

// TestThumbnailTieBreakDeterministic checks equal-size thumbnails order by URL so
// tests do not flake.
func TestThumbnailTieBreakDeterministic(t *testing.T) {
	ts := []Thumbnail{
		{URL: "z", Width: 100, Height: 100},
		{URL: "a", Width: 100, Height: 100},
		{URL: "m", Width: 100, Height: 100},
	}
	sortThumbnailsLargestFirst(ts)
	if ts[0].URL != "a" || ts[1].URL != "m" || ts[2].URL != "z" {
		t.Errorf("tie-break order = %q,%q,%q, want a,m,z", ts[0].URL, ts[1].URL, ts[2].URL)
	}
}

// TestFillWatchPageEnrichment fills chapters and availability from an
// already-fetched watch page and its player response.
func TestFillWatchPageEnrichment(t *testing.T) {
	body := readFixture(t, "watch_page.html")
	pr, err := parseWatchPage(body)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := pr.toVideo("testVideo01")
	if err != nil {
		t.Fatal(err)
	}
	fillWatchPageEnrichment(v, body, pr)
	if len(v.Chapters) != 3 {
		t.Errorf("chapters = %d, want 3", len(v.Chapters))
	}
	if v.Availability != AvailabilityPublic {
		t.Errorf("availability = %v, want public", v.Availability)
	}
	if v.PublishDate.Format("2006-01-02") != "2021-05-20" {
		t.Errorf("publishDate = %s, want 2021-05-20", v.PublishDate.Format("2006-01-02"))
	}
}

// TestFillWatchPageEnrichmentUnlisted sets AvailabilityUnlisted from the
// microformat isUnlisted flag.
func TestFillWatchPageEnrichmentUnlisted(t *testing.T) {
	pr := &playerResponse{}
	pr.Microformat.PlayerMicroformatRenderer.IsUnlisted = true
	v := &Video{Duration: time.Minute}
	fillWatchPageEnrichment(v, []byte("<html></html>"), pr)
	if v.Availability != AvailabilityUnlisted {
		t.Errorf("availability = %v, want unlisted", v.Availability)
	}
}
