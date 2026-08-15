package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/waxerr"
	"github.com/colespringer/waxtap/v3/youtube"
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

func TestWarnClientSubstitution(t *testing.T) {
	collect := func(a *acquired) []Event {
		var evs []Event
		em := newEmitter(func(e Event) { evs = append(evs, e) }, "vid")
		c := &Client{}
		c.warnClientSubstitution(em, a)
		return evs
	}
	t.Run("substitution emits fallback warning", func(t *testing.T) {
		evs := collect(&acquired{substitutedFrom: "WEB_EMBEDDED"})
		if len(evs) != 1 || evs[0].Warning == nil {
			t.Fatalf("events = %+v, want one warning", evs)
		}
		if !strings.Contains(evs[0].Warning.Detail, "used WEB through the watch-page fallback") {
			t.Errorf("detail = %q, want the WEB fallback", evs[0].Warning.Detail)
		}
		if !strings.Contains(evs[0].Warning.Detail, "WEB_EMBEDDED") {
			t.Errorf("detail = %q, want it to name the substituted client", evs[0].Warning.Detail)
		}
	})
	t.Run("no substitution is silent", func(t *testing.T) {
		if evs := collect(&acquired{}); len(evs) != 0 {
			t.Errorf("events = %+v, want none when substitutedFrom is empty", evs)
		}
	})
}

