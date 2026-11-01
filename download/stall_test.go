package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// stallDownloader is newTestDownloader with a stall timeout and httpx retries
// off, so every retry a test counts is the download layer's own.
func stallDownloader(chunkSize int64, parallelism int, stall time.Duration) *Downloader {
	return New(Config{
		HTTPClient:      httpx.New(httpx.Config{MaxRetries: -1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
		ChunkSize:       chunkSize,
		Parallelism:     parallelism,
		StallTimeout:    stall,
		MaxChunkRetries: 2,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	})
}

// hold answers a ranged request with its headers and the first partial bytes of
// the span, then holds the connection open sending nothing more until the client
// gives up. With headers false nothing at all is sent, which is a server that
// accepted the connection and never answered.
func hold(w http.ResponseWriter, r *http.Request, payload []byte, partial int, headers bool) {
	if headers {
		total := int64(len(payload))
		start, end := parseTestRange(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), total)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if partial > 0 {
			_, _ = w.Write(payload[start : start+int64(partial)])
		}
		w.(http.Flusher).Flush()
	}
	<-r.Context().Done()
}

// A chunk whose body stops arriving is abandoned after the idle window and
// resumed after the bytes already written, not restarted from the span's start.
func TestToFile_StallResumesChunk(t *testing.T) {
	payload := makePayload(8000)
	var held, resumed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch rng := r.Header.Get("Range"); {
		case rng == "bytes=2000-2999" && held.CompareAndSwap(false, true):
			hold(w, r, payload, 400, true) // 400 bytes, then silence
			return
		case rng == "bytes=2400-2999":
			resumed.Store(true)
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := stallDownloader(1000, 4, 100*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if !held.Load() {
		t.Fatal("the test did not exercise the stall")
	}
	if !resumed.Load() {
		t.Fatal("the stalled chunk was not resumed from the bytes already received")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after a resumed chunk")
	}
	assertNoTempFiles(t, dir)
}

// A link slower than any whole-chunk deadline would allow still finishes: the
// window bounds the wait for the next byte, not the chunk.
func TestToFile_SlowLinkFinishes(t *testing.T) {
	payload := makePayload(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		total := int64(len(payload))
		start, end := parseTestRange(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), total)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		f := w.(http.Flusher)
		for off := start; off <= end; off += 10 {
			time.Sleep(10 * time.Millisecond)
			if _, err := w.Write(payload[off:min(off+10, end+1)]); err != nil {
				return
			}
			f.Flush()
		}
	}))
	defer srv.Close()

	// Each 250-byte chunk drips in over about 250 ms, well past the 100 ms
	// window, and never pauses for more than 10 ms.
	d := stallDownloader(250, 4, 100*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch on a slow link")
	}
}

// A chunk that never delivers another byte exhausts the no-progress budget and
// is an incomplete delivery, with the stall named.
func TestToFile_HungChunkIsIncomplete(t *testing.T) {
	payload := makePayload(8000)
	var hung atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-999" {
			hung.Add(1)
			hold(w, r, payload, 0, true)
			return
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := stallDownloader(1000, 2, 100*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, nil, nil)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", err)
	}
	if !strings.Contains(err.Error(), "sent nothing for 100ms") {
		t.Errorf("err = %q, want it to name the stall", err)
	}
	// MaxChunkRetries 2: three attempts with nothing delivered, then give up.
	if got := hung.Load(); got != 3 {
		t.Errorf("hung chunk requested %d times, want 3", got)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("no output file should remain after an incomplete download")
	}
	assertNoTempFiles(t, dir)
}

// A server that accepts the connection and never answers is abandoned after
// the idle window and the request retried, inside the chunk's retry budget.
func TestToFile_HungResponseIsRetried(t *testing.T) {
	payload := makePayload(8000)
	var held atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if held.CompareAndSwap(false, true) {
			hold(w, r, payload, 0, false)
			return
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := stallDownloader(1000, 4, 100*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if !held.Load() {
		t.Fatal("the test did not exercise the stall")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after a retried request")
	}
}

// A server that never answers at all is a timeout, not an incomplete delivery:
// nothing was delivered for another client to finish.
func TestToFile_HungResponseIsATimeout(t *testing.T) {
	payload := makePayload(8000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hold(w, r, payload, 0, false)
	}))
	defer srv.Close()

	d := stallDownloader(1000, 2, 100*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline-class error", err)
	}
	if errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want no ErrIncompleteStream: nothing was delivered", err)
	}
	if !strings.Contains(err.Error(), "did not answer within 100ms") {
		t.Errorf("err = %q, want it to name the stall", err)
	}
	assertNoTempFiles(t, dir)
}

