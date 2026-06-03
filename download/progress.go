package download

import (
	"io"
	"sync"
)

// progressEmitInterval is the minimum byte delta between ProgressFunc calls.
// Body copies use small reads, and the callback runs synchronously on the
// worker, so progress is coalesced to this granularity.
const progressEmitInterval = 256 << 10 // 256 KiB

// progressReporter accumulates bytes delivered to a sink and emits throttled
// Progress snapshots. It is safe for concurrent use so parallel chunk workers
// can report into one reporter. The reported count is clamped to [0, total] to
// keep retries from pushing progress past the known size.
type progressReporter struct {
	fn ProgressFunc

	mu          sync.Mutex
	total       int64
	written     int64
	lastEmitted int64
}

func newProgress(fn ProgressFunc, total int64) *progressReporter {
	return &progressReporter{fn: fn, total: total}
}

// setTotal records a total length learned after construction (for example from a
// Content-Range header when the Source did not carry a content length).
func (p *progressReporter) setTotal(total int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.total = total
	p.mu.Unlock()
}

// add records a byte delta (negative to roll back a failed chunk) and emits a
// snapshot when enough progress has accumulated since the last emission.
func (p *progressReporter) add(delta int64) {
	if p == nil || p.fn == nil || delta == 0 {
		return
	}
	p.mu.Lock()
	p.written += delta
	if p.written < 0 {
		p.written = 0
	}
	d := p.written - p.lastEmitted
	if d < progressEmitInterval && d > -progressEmitInterval {
		p.mu.Unlock()
		return
	}
	snap := p.snapshotLocked()
	p.lastEmitted = p.written
	p.mu.Unlock()
	p.fn(snap)
}

// flush emits a final snapshot if anything new has accumulated since the last
// emission. It is idempotent, so callers (and the reader on EOF) may both flush
// without producing a duplicate terminal event.
func (p *progressReporter) flush() {
	if p == nil || p.fn == nil {
		return
	}
	p.mu.Lock()
	if p.written == p.lastEmitted {
		p.mu.Unlock()
		return
	}
	snap := p.snapshotLocked()
	p.lastEmitted = p.written
	p.mu.Unlock()
	p.fn(snap)
}

// snapshotLocked builds a Progress, clamping the count to the known total. The
// caller must hold p.mu.
func (p *progressReporter) snapshotLocked() Progress {
	written := p.written
	if p.total > 0 && written > p.total {
		written = p.total
	}
	return Progress{BytesWritten: written, Total: p.total}
}

// countingWriter forwards writes to w while reporting their length to a
// progressReporter. It lets io.Copy drive both the sink and progress in one pass.
type countingWriter struct {
	w   io.Writer
	rep *progressReporter
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.rep.add(int64(n))
	}
	return n, err
}
