package download

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2/potoken"
	"github.com/colespringer/waxtap/v2/waxerr"
)

// tokenOrigin serves payload but rejects requests whose ?tok= is not currently
// valid, modeling a signed URL that has expired.
type tokenOrigin struct {
	payload []byte
	mu      sync.Mutex
	valid   map[string]bool
	hits    map[string]int
}

func newTokenOrigin(payload []byte, valid map[string]bool) *tokenOrigin {
	return &tokenOrigin{payload: payload, valid: valid, hits: map[string]int{}}
}

func (o *tokenOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("tok")
	o.mu.Lock()
	o.hits[tok]++
	ok := o.valid[tok]
	o.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	serveRange(w, r, o.payload)
}

func (o *tokenOrigin) hitCount(tok string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hits[tok]
}

// assertNoTempFiles fails if any sibling *.part temp files remain, verifying
// atomic-temp cleanup on success, failure, and cancellation.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	if len(matches) != 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}

func TestToFile_ParallelChunks(t *testing.T) {
	payload := makePayload(8000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4) // 8 chunks across 4 workers
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	res, err := d.ToFile(context.Background(), Source{URL: srv.URL, ContentLength: int64(len(payload))}, path, nil, nil)
	if err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("output mismatch (len got=%d want=%d)", len(got), len(payload))
	}
	assertNoTempFiles(t, dir)
}

func TestToFile_QueryRangeStrategy(t *testing.T) {
	payload := makePayload(5000)
	var sawQuery atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("range") != "" {
			sawQuery.Store(true)
		}
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 3)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL, ContentLength: int64(len(payload)), RangeStrategy: QueryRange{}}
	if _, err := d.ToFile(context.Background(), src, path, nil, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if !sawQuery.Load() {
		t.Fatal("expected at least one &range= query request")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch with QueryRange")
	}
}

func TestToFile_SingleStreamUnknownLength(t *testing.T) {
	payload := makePayload(4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	// ContentLength 0 forces the single-stream fallback (no parallel chunks).
	res, err := d.ToFile(context.Background(), Source{URL: srv.URL}, path, nil, nil)
	if err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if res.BytesWritten != int64(len(payload)) {
		t.Fatalf("BytesWritten = %d, want %d", res.BytesWritten, len(payload))
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch on single-stream path")
	}
}

func TestToFile_RefreshOnExpiry(t *testing.T) {
	payload := makePayload(8000)
	origin := newTokenOrigin(payload, map[string]bool{"v1": false, "v2": true})
	srv := httptest.NewServer(origin)
	defer srv.Close()

	var refreshes atomic.Int32
	refresh := func(_ context.Context, f *potoken.HTTPFailure) (Source, error) {
		refreshes.Add(1)
		if f == nil || f.StatusCode != http.StatusForbidden {
			t.Errorf("refresh failure = %+v, want 403", f)
		}
		return Source{URL: srv.URL + "?tok=v2", ContentLength: int64(len(payload))}, nil
	}

	d := newTestDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?tok=v1", ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, refresh, nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh called %d times, want 1 (coordinated)", got)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after refresh")
	}
	if origin.hitCount("v2") < 8 {
		t.Fatalf("v2 hits = %d, want >= 8 chunks", origin.hitCount("v2"))
	}
}

func TestToFile_RefreshExhausted(t *testing.T) {
	payload := makePayload(8000)
	origin := newTokenOrigin(payload, map[string]bool{}) // every token invalid
	srv := httptest.NewServer(origin)
	defer srv.Close()

	refresh := func(context.Context, *potoken.HTTPFailure) (Source, error) {
		return Source{URL: srv.URL + "?tok=bad", ContentLength: int64(len(payload))}, nil
	}

	d := newTestDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?tok=bad", ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, refresh, nil)
	if !errors.Is(err, waxerr.ErrURLExpired) {
		t.Fatalf("err = %v, want ErrURLExpired", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("output file should not exist after failure")
	}
	assertNoTempFiles(t, dir)
}

func TestToFile_403WithoutRefresh(t *testing.T) {
	origin := newTokenOrigin(makePayload(4000), map[string]bool{})
	srv := httptest.NewServer(origin)
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?tok=x", ContentLength: 4000}
	_, err := d.ToFile(context.Background(), src, path, nil, nil)
	if !errors.Is(err, waxerr.ErrURLExpired) {
		t.Fatalf("err = %v, want ErrURLExpired", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("output file should not exist")
	}
}

func TestToFile_CancelCleansTemp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()

	src := Source{URL: srv.URL, ContentLength: 8000}
	_, err := d.ToFile(ctx, src, path, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("output file should not exist after cancel")
	}
	assertNoTempFiles(t, dir)
}

func TestToFile_Progress(t *testing.T) {
	payload := makePayload(8000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1000, 4)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	sink := &progressSink{}
	src := Source{URL: srv.URL, ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, nil, sink.fn()); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	final := sink.max()
	if final.BytesWritten != int64(len(payload)) {
		t.Fatalf("final progress = %d, want %d", final.BytesWritten, len(payload))
	}
	if final.Total != int64(len(payload)) {
		t.Fatalf("progress Total = %d, want %d", final.Total, len(payload))
	}
}
