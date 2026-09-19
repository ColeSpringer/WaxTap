package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// rangedDownloader is newTestDownloader with a block size small enough that a
// modest fixture spans several blocks, so a test can count requests.
func rangedDownloader(block int64) *Downloader {
	d := newTestDownloader(1<<10, 2)
	d.rangeBlock = block
	return d
}

// rangeOrigin serves payload with byte ranges and counts the requests it saw.
type rangeOrigin struct {
	payload []byte
	mu      sync.Mutex
	ranges  []string
}

func (o *rangeOrigin) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		o.ranges = append(o.ranges, r.Header.Get("Range")+r.URL.Query().Get("range"))
		o.mu.Unlock()
		serveRange(w, r, o.payload)
	}
}

func (o *rangeOrigin) requests() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ranges...)
}

func newRangeReader(t *testing.T, block int64, payload []byte, h http.HandlerFunc) (*RangeReader, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d := rangedDownloader(block)
	rr, err := d.OpenRange(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, nil, 0)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	return rr, srv
}

// TestRangeReaderServesReadsFromBlocks: one read inside a block is one request,
// a straddling read two, and a repeat none. This is the whole point of the
// reader: a demuxer's many small reads come off a handful of fetches.
func TestRangeReaderServesReadsFromBlocks(t *testing.T) {
	payload := makePayload(5000)
	o := &rangeOrigin{payload: payload}
	rr, _ := newRangeReader(t, 1000, payload, o.handler())

	// OpenRange fetched block 0 eagerly.
	if got := rr.Stats().Requests; got != 1 {
		t.Fatalf("after OpenRange: %d requests, want 1 (the eager first block)", got)
	}

	buf := make([]byte, 10)
	if _, err := rr.ReadAt(buf, 100); err != nil {
		t.Fatalf("ReadAt inside block 0: %v", err)
	}
	if got := rr.Stats().Requests; got != 1 {
		t.Errorf("a read inside the held block issued %d requests, want none beyond the first", got)
	}

	// Straddling blocks 1 and 2.
	straddle := make([]byte, 200)
	if _, err := rr.ReadAt(straddle, 1900); err != nil {
		t.Fatalf("straddling ReadAt: %v", err)
	}
	if got := rr.Stats().Requests; got != 3 {
		t.Errorf("a straddling read left %d requests, want 3 (block 0 plus the two it spans)", got)
	}
	if !bytes.Equal(straddle, payload[1900:2100]) {
		t.Error("straddling read returned the wrong bytes")
	}

	// A repeat of the same span is served from the cache.
	again := make([]byte, 200)
	if _, err := rr.ReadAt(again, 1900); err != nil {
		t.Fatalf("repeat ReadAt: %v", err)
	}
	if got := rr.Stats().Requests; got != 3 {
		t.Errorf("a repeat read issued more requests (%d), want the cache to serve it", got)
	}
}

// TestRangeReaderHeaderScanIsBounded: a 5000-byte header read over 1000-byte
// blocks is five requests and then none, the shape a large moov or a Matroska
// whose Tracks element sits past the first block produces.
func TestRangeReaderHeaderScanIsBounded(t *testing.T) {
	payload := makePayload(50000)
	o := &rangeOrigin{payload: payload}
	rr, _ := newRangeReader(t, 1000, payload, o.handler())

	head := make([]byte, 5000)
	if _, err := rr.ReadAt(head, 0); err != nil {
		t.Fatalf("header read: %v", err)
	}
	if got := rr.Stats().Requests; got != 5 {
		t.Errorf("a 5000-byte header over 1000-byte blocks took %d requests, want 5", got)
	}
	if !bytes.Equal(head, payload[:5000]) {
		t.Error("header read returned the wrong bytes")
	}
	if _, err := rr.ReadAt(make([]byte, 5000), 0); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got := rr.Stats().Requests; got != 5 {
		t.Errorf("re-reading the header took %d requests, want the blocks to still be held", got)
	}
	if got, want := rr.Stats().BytesFetched, int64(5000); got != want {
		t.Errorf("fetched %d bytes for a %d-byte header, want exactly the blocks it spans", got, want)
	}
}

