package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/potoken"
)

// noHTTPRetryDownloader lets retryable statuses reach the download layer by
// disabling httpx retries.
func noHTTPRetryDownloader(chunkSize int64, parallelism int) *Downloader {
	return New(Config{
		HTTPClient:      httpx.New(httpx.Config{MaxRetries: -1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
		ChunkSize:       chunkSize,
		Parallelism:     parallelism,
		MaxChunkRetries: 3,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	})
}

// fail503Once returns 503 for the first request, then serves normally.
type fail503Once struct {
	payload []byte
	failed  atomic.Bool
}

func (o *fail503Once) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if o.failed.CompareAndSwap(false, true) {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	serveRange(w, r, o.payload)
}

func TestToFile_RetriesTransientStatus(t *testing.T) {
	payload := makePayload(8000)
	srv := httptest.NewServer(&fail503Once{payload: payload})
	defer srv.Close()

	d := noHTTPRetryDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after transient retry")
	}
}

// shortOnce sends a truncated body (fewer bytes than the declared length) for
// the first ranged request, so io.Copy sees a short chunk and retries it.
type shortOnce struct {
	payload []byte
	short   atomic.Bool
}

func (o *shortOnce) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	total := int64(len(o.payload))
	if h := r.Header.Get("Range"); h != "" && o.short.CompareAndSwap(false, true) {
		start, end := parseTestRange(strings.TrimPrefix(h, "bytes="), total)
		full := end - start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(full, 10))
		w.WriteHeader(http.StatusPartialContent)
		short := full - 2
		if short < 0 {
			short = 0
		}
		_, _ = w.Write(o.payload[start : start+short]) // truncated: client sees a short chunk
		return
	}
	serveRange(w, r, o.payload)
}

func TestToFile_RetriesShortChunk(t *testing.T) {
	payload := makePayload(8000)
	srv := httptest.NewServer(&shortOnce{payload: payload})
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after short-chunk retry")
	}
}

// stallOrigin declares a length but never sends a byte, so every (re)open makes
// no progress. The reader's stall guard must give up rather than loop forever.
type stallOrigin struct{ size int }

func (o *stallOrigin) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", strconv.Itoa(o.size))
	w.WriteHeader(http.StatusOK)
	// Write nothing and return: the client expects o.size bytes and gets a
	// premature EOF at offset 0 on every attempt.
}

func TestStream_StallGuardTerminates(t *testing.T) {
	srv := httptest.NewServer(&stallOrigin{size: 4096})
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	var buf bytes.Buffer
	src := Source{URL: srv.URL, ContentLength: 4096}

	done := make(chan error, 1)
	go func() {
		_, err := d.ToWriter(context.Background(), src, &buf, nil, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stalled") {
			t.Fatalf("err = %v, want a stall error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stall guard did not terminate the download")
	}
}

// TestToFile_AlreadyCanceledContext covers cancellation before any worker starts
// a fetch. downloadChunks must return the context error, not nil.
func TestToFile_AlreadyCanceledContext(t *testing.T) {
	payload := makePayload(8000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4) // parallel path: 8 spans
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the download starts

	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	_, err := d.ToFile(ctx, src, path, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("output file should not exist after a canceled download")
	}
	assertNoTempFiles(t, dir)
}

// TestToFile_ChunkTimeoutRetries covers a per-chunk timeout while the parent
// context is still live. That timeout should use the retry budget.
func TestToFile_ChunkTimeoutRetries(t *testing.T) {
	payload := makePayload(8000)
	var stalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stalled.CompareAndSwap(false, true) {
			// First request stalls past the per-chunk timeout; the client aborts
			// this attempt and the chunk must retry, not give up.
			select {
			case <-r.Context().Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := New(Config{
		HTTPClient:      httpx.New(httpx.Config{MaxRetries: -1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
		ChunkSize:       1000,
		Parallelism:     4,
		ChunkTimeout:    80 * time.Millisecond,
		MaxChunkRetries: 3,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if !stalled.Load() {
		t.Fatal("expected the first request to stall (test did not exercise the timeout)")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after chunk-timeout retry")
	}
}

// refreshThenTransient returns 403 on the first request, then transient 503s,
// then serves normally.
type refreshThenTransient struct {
	payload   []byte
	transient int32
	n         atomic.Int32
}

func (o *refreshThenTransient) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch c := o.n.Add(1); {
	case c == 1:
		w.WriteHeader(http.StatusForbidden)
	case c <= 1+o.transient:
		w.WriteHeader(http.StatusServiceUnavailable)
	default:
		serveRange(w, r, o.payload)
	}
}

// TestToWriter_RefreshDoesNotConsumeRetryBudget covers a refresh followed by
// exactly MaxChunkRetries transient failures, then success.
func TestToWriter_RefreshDoesNotConsumeRetryBudget(t *testing.T) {
	payload := makePayload(3000)
	origin := &refreshThenTransient{payload: payload, transient: 2} // == MaxChunkRetries below
	srv := httptest.NewServer(origin)
	defer srv.Close()

	var refreshes atomic.Int32
	refresh := func(context.Context, *potoken.HTTPFailure) (Source, error) {
		refreshes.Add(1)
		return Source{URL: srv.URL, ContentLength: int64(len(payload))}, nil
	}

	// httpx retries disabled so each 503 reaches the download layer.
	d := New(Config{
		HTTPClient:      httpx.New(httpx.Config{MaxRetries: -1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
		ChunkSize:       1000,
		Parallelism:     4,
		MaxChunkRetries: 2,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	})
	var buf bytes.Buffer
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToWriter(context.Background(), src, &buf, refresh, nil); err != nil {
		t.Fatalf("ToWriter: %v", err)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh called %d times, want 1", got)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatal("output mismatch")
	}
}
