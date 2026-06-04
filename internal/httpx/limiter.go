package httpx

import (
	"context"
	"sync"
	"time"
)

// HostLimiter is a per-host request-spacing Limiter. Each host gets an
// independent schedule that admits at most one request every 1/qps seconds, so a
// burst of parallel chunk downloads to one origin is paced without affecting
// requests to other hosts. It is the politeness limiter the facade builds from
// Options.Politeness.PerHostQPS and shares across the youtube, googlevideo, and
// SponsorBlock hosts.
//
// The limiter does not allow token bursts; it spaces admissions. That keeps
// playlist and chunk fan-out from hitting one origin at once while leaving other
// hosts independent. A non-positive qps disables limiting.
type HostLimiter struct {
	interval time.Duration

	mu      sync.Mutex
	buckets map[string]*hostBucket
}

// NewHostLimiter returns a per-host limiter admitting qps requests per second per
// host. A qps <= 0 yields a limiter that never waits.
func NewHostLimiter(qps float64) *HostLimiter {
	var interval time.Duration
	if qps > 0 {
		interval = time.Duration(float64(time.Second) / qps)
	}
	return &HostLimiter{interval: interval, buckets: make(map[string]*hostBucket)}
}

// Wait blocks until a request to host may proceed, or ctx is done. It returns
// ctx.Err() on cancellation.
//
// An already-canceled context returns without reserving a slot, and a slot
// reserved for a wait that is then canceled is rolled back when it is still the
// tail of the host's schedule, so an abandoned request does not delay the next
// real one.
func (l *HostLimiter) Wait(ctx context.Context, host string) error {
	if l.interval <= 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err // don't charge a slot to an already-canceled request
	}
	b := l.bucket(host)
	wait, reserved := b.reserve(l.interval)
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		b.rollback(reserved, l.interval)
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (l *HostLimiter) bucket(host string) *hostBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[host]
	if b == nil {
		b = &hostBucket{}
		l.buckets[host] = b
	}
	return b
}

// hostBucket tracks the earliest time the next request to one host may proceed.
type hostBucket struct {
	mu   sync.Mutex
	next time.Time
}

// reserve claims the next admission slot and returns how long the caller must
// wait before using it, plus the reserved schedule time (for a later rollback).
// Each reservation advances the schedule by interval, so concurrent callers are
// spaced even though they wait without holding the lock.
func (b *hostBucket) reserve(interval time.Duration) (wait time.Duration, reserved time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.next.Before(now) {
		b.next = now
	}
	wait = b.next.Sub(now)
	b.next = b.next.Add(interval)
	return wait, b.next
}

// rollback gives back a reservation whose wait was canceled, but only when it is
// still the tail of the schedule (no later request has reserved behind it).
// Rolling back an interior reservation would let a still-waiting later request
// proceed early, so it is left in place.
func (b *hostBucket) rollback(reserved time.Time, interval time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.next.Equal(reserved) {
		b.next = b.next.Add(-interval)
	}
}