// TestRangeReaderClampsTheLastBlock: the final block is clamped to Size, so no
// request runs past the end, and a read that reaches Size reports io.EOF with
// the bytes it got.
func TestRangeReaderClampsTheLastBlock(t *testing.T) {
	payload := makePayload(2500) // two whole blocks and a 500-byte tail
	o := &rangeOrigin{payload: payload}
	rr, _ := newRangeReader(t, 1000, payload, o.handler())

	tail := make([]byte, 600)
	n, err := rr.ReadAt(tail, 2200)
	if n != 300 || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt near the end = %d, %v; want 300, io.EOF", n, err)
	}
	if !bytes.Equal(tail[:n], payload[2200:]) {
		t.Error("tail read returned the wrong bytes")
	}
	for _, spec := range o.requests() {
		if spec == "bytes=2000-2999" {
			t.Errorf("a block request ran past the end: %q", spec)
		}
	}
	if n, err := rr.ReadAt(make([]byte, 10), 2500); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at Size = %d, %v; want 0, io.EOF", n, err)
	}
	if n, err := rr.ReadAt(make([]byte, 10), 9999); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt past Size = %d, %v; want 0, io.EOF", n, err)
	}
}

// TestRangeReaderViewsShareOneFetchPerBlock: two WithContext views read the
// same block concurrently and the origin sees it once. The mutex is what makes
// that true, and it is also what makes ReadAt safe for concurrent use.
func TestRangeReaderViewsShareOneFetchPerBlock(t *testing.T) {
	payload := makePayload(8000)
	o := &rangeOrigin{payload: payload}
	rr, _ := newRangeReader(t, 1000, payload, o.handler())

	a := rr.WithContext(context.Background())
	b := rr.WithContext(context.Background())
	var wg sync.WaitGroup
	for _, v := range []*RangeReader{a, b, a, b} {
		wg.Add(1)
		go func(v *RangeReader) {
			defer wg.Done()
			buf := make([]byte, 100)
			if _, err := v.ReadAt(buf, 3000); err != nil {
				t.Errorf("concurrent ReadAt: %v", err)
			}
		}(v)
	}
	wg.Wait()
	// Block 0 (eager) plus block 3, fetched once however many views asked.
	if got := rr.Stats().Requests; got != 2 {
		t.Errorf("%d requests for one shared block, want 2 (the eager first plus one)", got)
	}
}

// TestRangeReaderRefreshesAFirstRequest403: the eager first block goes through
// the same ladder every fetch does, so an expired URL is renewed before a
// demuxer ever runs.
func TestRangeReaderRefreshesAFirstRequest403(t *testing.T) {
	payload := makePayload(3000)
	var codes []int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fresh := r.URL.Query().Get("fresh") == "1"
		mu.Lock()
		if fresh {
			codes = append(codes, 200)
		} else {
			codes = append(codes, 403)
		}
		mu.Unlock()
		if !fresh {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	var refreshes atomic.Int32
	refresh := func(context.Context, *potoken.HTTPFailure) (Source, error) {
		refreshes.Add(1)
		return Source{URL: srv.URL + "?fresh=1", ContentLength: int64(len(payload))}, nil
	}
	d := rangedDownloader(1000)
	rr, err := d.OpenRange(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, refresh, 0)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	if refreshes.Load() != 1 {
		t.Errorf("refreshes = %d, want 1", refreshes.Load())
	}
	buf := make([]byte, 50)
	if _, err := rr.ReadAt(buf, 10); err != nil {
		t.Fatalf("ReadAt after refresh: %v", err)
	}
	if !bytes.Equal(buf, payload[10:60]) {
		t.Error("post-refresh read returned the wrong bytes")
	}
	if got := rr.Current().URL; got != srv.URL+"?fresh=1" {
		t.Errorf("Current().URL = %q, want the refreshed URL", got)
	}
}

// TestOpenRangeRefusesAnUnrangeableSource covers the three shapes that mean
// "not rangeable": no stated length, a HeaderRange origin that answers 200, and
// a QueryRange origin whose body is not the slice asked for. Each comes back as
// *RangeUnsupportedError without burning retries, so the caller falls straight
// back to a sequential fetch.
func TestOpenRangeRefusesAnUnrangeableSource(t *testing.T) {
	payload := makePayload(4000)

	t.Run("no content length", func(t *testing.T) {
		d := rangedDownloader(1000)
		_, err := d.OpenRange(context.Background(), Source{URL: "http://127.0.0.1:0/x"}, nil, 0)
		if _, ok := errors.AsType[*RangeUnsupportedError](err); !ok {
			t.Fatalf("err = %v, want *RangeUnsupportedError", err)
		}
	})

	t.Run("header range answered 200", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
		}))
		defer srv.Close()
		d := rangedDownloader(1000)
		_, err := d.OpenRange(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, nil, 0)
		ue, ok := errors.AsType[*RangeUnsupportedError](err)
		if !ok {
			t.Fatalf("err = %v, want *RangeUnsupportedError", err)
		}
		if !errors.Is(err, errRangeIgnored) {
			t.Errorf("err = %v, want it to carry errRangeIgnored", err)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("origin saw %d requests, want 1: an ignored range earns no retry", got)
		}
		if ue.Source.URL != srv.URL {
			t.Errorf("Source.URL = %q, want the live source", ue.Source.URL)
		}
	})

	t.Run("query range wrong length", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			// A 200 whose body is the whole file: the range parameter was ignored.
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(payload)
		}))
		defer srv.Close()
		d := rangedDownloader(1000)
		src := Source{URL: srv.URL, ContentLength: int64(len(payload)), RangeStrategy: QueryRange{}}
		_, err := d.OpenRange(context.Background(), src, nil, 0)
		if _, ok := errors.AsType[*RangeUnsupportedError](err); !ok {
			t.Fatalf("err = %v, want *RangeUnsupportedError", err)
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("origin saw %d requests, want 1", got)
		}
	})
}

