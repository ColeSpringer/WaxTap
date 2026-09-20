package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// DefaultRangeBlock is the size of the blocks a RangeReader fetches. It is the
// unit a demuxer's many small reads are served from: large enough that a header
// scan is one or two requests, small enough that reading a header does not
// fetch the file.
const DefaultRangeBlock = 256 << 10

// rangeBlocksHeld is how many fetched blocks a RangeReader keeps. A demuxer
// reads a header forwards and then seeks, so the working set is small; holding
// a few blocks turns a re-read of a field into no request at all, and the cap
// keeps a probe's memory to a few hundred kilobytes.
const rangeBlocksHeld = 16

// RangeStats is what a RangeReader did on the wire, for a caller reporting the
// cost of a read.
type RangeStats struct {
	Requests     int   // range requests issued, retries included
	BytesFetched int64 // bytes read off those responses
}

// RangeBudgetError reports that a reader hit its byte budget: the consumer
// asked for more of the resource than a bounded read was meant to cover, so
// reading it in blocks is no longer the cheap route. It is sticky, so the
// consumer unwinds at once rather than fetching the rest of the file on the way
// out.
type RangeBudgetError struct {
	Fetched int64
	Budget  int64
	Size    int64
}

func (e *RangeBudgetError) Error() string {
	return fmt.Sprintf("download: a ranged read of %d bytes reached its %d-byte budget (resource is %d bytes)", e.Fetched, e.Budget, e.Size)
}

// RangeUnsupportedError reports that src cannot be read in ranges: it states no
// length, or the origin answered a bounded request without honouring it. Source
// is the live Source, refreshed if a refresh happened on the way, so a caller
// falling back to a sequential fetch does not re-present a URL the origin has
// already rejected.
type RangeUnsupportedError struct {
	Source Source
	Err    error
}

func (e *RangeUnsupportedError) Error() string {
	if e.Err == nil {
		return "download: the source cannot be read in ranges"
	}
	return fmt.Sprintf("download: the source cannot be read in ranges: %v", e.Err)
}

func (e *RangeUnsupportedError) Unwrap() error { return e.Err }

// RangeReader reads a Source at arbitrary offsets by fetching aligned blocks on
// demand, so a consumer that reads a container's headers reads a few hundred
// kilobytes rather than the file.
//
// It satisfies io.ReaderAt, whose contract it keeps to the letter because a
// demuxer relies on it:
//
//   - ReadAt is safe for concurrent use. One mutex covers the cache and the
//     fetches, so concurrent readers serialize behind a block fetch, as do
//     Err and Stats; a reader waiting on the mutex cannot act on its own
//     cancellation until the fetch in front of it returns. That is the right
//     trade for a probe, which reads one header in order, and the wrong one
//     for a consumer that reads a resource from several goroutines at once.
//   - A read starting at or past Size returns (0, io.EOF); a read that reaches
//     Size returns the bytes it got and io.EOF.
//   - A block request is clamped to the resource, so no request ever runs past
//     the end.
//   - Err is the first terminal failure of the source (a spent refresh budget,
//     exhausted retries, an origin that stopped serving), never a transient
//     attempt that recovered. Once set, every later ReadAt returns it. A
//     context error is not one of these: it belongs to the caller's view, not
//     to the source, so cancelling one view leaves the others readable.
type RangeReader struct {
	d      *Downloader
	ctx    context.Context
	shared *sharedSource
	st     *rangeState
	size   int64
	block  int64
	budget int64
}

// rangeState is the part of a RangeReader its context views share: the fetched
// blocks, the sticky error, and the request tally.
type rangeState struct {
	mu     sync.Mutex
	blocks map[int64][]byte
	order  []int64
	err    error
	stats  RangeStats
}

// OpenRange opens src for ranged reading. It fetches the first block before
// returning, so a source the origin will not serve in ranges, or one whose
// first request is refused, is known before a demuxer runs rather than part way
// through one.
//
// src.ContentLength must be known: a bounded reply cannot teach it (a QueryRange
// reply is a 200 whose Content-Length is the slice, and a HeaderRange 206 would
// need Content-Range's complete length, which an origin may omit). A Source
// without one is not rangeable and comes straight back as
// *RangeUnsupportedError, as does an origin that ignored the bounded request.
//
// budget caps the total bytes fetched; 0 is unlimited. A consumer that means to
// read a header states one, so a container whose layout turns a bounded read
// into a sweep of the whole resource (a head that keeps going, block after
// block) stops early with a *RangeBudgetError instead of fetching the file one
// block at a time. refresh may be nil.
func (d *Downloader) OpenRange(ctx context.Context, src Source, refresh RefreshFunc, budget int64) (*RangeReader, error) {
	// One sharedSource per reader: the probe gets its own refresh budget and
	// its own no-progress bail rather than sharing a download's.
	shared := newSharedSource(src, refresh, d.maxRefreshes)
	if src.ContentLength <= 0 {
		return nil, &RangeUnsupportedError{Source: src, Err: fmt.Errorf("the source states no content length")}
	}
	block := int64(DefaultRangeBlock)
	if d.rangeBlock > 0 {
		block = d.rangeBlock
	}
	r := &RangeReader{
		d:      d,
		ctx:    ctx,
		shared: shared,
		st:     &rangeState{blocks: make(map[int64][]byte)},
		size:   src.ContentLength,
		block:  block,
		budget: budget,
	}
	r.st.mu.Lock()
	_, err := r.blockAt(0)
	r.st.mu.Unlock()
	if err != nil {
		// blockAt has already turned an ignored range into the unsupported
		// report, so an origin that refuses on the first block and one that
		// refuses on the fiftieth reach the caller the same way.
		return nil, err
	}
	return r, nil
}

