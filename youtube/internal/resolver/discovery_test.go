package resolver

import (
	"context"
	"net/http"
	"testing"

	"github.com/colespringer/waxtap/internal/clientident"
)

// TestPlayer_DiscoveryUserAgent verifies the configured and default User-Agents
// used for discovery pages and base.js.
func TestPlayer_DiscoveryUserAgent(t *testing.T) {
	stream := "https://rr1.googlevideo.com/videoplayback?itag=140&n=12345&expire=2000000000&clen=3400000"
	cand := Candidate{SignatureCipher: cipherURL("ABCDEFGH", "sig", stream)}

	run := func(t *testing.T, cfgUA, wantUA string) {
		t.Helper()
		srv := newFixtureServer(t)
		inner := srv.doer()
		var uas []string
		rec := doerFunc(func(r *http.Request) (*http.Response, error) {
			uas = append(uas, r.Header.Get("User-Agent"))
			return inner.Do(r)
		})
		p := New(Config{HTTP: rec, DiscoveryUserAgent: cfgUA})
		if _, err := p.Resolve(context.Background(), Context{VideoID: "vid123"}, cand); err != nil {
			t.Fatal(err)
		}
		if len(uas) == 0 {
			t.Fatal("no discovery/base.js requests recorded")
		}
		for _, ua := range uas {
			if ua != wantUA {
				t.Errorf("discovery UA = %q, want %q", ua, wantUA)
			}
		}
	}

	t.Run("explicit", func(t *testing.T) { run(t, "UA-EXPLICIT/9", "UA-EXPLICIT/9") })
	t.Run("default", func(t *testing.T) { run(t, "", clientident.UserAgent(0)) })
}
