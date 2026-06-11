package waxtap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube"
)

func TestRefreshFailure(t *testing.T) {
	t.Run("caller canceled propagates", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Even if the underlying error looks like a generic failure, a done fctx wins.
		err := refreshFailure(ctx, "re-extract", errors.New("boom"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrURLExpired) {
			t.Errorf("cancellation must not be reclassified as ErrURLExpired: %v", err)
		}
	})
	t.Run("rate limit propagates", func(t *testing.T) {
		err := refreshFailure(context.Background(), "re-resolve", ErrRateLimited)
		if !errors.Is(err, ErrRateLimited) {
			t.Fatalf("err = %v, want ErrRateLimited", err)
		}
		if errors.Is(err, ErrURLExpired) {
			t.Errorf("rate limiting must not be reclassified as ErrURLExpired: %v", err)
		}
	})
	t.Run("availability verdict propagates", func(t *testing.T) {
		// A mid-download availability change must remain visible to the caller.
		login := &waxerr.PlayabilityError{Status: "LOGIN_REQUIRED", Sentinel: waxerr.ErrLoginRequired}
		err := refreshFailure(context.Background(), "re-extract", login)
		if !errors.Is(err, ErrLoginRequired) {
			t.Fatalf("err = %v, want ErrLoginRequired preserved", err)
		}
		if errors.Is(err, ErrURLExpired) {
			t.Errorf("availability verdict must not be reclassified as ErrURLExpired: %v", err)
		}
	})
	t.Run("ordinary failure becomes url-expired", func(t *testing.T) {
		err := refreshFailure(context.Background(), "re-extract attempt profile:0", errors.New("network blip"))
		if !errors.Is(err, ErrURLExpired) {
			t.Fatalf("err = %v, want ErrURLExpired", err)
		}
		if !strings.Contains(err.Error(), "network blip") {
			t.Errorf("err = %q, want the cause retained", err)
		}
	})
}

func TestIsIncompleteDelivery(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"incomplete-stream", ErrIncompleteStream, true},
		{"url-expired", ErrURLExpired, true},
		// The production errors are multi-level %w wraps (download/file.go, the SABR
		// stall, the refresh path); the predicate must unwrap them.
		{"wrapped incomplete (%w)", fmt.Errorf("chunk: %w", ErrIncompleteStream), true},
		{"double-wrapped url-expired", fmt.Errorf("outer: %w", fmt.Errorf("renew: %w", ErrURLExpired)), true},
		{"two-verb wrap", fmt.Errorf("%w: stalled: %w", ErrIncompleteStream, errors.New("cause")), true},
		{"string lookalike, not wrapped", errors.New("x: " + ErrIncompleteStream.Error()), false},
		{"unavailable", ErrVideoUnavailable, false},
		{"needs-po-token", ErrNeedsPOToken, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isIncompleteDelivery(tc.err); got != tc.want {
				t.Errorf("isIncompleteDelivery(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAttemptErrorsAggregate_IncompleteDominates(t *testing.T) {
	var causes attemptErrors
	causes.add(youtube.AttemptID("profile:0"), ErrIncompleteStream)
	causes.add(youtube.AttemptID("profile:2"), ErrNeedsPOToken)

	err := causes.aggregate()
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("aggregate = %v, want errors.Is ErrIncompleteStream", err)
	}
	if !strings.Contains(err.Error(), "no attempted client delivered a complete stream") {
		t.Errorf("message = %q, want the incomplete-stream phrasing", err)
	}
	if !strings.Contains(err.Error(), "profile:0") || !strings.Contains(err.Error(), "profile:2") {
		t.Errorf("message = %q, want it to list the attempts tried", err)
	}
}

func TestAttemptErrorsAggregate_PreservesAvailability(t *testing.T) {
	unavailable := &waxerr.PlayabilityError{Status: "ERROR", Sentinel: waxerr.ErrVideoUnavailable}
	var causes attemptErrors
	causes.add(youtube.AttemptID("profile:0"), ErrIncompleteStream)
	causes.add("", unavailable) // chain exhausted: no single attempt to name

	err := causes.aggregate()
	if !errors.Is(err, ErrVideoUnavailable) {
		t.Fatalf("aggregate = %v, want errors.Is ErrVideoUnavailable", err)
	}
	if errors.Is(err, ErrIncompleteStream) {
		t.Errorf("availability must not be collapsed into incomplete: %v", err)
	}
	if strings.Contains(err.Error(), "tried ,") {
		t.Errorf("message = %q, the empty attempt id must be omitted from the tried list", err)
	}
}

func TestAttemptErrorsAggregate_URLExpiredMapsToIncomplete(t *testing.T) {
	var causes attemptErrors
	causes.add(youtube.AttemptID("profile:0"), fmt.Errorf("%w: refresh", ErrURLExpired))
	causes.add(youtube.AttemptID("profile:2"), ErrNeedsPOToken)

	err := causes.aggregate()
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("aggregate = %v, want errors.Is ErrIncompleteStream (exit 7)", err)
	}
	if !errors.Is(err, ErrURLExpired) {
		t.Errorf("aggregate = %v, want the ErrURLExpired cause preserved", err)
	}
}

func TestAttemptErrorsAggregate_PreservesIncompleteDetail(t *testing.T) {
	detailed := fmt.Errorf("%w: short chunk at offset 524288: got 12 bytes, want 4096", ErrIncompleteStream)
	var causes attemptErrors
	causes.add(youtube.AttemptID("profile:0"), detailed)

	err := causes.aggregate()
	if !errors.Is(err, ErrIncompleteStream) {
		t.Fatalf("aggregate = %v, want ErrIncompleteStream", err)
	}
	if !strings.Contains(err.Error(), "short chunk at offset 524288") {
		t.Errorf("message = %q, want the dominant cause's detail preserved", err)
	}
}

func TestSelectSourceIndex_PinsItagAcrossSwitch(t *testing.T) {
	c := &Client{log: slog.New(slog.DiscardHandler)}
	formats := []Format{
		{Itag: 999, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 256000}, // selector would prefer this
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 130000},
	}
	idx, err := c.selectSourceIndex(Request{}, format.Target{}, formats, 251)
	if err != nil {
		t.Fatal(err)
	}
	if formats[idx].Itag != 251 {
		t.Errorf("itag = %d, want 251 (pinned across the switch)", formats[idx].Itag)
	}

	idx, err = c.selectSourceIndex(Request{}, format.Target{}, formats, 0)
	if err != nil {
		t.Fatal(err)
	}
	if formats[idx].Itag != 999 {
		t.Errorf("itag = %d, want 999 (selector, no pin)", formats[idx].Itag)
	}
}

func TestStreamErr_RecordReclassifiesURLExpired(t *testing.T) {
	var got error
	em := newEmitter(func(e Event) {
		if e.Stage == StageFailed {
			got = e.Err
		}
	}, "vid")

	var s streamErr
	s.record(fmt.Errorf("mid-read: %w", ErrURLExpired))
	s.terminal(em)

	if !errors.Is(got, ErrIncompleteStream) {
		t.Fatalf("terminal err = %v, want ErrIncompleteStream", got)
	}
}
