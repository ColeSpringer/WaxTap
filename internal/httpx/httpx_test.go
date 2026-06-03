package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

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

	var rl *waxerr.RateLimitError
	if !errors.As(err, &rl) {
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

	var rl *waxerr.RateLimitError
	if !errors.As(err, &rl) {
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
