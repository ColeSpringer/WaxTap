package httpx

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestHostLimiterSpacesRequests(t *testing.T) {
	// 100 qps => 10ms spacing. Three sequential requests to one host take at
	// least two intervals (the first is admitted immediately).
	l := NewHostLimiter(100)
	ctx := context.Background()

	start := time.Now()
	for range 3 {
		if err := l.Wait(ctx, "a.example"); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("3 spaced requests took %v, want >= 20ms", elapsed)
	}
}

func TestHostLimiterIndependentHosts(t *testing.T) {
	// A slow host must not delay a different host: the second host's first
	// request is admitted immediately.
	l := NewHostLimiter(1) // 1s spacing
	ctx := context.Background()

	if err := l.Wait(ctx, "slow.example"); err != nil {
		t.Fatalf("Wait slow: %v", err)
	}
	start := time.Now()
	if err := l.Wait(ctx, "other.example"); err != nil {
		t.Fatalf("Wait other: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("first request to a fresh host waited %v, want ~0", elapsed)
	}
}

func TestHostLimiterZeroDisables(t *testing.T) {
	l := NewHostLimiter(0)
	start := time.Now()
	for range 100 {
		if err := l.Wait(context.Background(), "a.example"); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("disabled limiter waited %v, want ~0", elapsed)
	}
}

func TestHostLimiterHonorsContext(t *testing.T) {
	l := NewHostLimiter(1) // 1s spacing
	// Consume the immediate slot, so the next call must wait ~1s.
	if err := l.Wait(context.Background(), "a.example"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, "a.example"); err == nil {
		t.Fatal("Wait returned nil, want context deadline error")
	}
}

func TestHostLimiterAlreadyCanceledDoesNotReserve(t *testing.T) {
	l := NewHostLimiter(1) // 1s spacing
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// An already-canceled context must return its error without charging a slot.
	if err := l.Wait(ctx, "a.example"); err == nil {
		t.Fatal("Wait with canceled ctx returned nil, want error")
	}
	// The schedule was not advanced, so a fresh request is admitted immediately.
	start := time.Now()
	if err := l.Wait(context.Background(), "a.example"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("fresh request waited %v after a canceled one reserved nothing, want ~0", elapsed)
	}
}

func TestHostLimiterRollsBackCanceledWait(t *testing.T) {
	l := NewHostLimiter(2) // 500ms spacing
	// Consume the immediate slot so the next request must wait.
	if err := l.Wait(context.Background(), "a.example"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// This request reserves the next slot, then its wait is canceled.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx, "a.example"); err == nil {
		t.Fatal("Wait should have been canceled")
	}
	// Because the canceled reservation was rolled back (it was the tail), the next
	// real request only waits out the original ~500ms slot, not two slots.
	start := time.Now()
	if err := l.Wait(context.Background(), "a.example"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Errorf("next request waited %v; a canceled reservation was not rolled back", elapsed)
	}
}

func TestHostLimiterPenalizeClampsForwardNotAdditive(t *testing.T) {
	l := NewHostLimiter(0) // cooldown-only (QPS disabled)
	// Penalties extend the deadline; they do not add their durations.
	l.Penalize("a.example", 200*time.Millisecond)
	l.Penalize("a.example", 200*time.Millisecond)

	start := time.Now()
	if err := l.Wait(context.Background(), "a.example"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("Wait took %v; cooldown not honored with QPS disabled (want ~200ms)", elapsed)
	}
	if elapsed > 350*time.Millisecond {
		t.Errorf("Wait took %v; penalties stacked instead of clamping forward (want ~200ms)", elapsed)
	}
}

func TestHostLimiterCooldownAppliedMidWait(t *testing.T) {
	l := NewHostLimiter(2) // 500ms spacing
	// Consume the immediate slot.
	if err := l.Wait(context.Background(), "a.example"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = l.Wait(context.Background(), "a.example")
		done <- time.Since(start)
	}()
	// Apply a longer cooldown after the waiter reserves its slot.
	time.Sleep(50 * time.Millisecond)
	l.Penalize("a.example", 700*time.Millisecond)

	elapsed := <-done
	if elapsed < 650*time.Millisecond {
		t.Errorf("waiter admitted after %v; it ignored a cooldown applied mid-wait (want >= ~750ms)", elapsed)
	}
}

func TestHostLimiterMultipleWaitersHonorExtendedCooldown(t *testing.T) {
	l := NewHostLimiter(0) // cooldown-only, so all waiters share one schedule
	l.Penalize("a.example", 200*time.Millisecond)

	const n = 5
	results := make(chan time.Duration, n)
	start := time.Now()
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			_ = l.Wait(context.Background(), "a.example")
			results <- time.Since(start)
		})
	}
	// Extend the cooldown while waiters are blocked.
	time.Sleep(50 * time.Millisecond)
	l.Penalize("a.example", 400*time.Millisecond) // extends to ~start+450ms
	wg.Wait()
	close(results)

	for elapsed := range results {
		if elapsed < 350*time.Millisecond {
			t.Errorf("a waiter admitted after %v; it ignored the extended cooldown (want >= ~450ms)", elapsed)
		}
	}
}

func TestHostLimiterSpacingResumesAfterCooldown(t *testing.T) {
	l := NewHostLimiter(20) // 50ms spacing
	l.Penalize("a.example", 100*time.Millisecond)

	start := time.Now()
	for range 3 {
		if err := l.Wait(context.Background(), "a.example"); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	elapsed := time.Since(start)
	// The cooldown and two spacing intervals should take about 200ms.
	if elapsed < 170*time.Millisecond {
		t.Errorf("3 requests after cooldown took %v; QPS spacing did not resume (want ~200ms)", elapsed)
	}
}

func TestHostLimiterConcurrent(t *testing.T) {
	// The limiter must be safe under concurrent Wait calls for the same and
	// different hosts; run with -race.
	l := NewHostLimiter(1000)
	var wg sync.WaitGroup
	for h := range 4 {
		host := string(rune('a'+h)) + ".example"
		for range 25 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = l.Wait(context.Background(), host)
			}()
		}
	}
	wg.Wait()
}
