package waxtap

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
)

func TestEmitThrottleDedup(t *testing.T) {
	var mu sync.Mutex
	var throttled, retried int
	em := newEmitter(func(ev Event) {
		if ev.Stage != StageWarning || ev.Warning == nil {
			return
		}
		mu.Lock()
		switch ev.Warning.Code {
		case WarnThrottled:
			throttled++
		case WarnRateLimitedRetried:
			retried++
		}
		mu.Unlock()
	}, "dummyVideo0")

	// Parallel reports of the same response produce one warning.
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			emitThrottle(em, httpx.ThrottleEvent{
				Host: "googlevideo.com", StatusCode: 429, Penalty: time.Second, Phase: httpx.ThrottleDetected,
			})
		})
	}
	wg.Wait()

	// The retry phase produces a separate warning.
	emitThrottle(em, httpx.ThrottleEvent{Host: "googlevideo.com", StatusCode: 429, Phase: httpx.ThrottleRetryStarted})

	if throttled != 1 {
		t.Errorf("WarnThrottled emitted %d times, want 1 (deduped)", throttled)
	}
	if retried != 1 {
		t.Errorf("WarnRateLimitedRetried emitted %d times, want 1", retried)
	}
	if len(em.warnings) != 2 {
		t.Errorf("recorded warnings = %d, want 2", len(em.warnings))
	}
}

func TestEmitterConcurrentProgressAndThrottle(t *testing.T) {
	// Run with -race to verify concurrent progress and warning delivery.
	em := newEmitter(func(Event) {}, "dummyVideo0")
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() { em.progress(int64(i), 100) })
		wg.Go(func() {
			emitThrottle(em, httpx.ThrottleEvent{Host: "h", StatusCode: 429, Phase: httpx.ThrottleDetected})
		})
	}
	wg.Wait()

	if len(em.warnings) != 1 {
		t.Errorf("recorded warnings = %d, want 1 (deduped under concurrency)", len(em.warnings))
	}
}

func TestEmitterCallbackPanicRecovered(t *testing.T) {
	em := newEmitter(func(Event) { panic("boom") }, "dummyVideo0")
	em.warn(WarnProceedUncut, "x")
	emitThrottle(em, httpx.ThrottleEvent{Host: "h", StatusCode: 429, Phase: httpx.ThrottleDetected})
	if len(em.warnings) != 2 {
		t.Errorf("recorded warnings = %d, want 2", len(em.warnings))
	}
}

// TestEmitterWarnNthNumbersPerJob covers F8: repeated warnings of the same code
// number themselves, job-wide, so a run that rotated twice does not read as one
// duplicated entry. The ordinal must survive a whole-chain retry, whose second
// attempt carries its own per-attempt counters.
func TestEmitterWarnNthNumbersPerJob(t *testing.T) {
	em := newEmitter(nil, "dummyVideo0")
	rotate := func() {
		em.warnNth(WarnSessionRotated, func(n int) string { return fmt.Sprintf("rotated (rotation %d)", n) })
	}
	rotate()
	em.warn(WarnFallbackProfile, "unrelated") // an interleaved code must not shift the count
	rotate()
	em.warnNth(WarnURLReResolved, func(n int) string { return fmt.Sprintf("re-resolved (refresh %d)", n) })

	got := em.collected()
	want := []string{"rotated (rotation 1)", "unrelated", "rotated (rotation 2)", "re-resolved (refresh 1)"}
	if len(got) != len(want) {
		t.Fatalf("warnings = %v, want %d entries", got, len(want))
	}
	for i, w := range got {
		if w.Detail != want[i] {
			t.Errorf("warning %d detail = %q, want %q", i, w.Detail, want[i])
		}
	}
}

// TestEmitterWarnNthConcurrent pins the ordinals as unique under concurrency:
// the count and the append are one critical section, so no two callers draw the
// same number.
func TestEmitterWarnNthConcurrent(t *testing.T) {
	em := newEmitter(nil, "dummyVideo0")
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			em.warnNth(WarnSessionRotated, func(i int) string { return fmt.Sprintf("%d", i) })
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, w := range em.collected() {
		if seen[w.Detail] {
			t.Fatalf("ordinal %q was drawn twice", w.Detail)
		}
		seen[w.Detail] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct ordinals, want %d", len(seen), n)
	}
}

// The throttle detail must not claim a pause WaxTap does not take: the penalty
// is a wait the limiter holds later requests to that host for, and the request
// that hit the limit is already past it.
func TestThrottleDetailDoesNotClaimAPause(t *testing.T) {
	cases := []struct {
		name  string
		code  WarningCode
		ev    httpx.ThrottleEvent
		want  string
		avoid string
	}{
		{
			name: "retry started",
			code: WarnRateLimitedRetried,
			ev:   httpx.ThrottleEvent{Host: "youtube.com", StatusCode: 429},
			want: "retrying request to youtube.com after HTTP 429",
		},
		{
			name:  "a stated penalty",
			code:  WarnThrottled,
			ev:    httpx.ThrottleEvent{Host: "youtube.com", StatusCode: 429, Penalty: 30 * time.Second},
			want:  "rate limited by youtube.com (HTTP 429); further requests to it wait 30s",
			avoid: "pausing",
		},
		{
			name: "no penalty",
			code: WarnThrottled,
			ev:   httpx.ThrottleEvent{Host: "youtube.com", StatusCode: 429},
			want: "rate limited by youtube.com (HTTP 429)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := throttleDetail(tc.code, tc.ev)
			if got != tc.want {
				t.Errorf("detail = %q, want %q", got, tc.want)
			}
			if tc.avoid != "" && strings.Contains(got, tc.avoid) {
				t.Errorf("detail = %q, want it not to say %q", got, tc.avoid)
			}
		})
	}
}