// TestRangeReaderReportsALaterIgnoredRangeAsUnsupported: an origin that serves
// the first block and then answers a later one with the whole body is refusing
// ranges just as surely as one that refuses the first, and a caller with a
// sequential fallback has to be able to tell. The report carries the live
// Source either way.
func TestRangeReaderReportsALaterIgnoredRangeAsUnsupported(t *testing.T) {
	payload := makePayload(8000)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			serveRange(w, r, payload) // the first block is honoured
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK) // and the next is not
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := rangedDownloader(1000)
	rr, err := d.OpenRange(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, nil, 0)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	_, err = rr.ReadAt(make([]byte, 50), 3500)
	ue, ok := errors.AsType[*RangeUnsupportedError](err)
	if !ok {
		t.Fatalf("err = %v, want *RangeUnsupportedError", err)
	}
	if ue.Source.URL != srv.URL {
		t.Errorf("Source.URL = %q, want the live source", ue.Source.URL)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("origin saw %d requests, want 2: an ignored range earns no retry", got)
	}
	if _, ok := errors.AsType[*RangeUnsupportedError](rr.Err()); !ok {
		t.Errorf("Err() = %v, want the sticky refusal", rr.Err())
	}
}

// TestRangeReaderEvictsPastItsHeldBlocks: a reader with no budget still bounds
// what it holds, so a consumer that walks a long resource cannot grow the cache
// to the size of the file. The evicted block is re-fetched if it is read again.
func TestRangeReaderEvictsPastItsHeldBlocks(t *testing.T) {
	blocks := rangeBlocksHeld + 4
	payload := makePayload(1000 * blocks)
	o := &rangeOrigin{payload: payload}
	rr, _ := newRangeReader(t, 1000, payload, o.handler())

	for i := range blocks {
		if _, err := rr.ReadAt(make([]byte, 10), int64(i*1000)); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
	}
	if got := rr.Stats().Requests; got != blocks {
		t.Fatalf("%d requests for %d blocks, want one each", got, blocks)
	}
	// Block 0 was evicted four blocks ago, so reading it again refetches.
	if _, err := rr.ReadAt(make([]byte, 10), 0); err != nil {
		t.Fatalf("re-read of the evicted block: %v", err)
	}
	if got := rr.Stats().Requests; got != blocks+1 {
		t.Errorf("%d requests, want the evicted block refetched", got)
	}
	// The most recent block is still held.
	if _, err := rr.ReadAt(make([]byte, 10), int64((blocks-1)*1000)); err != nil {
		t.Fatalf("re-read of a held block: %v", err)
	}
	if got := rr.Stats().Requests; got != blocks+1 {
		t.Errorf("%d requests, want the held block served from the cache", got)
	}
}

