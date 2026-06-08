package resolver

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveSolve is an opt-in end-to-end check against YouTube's current player.
// It is skipped unless WAXTAP_LIVE=1 and needs network access. It verifies the
// two failures the whole-player solver fixes: the signature timestamp is present
// (Bug B) and the n transform returns a real value rather than undefined (Bug A).
//
// The player rotates often, so this is a maintenance signal, not a CI gate. Set
// WAXTAP_LIVE_VIDEO to override the discovery video.
func TestLiveSolve(t *testing.T) {
	if os.Getenv("WAXTAP_LIVE") != "1" {
		t.Skip("set WAXTAP_LIVE=1 to run the live player solve test")
	}
	videoID := os.Getenv("WAXTAP_LIVE_VIDEO")
	if videoID == "" {
		videoID = "jNQXAC9IVRw" // "Me at the zoo": the canonical non-commercial test video
	}

	p := New(Config{HTTP: &http.Client{Timeout: 30 * time.Second}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rc := Context{VideoID: videoID}

	sts, err := p.SignatureTimestamp(ctx, rc)
	if err != nil {
		t.Fatalf("SignatureTimestamp: %v", err)
	}
	if sts <= 0 {
		t.Errorf("signature timestamp = %d, want > 0 (Bug B)", sts)
	}

	const sampleN = "Zjuhd8Vr8MnqKaB1"
	out, err := p.DescrambleN(ctx, rc, "https://rr1.googlevideo.com/videoplayback?n="+sampleN)
	if err != nil {
		t.Fatalf("DescrambleN: %v", err)
	}
	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("parse descrambled URL: %v", err)
	}
	got := u.Query().Get("n")
	if got == "" || got == "undefined" || got == sampleN || strings.HasSuffix(got, sampleN) {
		t.Fatalf("decoded n = %q, want a real transform of %q (Bug A)", got, sampleN)
	}
	t.Logf("live solve OK: sts=%d  n %q -> %q", sts, sampleN, got)
}
