package youtube

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/waxerr"
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

// A per-operation deadline that expires while an attempt is failing must not
// erase what failed. The shape it exists for is a dead proxy: every request
// reports proxyconnect, the extraction budget runs out, and reporting only
// "context deadline exceeded" sends the user at their network instead of the
// setting that broke.
//
// This is the same policy httpx.Do applies one layer down: a cancellation is
// the caller giving up and outranks the cause; an expired deadline does not.
func TestExtractExcluding_DeadlineKeepsAttemptCause(t *testing.T) {
	proxyFail := &url.Error{
		Op:  "Post",
		URL: "https://www.youtube.com/youtubei/v1/player",
		Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("i/o timeout")},
	}

	t.Run("expired deadline keeps the cause", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		c := newTestClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, proxyFail
		}))
		// Stand in for a budget that ran out during the attempt.
		ctx, tcancel := context.WithTimeout(ctx, time.Nanosecond)
		defer tcancel()
		defer cancel()

		_, err := c.ExtractExcluding(ctx, "testVideo01", nil)
		if op, ok := errors.AsType[*net.OpError](err); !ok || op.Op != "proxyconnect" {
			t.Fatalf("err = %v, want the attempt's proxyconnect cause, not a bare deadline", err)
		}
	})

	t.Run("cancellation still outranks", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := newTestClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, proxyFail
		}))
		_, err := c.ExtractExcluding(ctx, "testVideo01", nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// The default-budget shape of a dead proxy: the visitor bootstrap fails with
// proxyconnect and is deliberately worked around (synthetic visitorData), its
// dial timeouts consume the extraction budget, and the player request is then
// cut off mid-dial, where net/http reports the bare context error with no
// transport cause at all. The only request that named the proxy was the one
// whose failure was swallowed, so the session has to carry it forward.
func TestExtractExcluding_BareDeadlineNamesSwallowedBootstrapCause(t *testing.T) {
	proxyFail := func(u string) error {
		return &url.Error{
			Op:  "Get",
			URL: u,
			Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("i/o timeout")},
		}
	}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/player") {
			<-r.Context().Done()
			// net/http reports the context error itself when it fires mid-dial.
			return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: r.Context().Err()}
		}
		return nil, proxyFail(r.URL.String())
	})
	// The bootstrap only runs on a jar-backed client, as in the real CLI.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := New(Config{HTTP: httpx.New(httpx.Config{
		HTTPClient:  &http.Client{Transport: rt, Jar: jar},
		MaxRetries:  1,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  2 * time.Millisecond,
	})})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = c.ExtractExcluding(ctx, "testVideo01", nil)
	if op, ok := errors.AsType[*net.OpError](err); !ok || op.Op != "proxyconnect" {
		t.Fatalf("err = %v, want the swallowed bootstrap's proxyconnect cause folded in", err)
	}
	// Both truths survive: what stopped the run and what to fix.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to still report the expired deadline", err)
	}
}

// An availability verdict from an earlier profile must not become the report of
// a run the budget killed: the chain would have kept trying more clients, so
// "video unavailable" (exit 3, definitive-sounding) is not established, while
// the expired deadline is a fact. The earlier verdict yields only when the
// interrupted attempt says nothing at all and the earlier error names a
// transport cause worth surfacing.
func TestExtractExcluding_DeadlineNotHijackedByDomainVerdict(t *testing.T) {
	unavailable := readFixture(t, "player_unavailable.json") // status ERROR
	var playerCalls int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/player") {
			return fixtureResp(http.StatusNotFound, nil), nil
		}
		playerCalls++
		if playerCalls == 1 {
			return fixtureResp(http.StatusOK, unavailable), nil
		}
		<-r.Context().Done()
		return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: r.Context().Err()}
	})
	c := newTestClientWith(rt, []ClientProfile{makeProfile(profileAndroidVR), makeProfile(profileAndroidVR)}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := c.ExtractExcluding(ctx, "testVideo01", nil)
	if playerCalls < 2 {
		t.Fatalf("only %d player call(s); the test never reached the deadline shape", playerCalls)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the expired deadline reported", err)
	}
	if errors.Is(err, waxerr.ErrVideoUnavailable) {
		t.Fatalf("err = %v, want the earlier availability verdict not to hijack a budget kill", err)
	}
}