// TestOpenRangeRefusesAChunkedIgnoredRange: an origin that ignores the range
// parameter and answers without a length would otherwise hand back the file's
// head for every block, and the reader would cache each one under a different
// offset. There is nothing to detect that after the fact, so the reply is
// refused at validation.
func TestOpenRangeRefusesAChunkedIgnoredRange(t *testing.T) {
	payload := makePayload(20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length, flushed so Go sends it chunked: the whole file
		// under a request for one block of it.
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := rangedDownloader(1000)
	src := Source{URL: srv.URL, ContentLength: int64(len(payload)), RangeStrategy: QueryRange{}}
	_, err := d.OpenRange(context.Background(), src, nil, 0)
	if _, ok := errors.AsType[*RangeUnsupportedError](err); !ok {
		t.Fatalf("err = %v, want *RangeUnsupportedError: an unmeasurable bounded reply is not a block", err)
	}
	if !errors.Is(err, errRangeIgnored) {
		t.Errorf("err = %v, want it to carry errRangeIgnored", err)
	}
}

// TestRangeReaderShortBlockIsIncomplete: a body that stops early is retried and
// then reported as an incomplete stream, the class that sends a download to
// another client rather than to the user as a bad file.
func TestRangeReaderShortBlockIsIncomplete(t *testing.T) {
	payload := makePayload(4000)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := parseTestRange(rangeSpecOf(r), int64(len(payload)))
		if start == 0 {
			serveRange(w, r, payload) // the eager first block is whole
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : start+10]) // short
	}))
	defer srv.Close()

	d := rangedDownloader(1000)
	rr, err := d.OpenRange(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, nil, 0)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	_, err = rr.ReadAt(make([]byte, 50), 1500)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", err)
	}
	if got := hits.Load(); got < 2 {
		t.Errorf("origin saw %d requests for the short block, want it retried", got)
	}
	// The failure sticks: a later read reports the same thing rather than
	// re-attempting a source that has already proved it will not deliver.
	if _, again := rr.ReadAt(make([]byte, 10), 2500); !errors.Is(again, waxerr.ErrIncompleteStream) {
		t.Errorf("second ReadAt = %v, want the sticky failure", again)
	}
	if !errors.Is(rr.Err(), waxerr.ErrIncompleteStream) {
		t.Errorf("Err() = %v, want the terminal failure", rr.Err())
	}
}

// TestRangeReaderStopsAtItsBudget: a consumer that keeps asking for more of the
// resource than a bounded read was meant to cover is stopped rather than left
// to fetch the file one block at a time, and the refusal sticks so it unwinds
// at once.
func TestRangeReaderStopsAtItsBudget(t *testing.T) {
	payload := makePayload(20000)
	o := &rangeOrigin{payload: payload}
	srv := httptest.NewServer(o.handler())
	defer srv.Close()
	d := rangedDownloader(1000)
	rr, err := d.OpenRange(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, nil, 3000)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}

	// Blocks 0, 1 and 2 fit the 3000-byte budget; the fourth is refused.
	if _, err := rr.ReadAt(make([]byte, 2500), 0); err != nil {
		t.Fatalf("read inside the budget: %v", err)
	}
	_, err = rr.ReadAt(make([]byte, 100), 3500)
	over, ok := errors.AsType[*RangeBudgetError](err)
	if !ok {
		t.Fatalf("err = %v, want *RangeBudgetError", err)
	}
	if over.Budget != 3000 || over.Size != int64(len(payload)) {
		t.Errorf("budget error = %+v, want budget 3000 over a %d-byte resource", over, len(payload))
	}
	if got := rr.Stats().BytesFetched; got > 3000 {
		t.Errorf("fetched %d bytes past a 3000-byte budget", got)
	}
	// Sticky, including for a block already held: the consumer is unwinding.
	if _, again := rr.ReadAt(make([]byte, 10), 0); !errors.Is(again, err) {
		t.Errorf("second ReadAt = %v, want the sticky budget refusal", again)
	}
	if _, ok := errors.AsType[*RangeBudgetError](rr.Err()); !ok {
		t.Errorf("Err() = %v, want the budget refusal", rr.Err())
	}
}

// TestRangeReaderHonorsCancellation: a cancelled context stops the read and
// reports the cancellation, not a transport error dressed up as bad input.
func TestRangeReaderHonorsCancellation(t *testing.T) {
	payload := makePayload(4000)
	o := &rangeOrigin{payload: payload}
	rr, _ := newRangeReader(t, 1000, payload, o.handler())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	view := rr.WithContext(ctx)
	if _, err := view.ReadAt(make([]byte, 50), 2500); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// The cancellation was this view's, not the source's, so the original view
	// still reads.
	if _, err := rr.ReadAt(make([]byte, 50), 10); err != nil {
		t.Errorf("read after another view's cancellation: %v", err)
	}
	if rr.Err() != nil {
		t.Errorf("Err() = %v, want nil: a cancelled view does not fail the source", rr.Err())
	}
	if _, err := rr.ReadAt(make([]byte, 50), 3500); err != nil {
		t.Errorf("fetch after another view's cancellation: %v", err)
	}
}

// rangeSpecOf reports the requested range whichever dialect asked for it.
func rangeSpecOf(r *http.Request) string {
	if q := r.URL.Query().Get("range"); q != "" {
		return q
	}
	h := r.Header.Get("Range")
	if len(h) > 6 && h[:6] == "bytes=" {
		return h[6:]
	}
	return h
}
