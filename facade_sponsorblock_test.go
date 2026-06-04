package waxtap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/colespringer/waxtap/sponsorblock"
)

const sbVideoID = "abcdef12345"

// sbServer returns a SponsorBlock test server responding with the given status
// and body to any hash-prefix request, plus a Client pointed at it.
func sbServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{SponsorBlock: SponsorBlockOptions{BaseURL: srv.URL}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestCollectRangesNilCut(t *testing.T) {
	c := newOfflineClient(t)
	all, sb, err := c.collectRanges(context.Background(), nil, sbVideoID, newEmitter(nil, ""))
	if err != nil || all != nil || sb != nil {
		t.Errorf("nil cut = (%v, %v, %v), want all nil", all, sb, err)
	}
}

func TestCollectRangesExplicitOnly(t *testing.T) {
	c := newOfflineClient(t)
	cs := &CutSpec{Ranges: []TimeRange{{Start: time.Second, End: 2 * time.Second}}}
	all, sb, err := c.collectRanges(context.Background(), cs, sbVideoID, newEmitter(nil, ""))
	if err != nil {
		t.Fatalf("collectRanges: %v", err)
	}
	if len(all) != 1 || sb != nil {
		t.Errorf("explicit-only = (%v, %v), want 1 explicit and no SB", all, sb)
	}
}

func TestCollectRangesWithSponsorBlock(t *testing.T) {
	body := `[{"videoID":"` + sbVideoID + `","segments":[
	  {"category":"music_offtopic","actionType":"skip","segment":[0.0,5.5],"UUID":"a","locked":1,"votes":3}
	]}]`
	c := sbServer(t, http.StatusOK, body)
	cs := &CutSpec{
		Ranges:       []TimeRange{{Start: 100 * time.Second, End: 110 * time.Second}},
		SponsorBlock: []sponsorblock.Category{sponsorblock.CategoryMusicOffTopic},
	}
	all, sb, err := c.collectRanges(context.Background(), cs, sbVideoID, newEmitter(nil, ""))
	if err != nil {
		t.Fatalf("collectRanges: %v", err)
	}
	if len(sb) != 1 {
		t.Fatalf("SB ranges = %d, want 1: %+v", len(sb), sb)
	}
	if len(all) != 2 {
		t.Errorf("combined ranges = %d, want 2 (explicit + SB)", len(all))
	}
}

func TestCollectRangesSponsorBlockEmpty(t *testing.T) {
	c := sbServer(t, http.StatusNotFound, "Not Found")
	var warns []WarningCode
	em := newEmitter(func(e Event) {
		if e.Warning != nil {
			warns = append(warns, e.Warning.Code)
		}
	}, "")

	cs := &CutSpec{SponsorBlock: []sponsorblock.Category{sponsorblock.CategoryMusicOffTopic}}
	all, sb, err := c.collectRanges(context.Background(), cs, sbVideoID, em)
	if err != nil {
		t.Fatalf("collectRanges: %v", err)
	}
	if sb != nil || all != nil {
		t.Errorf("empty SB = (%v, %v), want nil", all, sb)
	}
	if len(warns) != 1 || warns[0] != WarnSponsorBlockEmpty {
		t.Errorf("warnings = %v, want [WarnSponsorBlockEmpty]", warns)
	}
}

func TestCollectRangesProceedUncut(t *testing.T) {
	c := sbServer(t, http.StatusInternalServerError, "boom")
	var warns []WarningCode
	em := newEmitter(func(e Event) {
		if e.Warning != nil {
			warns = append(warns, e.Warning.Code)
		}
	}, "")

	cs := &CutSpec{
		Ranges:       []TimeRange{{Start: time.Second, End: 2 * time.Second}},
		SponsorBlock: []sponsorblock.Category{sponsorblock.CategoryMusicOffTopic},
		OnError:      ProceedUncut,
	}
	all, sb, err := c.collectRanges(context.Background(), cs, sbVideoID, em)
	if err != nil {
		t.Fatalf("ProceedUncut should not error: %v", err)
	}
	if len(all) != 1 || sb != nil {
		t.Errorf("ProceedUncut = (%v, %v), want explicit only", all, sb)
	}
	if len(warns) != 1 || warns[0] != WarnProceedUncut {
		t.Errorf("warnings = %v, want [WarnProceedUncut]", warns)
	}
}

func TestCollectRangesFailDownload(t *testing.T) {
	c := sbServer(t, http.StatusInternalServerError, "boom")
	cs := &CutSpec{
		SponsorBlock: []sponsorblock.Category{sponsorblock.CategoryMusicOffTopic},
		OnError:      FailDownload,
	}
	_, _, err := c.collectRanges(context.Background(), cs, sbVideoID, newEmitter(nil, ""))
	if err == nil {
		t.Fatal("FailDownload should propagate the SponsorBlock fetch error")
	}
}

func TestSponsorBlockTimeoutPrecedence(t *testing.T) {
	c, err := New(Options{
		Timeouts:     Timeouts{SponsorBlock: 5 * time.Second},
		SponsorBlock: SponsorBlockOptions{Timeout: 3 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Per-request CutSpec.Timeout wins.
	if got := c.sponsorBlockTimeout(&CutSpec{Timeout: time.Second}); got != time.Second {
		t.Errorf("CutSpec.Timeout = %v, want 1s", got)
	}
	// Then the SponsorBlock option.
	if got := c.sponsorBlockTimeout(&CutSpec{}); got != 3*time.Second {
		t.Errorf("option timeout = %v, want 3s", got)
	}
	// Then the per-operation timeout.
	c2, _ := New(Options{Timeouts: Timeouts{SponsorBlock: 5 * time.Second}})
	if got := c2.sponsorBlockTimeout(&CutSpec{}); got != 5*time.Second {
		t.Errorf("operation timeout = %v, want 5s", got)
	}
}