func TestBaseSkip(t *testing.T) {
	if skip := baseSkip(Request{}); len(skip) != 0 {
		t.Errorf("default baseSkip = %v, want empty (all fallbacks allowed)", skip)
	}
	skip := baseSkip(Request{NoFallback: true})
	if !skip[youtube.AttemptWatchPage] {
		t.Errorf("NoFallback baseSkip must exclude the watch-page attempt, got %v", skip)
	}
	// Only the watch-page is pre-skipped; the primary profile chain still runs.
	if skip[youtube.AttemptWebContext] {
		t.Errorf("NoFallback baseSkip must not pre-skip the web-context attempt, got %v", skip)
	}
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

func TestIsUpstreamDiagnostic(t *testing.T) {
	upstream := []error{
		ErrNeedsPOToken,
		ErrExtractionFailed,
		ErrCipherSolve,
		&waxerr.PlayabilityError{Status: "ERROR", Sentinel: waxerr.ErrVideoUnavailable},
		ErrLoginRequired,
	}
	for _, err := range upstream {
		if !isUpstreamDiagnostic(err) {
			t.Errorf("isUpstreamDiagnostic(%v) = false, want true", err)
		}
	}
	// Local I/O failures must never be classified as upstream, so they are never
	// masked by an earlier incomplete delivery.
	for _, err := range []error{os.ErrPermission, errors.New("write: no space left on device"), ErrIncompleteStream} {
		if isUpstreamDiagnostic(err) {
			t.Errorf("isUpstreamDiagnostic(%v) = true, want false", err)
		}
	}
}

func TestAttemptErrorsHasIncomplete(t *testing.T) {
	var causes attemptErrors
	if causes.hasIncomplete() {
		t.Error("empty causes should report no incomplete")
	}
	causes.add(youtube.AttemptID("profile:2"), ErrNeedsPOToken)
	if causes.hasIncomplete() {
		t.Error("needs-po-token alone is not an incomplete delivery")
	}
	causes.add(youtube.AttemptID("profile:0"), fmt.Errorf("chunk: %w", ErrIncompleteStream))
	if !causes.hasIncomplete() {
		t.Error("a recorded incomplete delivery should be detected")
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

func TestCancelCause(t *testing.T) {
	truncated := fmt.Errorf("%w: short chunk at offset 524288: got 12 bytes, want 4096", ErrIncompleteStream)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("canceled reclassifies", func(t *testing.T) {
		err := cancelCause(canceled, truncated)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		// Wrapped, not joined: a caller whose retry logic checks the sentinel must
		// not retry a download the user canceled.
		if errors.Is(err, ErrIncompleteStream) {
			t.Errorf("err = %v, want ErrIncompleteStream unmatchable after cancellation", err)
		}
		// The detail keeps naming the producing site in the message.
		if !strings.Contains(err.Error(), "short chunk at offset 524288") {
			t.Errorf("err = %q, want the masked detail retained", err)
		}
	})
	t.Run("live context is untouched", func(t *testing.T) {
		if err := cancelCause(context.Background(), truncated); !errors.Is(err, ErrIncompleteStream) {
			t.Errorf("err = %v, want the incomplete stream preserved on a live context", err)
		}
	})
	t.Run("already canceled is untouched", func(t *testing.T) {
		in := fmt.Errorf("acquire: %w", context.Canceled)
		if err := cancelCause(canceled, in); err != in {
			t.Errorf("err = %v, want the input returned unwrapped", err)
		}
	})
	t.Run("nil stays nil", func(t *testing.T) {
		if err := cancelCause(canceled, nil); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
}

// The terminal event and the returned error must agree on a canceled run: the
// substitution runs inside the same defer, before the emitter call.
func TestDownload_CancellationReachesTerminalEvent(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var failed error
	// A missing Output fails at the facade with a plain error, no network, which is
	// the shape the defer has to reclassify.
	_, derr := c.Download(ctx, Request{
		URL: "dummyVideo0",
		ProcessSpec: ProcessSpec{
			Events: func(e Event) {
				if e.Stage == StageFailed {
					failed = e.Err
				}
			},
		},
	})
	if !errors.Is(derr, context.Canceled) {
		t.Fatalf("returned err = %v, want context.Canceled", derr)
	}
	if failed == nil {
		t.Fatal("no Failed event emitted")
	}
	if failed.Error() != derr.Error() {
		t.Errorf("event err = %q, returned err = %q, want them to agree", failed, derr)
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
	s.record(t.Context(), fmt.Errorf("mid-read: %w", ErrURLExpired))
	s.terminal(em)

	if !errors.Is(got, ErrIncompleteStream) {
		t.Fatalf("terminal err = %v, want ErrIncompleteStream", got)
	}
}

// A late read failure under a canceled context is the cancellation, not a
// truncated stream: Stream has already returned, so this event is the only place
// that could claim otherwise.
func TestStreamErr_RecordKeepsCancellation(t *testing.T) {
	var got error
	em := newEmitter(func(e Event) {
		if e.Stage == StageFailed {
			got = e.Err
		}
	}, "vid")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var s streamErr
	s.record(ctx, fmt.Errorf("mid-read: %w", ErrURLExpired))
	s.terminal(em)

	if errors.Is(got, ErrIncompleteStream) {
		t.Fatalf("terminal err = %v, want no ErrIncompleteStream re-wrap", got)
	}
	// The event must say what happened, not just avoid saying the wrong thing: a
	// listener branching on cancellation has only this event to go on, since
	// Stream returned before the read failed.
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("terminal err = %v, want context.Canceled", got)
	}
	if !strings.Contains(got.Error(), "mid-read") {
		t.Errorf("terminal err = %q, want the read detail retained", got)
	}
}

// TestAttemptErrorsAllDeliveriesIncomplete pins the gate on the whole-chain
// retry. The default chain records causes from attempts that never reached the
// transfer (WEB stops at "requires a player PO token"), so a literal "every
// recorded cause is incomplete" would never fire on the chain that needs it.
func TestAttemptErrorsAllDeliveriesIncomplete(t *testing.T) {
	incomplete := fmt.Errorf("chunk: %w", ErrIncompleteStream)

	var empty attemptErrors
	if empty.allDeliveriesIncomplete() {
		t.Error("no causes at all is not evidence of an incomplete delivery")
	}

	var noDelivery attemptErrors
	noDelivery.add(youtube.AttemptID("profile:2"), ErrNeedsPOToken)
	if noDelivery.allDeliveriesIncomplete() {
		t.Error("an attempt that never reached the transfer must not qualify on its own")
	}

	// The measured real-chain shape: two clients capped, the rest unable to start.
	var realChain attemptErrors
	realChain.addDelivered(&acquired{attempt: "profile:0", stats: &refreshStats{}}, fmt.Errorf("%w: refresh budget spent", ErrURLExpired))
	realChain.addDelivered(&acquired{attempt: "profile:2", stats: &refreshStats{}}, incomplete)
	realChain.add(youtube.AttemptID("watch-page"), ErrNeedsPOToken)
	if !realChain.allDeliveriesIncomplete() {
		t.Error("every attempt that delivered was incomplete; the retry must be allowed")
	}

	// One delivery that failed for another reason is enough to decline: a second
	// pass would only repeat it.
	var mixed attemptErrors
	mixed.addDelivered(&acquired{attempt: "profile:0", stats: &refreshStats{}}, incomplete)
	mixed.addDelivered(&acquired{attempt: "profile:2", stats: &refreshStats{}}, ErrVideoUnavailable)
	if mixed.allDeliveriesIncomplete() {
		t.Error("an availability failure at delivery must stop the retry")
	}
}

// The rendered attempt lines end up in a playlist run's NDJSON error field as
// well as in the terminal message, and a cause can carry a signed stream URL, so
// redaction happens where the strings are built rather than at one display site.
func TestAttemptErrorsRenderedRedactsURLs(t *testing.T) {
	signed := "https://rr3---sn-x.googlevideo.com/videoplayback?expire=1&sig=SECRET&pot=TOKEN"
	var causes attemptErrors
	causes.pass = 1
	causes.addDelivered(
		&acquired{attempt: youtube.AttemptID("profile:0"), stats: &refreshStats{}},
		fmt.Errorf("%w: %v", ErrIncompleteStream, &url.Error{Op: "Get", URL: signed, Err: io.EOF}),
	)
	causes.pass = 2
	causes.add(youtube.AttemptID("watch-page"), fmt.Errorf("re-resolve: %s failed", signed))

	lines := causes.rendered()
	joined := strings.Join(lines, "; ")
	for _, leak := range []string{"sig=SECRET", "pot=TOKEN", "videoplayback"} {
		if strings.Contains(joined, leak) {
			t.Errorf("rendered leaked %q: %s", leak, joined)
		}
	}
	if !strings.Contains(joined, "googlevideo.com") {
		t.Errorf("rendered dropped the host, which is the useful half: %s", joined)
	}
	// A second pass over the same clients is distinguishable from one longer pass.
	if !strings.Contains(joined, "(pass 2)") || strings.Contains(lines[0], "(pass") {
		t.Errorf("rendered = %s, want only later passes marked", joined)
	}
	// The aggregate carries the same redacted text.
	if e := causes.aggregate(); strings.Contains(e.Error(), "sig=SECRET") {
		t.Errorf("aggregate leaked the signature: %v", e)
	}
}
