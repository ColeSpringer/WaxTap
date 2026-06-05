package youtube

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestExtract_PlayabilityErrorFallsThrough verifies that a generic ERROR from one
// client no longer aborts the chain: a later client that returns OK still wins.
func TestExtract_PlayabilityErrorFallsThrough(t *testing.T) {
	ok := readFixture(t, "player_ok.json")
	errBody := readFixture(t, "player_unavailable.json") // status ERROR
	var playerCalls int
	c := newTestClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/player") {
			playerCalls++
			if playerCalls == 1 {
				return fixtureResp(http.StatusOK, errBody), nil
			}
			return fixtureResp(http.StatusOK, ok), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}))

	ext, err := c.Extract(context.Background(), "testVideo01")
	if err != nil {
		t.Fatalf("extract should fall through ERROR to the next client: %v", err)
	}
	if ext.Video().Title != "Test Song" {
		t.Errorf("title = %q", ext.Video().Title)
	}
	if playerCalls < 2 {
		t.Errorf("playerCalls = %d, want >= 2 (chain continued past ERROR)", playerCalls)
	}
}
