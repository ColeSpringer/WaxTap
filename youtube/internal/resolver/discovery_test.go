package resolver

import (
	"context"
	"net/http"
	"testing"

	"github.com/colespringer/waxtap/v2/internal/clientident"
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

func TestStripTCEVariant(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			"es6 tce rewritten",
			"https://www.youtube.com/s/player/9d2ef9ef/player_es6_tce.vflset/en_US/base.js",
			"https://www.youtube.com/s/player/9d2ef9ef/player_es6.vflset/en_US/base.js",
		},
		{
			"ias tce rewritten (any locale)",
			"https://www.youtube.com/s/player/abc123/player_ias_tce.vflset/de_DE/base.js",
			"https://www.youtube.com/s/player/abc123/player_ias.vflset/de_DE/base.js",
		},
		{
			"regular es6 unchanged",
			"https://www.youtube.com/s/player/abc123/player_es6.vflset/en_US/base.js",
			"https://www.youtube.com/s/player/abc123/player_es6.vflset/en_US/base.js",
		},
		{
			"embed build unchanged",
			"https://www.youtube.com/s/player/abc123/player_embed_es6.vflset/en_US/base.js",
			"https://www.youtube.com/s/player/abc123/player_embed_es6.vflset/en_US/base.js",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripTCEVariant(tc.in); got != tc.want {
				t.Errorf("stripTCEVariant(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPlayerURL_NormalizesTCEVariant verifies the rewrite is wired into the
// caller-supplied PlayerURL path (no network: this branch returns directly).
func TestPlayerURL_NormalizesTCEVariant(t *testing.T) {
	p := New(Config{})
	got, err := p.playerURL(context.Background(), Context{
		PlayerURL: "https://www.youtube.com/s/player/9d2ef9ef/player_es6_tce.vflset/en_US/base.js",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "https://www.youtube.com/s/player/9d2ef9ef/player_es6.vflset/en_US/base.js"
	if got != want {
		t.Errorf("playerURL = %q, want %q (_tce normalized to the solvable build)", got, want)
	}
}
