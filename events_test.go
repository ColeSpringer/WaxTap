package waxtap

import (
	"sync"
	"testing"
	"time"

	"github.com/colespringer/waxtap/internal/httpx"
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
