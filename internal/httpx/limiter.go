package httpx

import (
	"context"
	"sync"
	"time"
)

// HostLimiter spaces requests and applies cooldowns independently per host.
//
// Requests are admitted at most once every 1/qps seconds; bursts are not allowed.
// [HostLimiter.Penalize] pauses a host even when qps is non-positive.
type HostLimiter struct {
	interval time.Duration

	mu      sync.Mutex
	buckets map[string]*hostBucket
}

// NewHostLimiter returns a per-host limiter that admits qps requests per second.
// A non-positive qps disables spacing but still permits cooldowns.
func NewHostLimiter(qps float64) *HostLimiter {
	var interval time.Duration
	if qps > 0 {
		interval = time.Duration(float64(time.Second) / qps)
	}
	return &HostLimiter{interval: interval, buckets: make(map[string]*hostBucket)}
}

// Wait blocks until a request to host may proceed or ctx is done.
//
// Wait rechecks the host cooldown after each timer wakeup. On cancellation, it
// rolls back the request's spacing reservation when no later request depends on
// it.
func (l *HostLimiter) Wait(ctx context.Context, host string) error {
	if err := ctx.Err(); err != nil {
		return err // do not reserve a slot for an already-canceled request
	}
	b := l.bucket(host)
	slot, reserved := b.reserve(l.interval)
	for {
		wait := b.admitDelay(slot)
		if wait <= 0 {
			return nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			b.rollback(reserved, l.interval)
			return ctx.Err()
		case <-t.C:
			// A cooldown may have changed while the timer was running.
		}
	}
}

// Penalize pauses requests to host for at least d. It also moves the spacing
// schedule forward so requests remain spaced after the cooldown. A non-positive
// duration has no effect.
func (l *HostLimiter) Penalize(host string, d time.Duration) {
	if d <= 0 {
		return
	}
	b := l.bucket(host)
	b.mu.Lock()
	defer b.mu.Unlock()
	until := time.Now().Add(d)
	if until.After(b.cooldownUntil) {
		b.cooldownUntil = until
	}
	if until.After(b.next) {
		b.next = until
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

// hostBucket stores one host's spacing schedule and cooldown.
type hostBucket struct {
	mu            sync.Mutex
	next          time.Time // earliest time the next QPS-spaced request may proceed
	cooldownUntil time.Time // host paused until this time (Penalize)
}

// reserve returns the next admission time and the new schedule tail.
func (b *hostBucket) reserve(interval time.Duration) (slot, reserved time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	base := time.Now()
	if b.next.After(base) {
		base = b.next
	}
	if b.cooldownUntil.After(base) {
		base = b.cooldownUntil
	}
	b.next = base.Add(interval)
	return base, b.next
}

// admitDelay returns the remaining wait for a slot and the current cooldown.
func (b *hostBucket) admitDelay(slot time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	admit := slot
	if b.cooldownUntil.After(admit) {
		admit = b.cooldownUntil
	}
	return time.Until(admit)
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