// WithContext returns a view of r whose reads honor ctx. The two share the
// fetched blocks, the refresh state, and the mutex; only the context differs,
// so a caller may hold both.
func (r *RangeReader) WithContext(ctx context.Context) *RangeReader {
	view := *r
	view.ctx = ctx
	return &view
}

// Size is the resource's length in bytes.
func (r *RangeReader) Size() int64 { return r.size }

// Current is the Source as it stands, which is the refreshed one when a refresh
// has happened.
func (r *RangeReader) Current() Source {
	src, _ := r.shared.current()
	return src
}

// Err is the first terminal failure, or nil.
func (r *RangeReader) Err() error {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	return r.st.err
}

// Stats is what the reader has done on the wire so far.
func (r *RangeReader) Stats() RangeStats {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	return r.st.stats
}

// ReadAt implements io.ReaderAt over the block cache.
func (r *RangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("download: ReadAt at negative offset %d", off)
	}
	if len(p) == 0 {
		if off >= r.size {
			return 0, io.EOF
		}
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	// A dead context fails the read whether or not the cache could serve it, so
	// cancellation does not depend on what happens to be held.
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}

	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	if r.st.err != nil {
		return 0, r.st.err
	}

	n := 0
	for n < len(p) {
		if off+int64(n) >= r.size {
			return n, io.EOF
		}
		idx := (off + int64(n)) / r.block
		blk, err := r.blockAt(idx)
		if err != nil {
			return n, err
		}
		start := (off + int64(n)) - idx*r.block
		n += copy(p[n:], blk[start:])
	}
	if off+int64(n) >= r.size {
		return n, io.EOF
	}
	return n, nil
}

// blockAt returns block idx, fetching it if it is not held. The caller holds
// the mutex; a fetch runs under it, which is what makes concurrent readers
// share one fetch per block instead of racing for it.
func (r *RangeReader) blockAt(idx int64) ([]byte, error) {
	if blk, ok := r.st.blocks[idx]; ok {
		return blk, nil
	}
	if r.budget > 0 && r.st.stats.BytesFetched >= r.budget {
		err := &RangeBudgetError{Fetched: r.st.stats.BytesFetched, Budget: r.budget, Size: r.size}
		r.st.err = err
		return nil, err
	}
	start := idx * r.block
	end := min(start+r.block, r.size) - 1
	blk, err := r.fetchBlock(start, end)
	if err != nil {
		// An origin that ignored this bounded range will ignore the rest, so it
		// is reported as the source not being rangeable at all, whichever block
		// found out. The live Source rides along so a caller falling back to a
		// sequential fetch does not re-present a URL the origin has refreshed.
		if errors.Is(err, errRangeIgnored) {
			live, _ := r.shared.current()
			err = &RangeUnsupportedError{Source: live, Err: err}
		}
		// A context error is this view's, not the source's; see RangeReader.
		if r.ctx.Err() == nil {
			r.st.err = err
		}
		return nil, err
	}
	r.st.blocks[idx] = blk
	r.st.order = append(r.st.order, idx)
	for len(r.st.order) > rangeBlocksHeld {
		delete(r.st.blocks, r.st.order[0])
		r.st.order = r.st.order[1:]
	}
	return blk, nil
}

// fetchBlock reads [start, end] into memory, retrying a short body the way a
// chunk write does and reporting a persistently short one as an incomplete
// stream.
func (r *RangeReader) fetchBlock(start, end int64) ([]byte, error) {
	want := end - start + 1
	for attempt := 0; ; attempt++ {
		resp, err := r.d.fetchRetrying(r.ctx, r.shared, start, end)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, want)
		n, rerr := io.ReadFull(io.LimitReader(resp.Body, want), buf)
		drainClose(resp)
		r.st.stats.Requests++
		r.st.stats.BytesFetched += int64(n)
		r.shared.noteDelivered(int64(n))
		if rerr == nil {
			return buf, nil
		}
		if cerr := r.ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if attempt >= r.d.maxChunkRetries {
			return nil, fmt.Errorf("%w: short block at offset %d: got %d bytes, want %d: %w",
				waxerr.ErrIncompleteStream, start, n, want, rerr)
		}
		if berr := r.d.backoff(r.ctx, attempt); berr != nil {
			return nil, berr
		}
	}
}

// compile-time proof that a RangeReader is what a random-access consumer needs.
var _ interface {
	io.ReaderAt
	Size() int64
} = (*RangeReader)(nil)