// The sequential path resumes a stalled stream from its offset.
func TestToWriter_StallResumesFromOffset(t *testing.T) {
	payload := makePayload(3000)
	var held, resumed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if held.CompareAndSwap(false, true) {
			hold(w, r, payload, 1500, true)
			return
		}
		if r.Header.Get("Range") == "bytes=1500-" {
			resumed.Store(true)
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := stallDownloader(1<<20, 4, 100*time.Millisecond)
	var buf bytes.Buffer
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToWriter(context.Background(), src, &buf, nil, nil); err != nil {
		t.Fatalf("ToWriter: %v", err)
	}
	if !resumed.Load() {
		t.Fatal("the stream was not resumed from the bytes already delivered")
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("output mismatch after a resumed stream")
	}
}

// A ranged block whose body stalls is resumed after the bytes it already has,
// the way a chunk is, so the wire cost is the block once.
func TestRangeReader_StallResumesBlock(t *testing.T) {
	payload := makePayload(4096)
	var held atomic.Bool
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if held.CompareAndSwap(false, true) {
			hold(w, r, payload, 600, true)
			return
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := stallDownloader(1<<10, 2, 100*time.Millisecond)
	d.rangeBlock = 1024
	rr, err := d.OpenRange(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, nil, 0)
	if err != nil {
		t.Fatalf("OpenRange: %v", err)
	}
	buf := make([]byte, 1024)
	if _, err := rr.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, payload[:1024]) {
		t.Fatal("block content mismatch after a resumed block")
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	want := []string{"bytes=0-1023", "bytes=600-1023"}
	if !slices.Equal(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
	if st := rr.Stats(); st.Requests != 2 || st.BytesFetched != 1024 {
		t.Errorf("Stats = %+v, want 2 requests fetching 1024 bytes in all", st)
	}
}

// cutShort answers every ranged request with the full length declared but only
// the first `serve` bytes of the span written, then closes: a clean short body
// that moved the transfer forward.
type cutShort struct {
	payload []byte
	serve   int64
	mu      sync.Mutex
	times   map[string][]time.Time // request times, keyed by the span's end offset
}

func (o *cutShort) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	total := int64(len(o.payload))
	start, end := parseTestRange(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), total)
	o.mu.Lock()
	key := strconv.FormatInt(end, 10)
	o.times[key] = append(o.times[key], time.Now())
	o.mu.Unlock()
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(o.payload[start:min(start+o.serve, end+1)])
}

// A resume after progress still waits one backoff step before asking again, so
// an origin that cuts every response short is not hammered with back-to-back
// requests.
func TestToFile_ResumeAfterProgressBacksOff(t *testing.T) {
	payload := makePayload(800)
	origin := &cutShort{payload: payload, serve: 100, times: map[string][]time.Time{}}
	srv := httptest.NewServer(origin)
	defer srv.Close()

	const step = 40 * time.Millisecond
	d := New(Config{
		HTTPClient:      httpx.New(httpx.Config{MaxRetries: -1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
		ChunkSize:       400,
		Parallelism:     2,
		StallTimeout:    time.Second,
		MaxChunkRetries: 2,
		BaseBackoff:     step,
		MaxBackoff:      step,
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after resumed spans")
	}
	origin.mu.Lock()
	defer origin.mu.Unlock()
	for span, ts := range origin.times {
		if len(ts) != 4 {
			t.Errorf("span ending %s: %d requests, want 4 (100 bytes each)", span, len(ts))
		}
		for i := 1; i < len(ts); i++ {
			if gap := ts[i].Sub(ts[i-1]); gap < step-5*time.Millisecond {
				t.Errorf("span ending %s: resume %d came %v after the previous request, want at least %v", span, i, gap, step)
			}
		}
	}
}

// A request that fails and a body that adds nothing spend one budget between
// them, so the request ladder and the resume loop cannot each run their full
// length in turn.
func TestToWriter_RequestAndBodyFailuresShareOneBudget(t *testing.T) {
	payload := makePayload(3000)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1, 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			// Answers, then closes without a byte: a body that added nothing.
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
		default:
			serveRange(w, r, payload)
		}
	}))
	defer srv.Close()

	d := stallDownloader(1<<20, 4, time.Second) // MaxChunkRetries 2: three failed attempts in all
	var buf bytes.Buffer
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	_, err := d.ToWriter(context.Background(), src, &buf, nil, nil)
	if err == nil {
		t.Fatal("ToWriter succeeded, want the budget spent after three failed attempts")
	}
	if got := n.Load(); got != 3 {
		t.Errorf("requests = %d, want 3: the two ladders share one budget", got)
	}
	// Nothing was delivered, so the last failure is reported as itself.
	if hse, ok := errors.AsType[*waxerr.HTTPStatusError](err); !ok || hse.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("err = %v, want the final 503", err)
	}
}

