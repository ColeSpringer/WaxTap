package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// recordingLimiter records penalties and does not delay requests.
type recordingLimiter struct {
	mu        sync.Mutex
	penalties []time.Duration
}

func (r *recordingLimiter) Wait(context.Context, string) error { return nil }

func (r *recordingLimiter) Penalize(_ string, d time.Duration) {
	r.mu.Lock()
	r.penalties = append(r.penalties, d)
	r.mu.Unlock()
}

func (r *recordingLimiter) snapshot() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.penalties...)
}

// collectThrottle returns a context with a recording hook and a snapshot
// function.
func collectThrottle(ctx context.Context) (context.Context, func() []ThrottleEvent) {
	var mu sync.Mutex
	var events []ThrottleEvent
	ctx = WithThrottleHook(ctx, func(e ThrottleEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})
	return ctx, func() []ThrottleEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]ThrottleEvent(nil), events...)
	}
}

func countPhase(events []ThrottleEvent, phase ThrottlePhase) int {
	n := 0
	for _, e := range events {
		if e.Phase == phase {
			n++
		}
	}
	return n
}

func newReq(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}

func TestDo_RetriesThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway) // 502: retryable
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, MaxRetries: 5})
	resp, err := c.Do(newReq(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestDo_RetryAfterBeyondCapFailsFast(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(3600)) // 1 hour
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Config{MaxRetryWait: 30 * time.Second, MaxRetries: 5})
	start := time.Now()
	_, err := c.Do(newReq(t, context.Background(), srv.URL))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected fail-fast, but slept %s", elapsed)
	}

	rl, ok := errors.AsType[*waxerr.RateLimitError](err)
	if !ok {
		t.Fatalf("err = %v, want *waxerr.RateLimitError", err)
	}
	if !errors.Is(err, waxerr.ErrRateLimited) {
		t.Fatalf("err does not match ErrRateLimited: %v", err)
	}
	if rl.RetryAfter != time.Hour {
		t.Fatalf("RetryAfter = %s, want 1h", rl.RetryAfter)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (no retry past cap)", got)
	}
}

func TestDo_ContextCancelNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	c := New(Config{MaxRetries: 5})
	_, err := c.Do(newReq(t, ctx, srv.URL))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestDo_403WithRetryAfterIsRateLimited(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", strconv.Itoa(3600)) // 1 hour, beyond cap
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(Config{MaxRetryWait: 30 * time.Second, MaxRetries: 5})
	_, err := c.Do(newReq(t, context.Background(), srv.URL))

	rl, ok := errors.AsType[*waxerr.RateLimitError](err)
	if !ok {
		t.Fatalf("err = %v, want *waxerr.RateLimitError", err)
	}
	if rl.StatusCode != http.StatusForbidden {
		t.Fatalf("StatusCode = %d, want 403", rl.StatusCode)
	}
	if !errors.Is(err, waxerr.ErrRateLimited) {
		t.Fatalf("err does not match ErrRateLimited: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (fail fast past cap)", got)
	}
}

func TestDo_Bare403PassesThrough(t *testing.T) {
	// A 403 without Retry-After passes through so the resolver/download layer can
	// classify it as PO-token-required, URL-expired, or another failure.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(Config{MaxRetries: 5})
	resp, err := c.Do(newReq(t, context.Background(), srv.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1 (no retry on bare 403)", got)
	}
}

func TestDo_RateLimitPenalizesByRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(5))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rl := &recordingLimiter{}
	// Disable retries so the test does not wait for Retry-After.
	c := New(Config{Limiter: rl, Cooldown: 2 * time.Second, MaxRetryWait: 30 * time.Second, MaxRetries: -1})
	if _, err := c.Do(newReq(t, context.Background(), srv.URL)); err == nil {
		t.Fatal("Do returned nil, want a rate-limit error")
	}
	got := rl.snapshot()
	if len(got) != 1 || got[0] != 5*time.Second {
		t.Fatalf("penalties = %v, want [5s]", got)
	}
}

func TestDo_RateLimitNoRetryAfterPenalizesByCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rl := &recordingLimiter{}
	c := New(Config{Limiter: rl, Cooldown: 3 * time.Second, MaxRetries: -1})
	if _, err := c.Do(newReq(t, context.Background(), srv.URL)); err == nil {
		t.Fatal("Do returned nil, want a rate-limit error")
	}
	got := rl.snapshot()
	if len(got) != 1 || got[0] != 3*time.Second {
		t.Fatalf("penalties = %v, want [3s]", got)
	}
}

func TestDo_RateLimitOverCapPenalizesByMaxRetryWaitAndFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(3600))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	rl := &recordingLimiter{}
	c := New(Config{Limiter: rl, Cooldown: time.Second, MaxRetryWait: 30 * time.Second, MaxRetries: 5})
	start := time.Now()
	_, err := c.Do(newReq(t, context.Background(), srv.URL))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected fail-fast, slept %s", elapsed)
	}
	if _, ok := errors.AsType[*waxerr.RateLimitError](err); !ok {
		t.Fatalf("err = %v, want *waxerr.RateLimitError", err)
	}
	got := rl.snapshot()
	if len(got) != 1 || got[0] != 30*time.Second {
		t.Fatalf("penalties = %v, want [30s] (clamped to MaxRetryWait)", got)
	}
}

func TestDo_ThrottleHookDetectedFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, events := collectThrottle(context.Background())
	c := New(Config{MaxRetries: -1, Cooldown: 2 * time.Second})
	_, _ = c.Do(newReq(t, ctx, srv.URL))

	got := events()
	if n := countPhase(got, ThrottleDetected); n != 1 {
		t.Fatalf("ThrottleDetected fired %d times, want 1", n)
	}
	if n := countPhase(got, ThrottleRetryStarted); n != 0 {
		t.Fatalf("ThrottleRetryStarted fired %d times, want 0 (no retry attempted)", n)
	}
	if got[0].Penalty != 2*time.Second || got[0].StatusCode != http.StatusTooManyRequests {
		t.Fatalf("detected event = %+v, want penalty 2s, status 429", got[0])
	}
}

func TestDo_ThrottleHookRetryStartedFires(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, events := collectThrottle(context.Background())
	c := New(Config{MaxRetries: 3, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	resp, err := c.Do(newReq(t, ctx, srv.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	got := events()
	if n := countPhase(got, ThrottleDetected); n != 1 {
		t.Fatalf("ThrottleDetected fired %d times, want 1", n)
	}
	if n := countPhase(got, ThrottleRetryStarted); n != 1 {
		t.Fatalf("ThrottleRetryStarted fired %d times, want 1 (the retry began)", n)
	}
}

func TestDo_ThrottleHookNoRetryStartedOnCancelDuringSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", strconv.Itoa(2))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ctx, events := collectThrottle(ctx)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	c := New(Config{MaxRetries: 5, MaxRetryWait: 30 * time.Second})
	_, err := c.Do(newReq(t, ctx, srv.URL))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	got := events()
	if n := countPhase(got, ThrottleDetected); n != 1 {
		t.Fatalf("ThrottleDetected fired %d times, want 1", n)
	}
	if n := countPhase(got, ThrottleRetryStarted); n != 0 {
		t.Fatalf("ThrottleRetryStarted fired %d times, want 0 (sleep was canceled)", n)
	}
}