// Once bytes have landed, running out of attempts is an incomplete delivery
// whatever the last failure was: the stream ended before completion, and
// another client may finish it.
func TestToFile_HungResumeAfterBytesIsIncomplete(t *testing.T) {
	payload := makePayload(8000)
	var first atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch rng := r.Header.Get("Range"); {
		case rng == "bytes=0-999" && first.CompareAndSwap(false, true):
			hold(w, r, payload, 400, true) // 400 bytes, then the body stalls
		case strings.HasPrefix(rng, "bytes=400-"):
			hold(w, r, payload, 0, false) // every resume hangs before answering
		default:
			serveRange(w, r, payload)
		}
	}))
	defer srv.Close()

	d := stallDownloader(1000, 2, 100*time.Millisecond)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, nil, nil)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream: bytes had been delivered", err)
	}
	if !strings.Contains(err.Error(), "did not answer within 100ms") {
		t.Errorf("err = %q, want the stall kept as the cause", err)
	}
	assertNoTempFiles(t, dir)
}

// tailErrTransport hands back every body with its final bytes accompanied by a
// non-EOF error, as an HTTP/2 abort or a cancelled request can.
type tailErrTransport struct{ inner http.RoundTripper }

var errTail = errors.New("connection reset after the final bytes")

func (t tailErrTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	data, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	resp.Body = &tailErrBody{data: data}
	return resp, nil
}

type tailErrBody struct{ data []byte }

func (b *tailErrBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) == 0 {
		return n, errTail
	}
	return n, nil
}

func (*tailErrBody) Close() error { return nil }

// tailErrDownloader fetches through tailErrTransport.
func tailErrDownloader(chunkSize int64, parallelism int) *Downloader {
	return New(Config{
		HTTPClient: httpx.New(httpx.Config{
			HTTPClient:  &http.Client{Transport: tailErrTransport{inner: &http.Transport{}}},
			MaxRetries:  -1,
			BaseBackoff: time.Millisecond,
			MaxBackoff:  2 * time.Millisecond,
		}),
		ChunkSize:       chunkSize,
		Parallelism:     parallelism,
		MaxChunkRetries: 2,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	})
}

// A span whose last bytes arrive together with an error is complete: nothing
// is resumed past its end.
func TestToFile_FinalBytesWithAnErrorCompleteTheChunk(t *testing.T) {
	payload := makePayload(4000)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := tailErrDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch")
	}
	if got := requests.Load(); got != 4 {
		t.Errorf("requests = %d, want 4: a complete span is not resumed", got)
	}
}

// The sequential path is the same: a known total reached is the end, whatever
// error rode along with the final bytes.
func TestToWriter_FinalBytesWithAnErrorCompleteTheStream(t *testing.T) {
	payload := makePayload(3000)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := tailErrDownloader(1<<20, 4)
	var buf bytes.Buffer
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToWriter(context.Background(), src, &buf, nil, nil); err != nil {
		t.Fatalf("ToWriter: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("output mismatch")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: a complete stream is not resumed", got)
	}
}

// failingWriterAt refuses every write, as a full disk does.
type failingWriterAt struct{ err error }

func (f failingWriterAt) WriteAt([]byte, int64) (int, error) { return 0, f.err }

// A sink that cannot be written to is not a transfer problem a retry can fix,
// so the chunk fails on the spot with the write named.
func TestDownloadChunks_WriteErrorIsTerminal(t *testing.T) {
	payload := makePayload(1000)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	shared := newSharedSource(Source{URL: srv.URL, ContentLength: 1000}, nil, 0)
	boom := errors.New("disk full")
	err := d.downloadChunks(context.Background(), shared, failingWriterAt{err: boom}, planChunks(1000, 1000), newProgress(nil, 1000))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the sink's error", err)
	}
	if errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Errorf("err = %v, want no ErrIncompleteStream: the source was fine", err)
	}
	if !strings.Contains(err.Error(), "write chunk") {
		t.Errorf("err = %q, want it to name the write", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1: a failing sink is not retried", got)
	}
}

func TestNew_StallTimeoutDefaultsAndDisables(t *testing.T) {
	hc := httpx.New(httpx.Config{})
	if d := New(Config{HTTPClient: hc}); d.stallTimeout != httpx.DefaultStallTimeout {
		t.Errorf("zero StallTimeout: stallTimeout = %v, want the default %v", d.stallTimeout, httpx.DefaultStallTimeout)
	}
	if d := New(Config{HTTPClient: hc, StallTimeout: -1}); d.stallTimeout != 0 {
		t.Errorf("negative StallTimeout: stallTimeout = %v, want 0 (disabled)", d.stallTimeout)
	}
	if d := New(Config{HTTPClient: hc, StallTimeout: time.Minute}); d.stallTimeout != time.Minute {
		t.Errorf("StallTimeout = %v, want 1m", d.stallTimeout)
	}
}
