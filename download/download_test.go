package download

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// newTestDownloader builds a Downloader with fast backoffs for tests.
func newTestDownloader(chunkSize int64, parallelism int) *Downloader {
	return New(Config{
		HTTPClient: httpx.New(httpx.Config{
			MaxRetries:  1,
			BaseBackoff: time.Millisecond,
			MaxBackoff:  2 * time.Millisecond,
		}),
		ChunkSize:       chunkSize,
		Parallelism:     parallelism,
		MaxChunkRetries: 2,
		MaxRefreshes:    2,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	})
}

// makePayload returns deterministic bytes of length n.
func makePayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// serveRange writes the requested byte range of payload, honoring a Range header
// (206 + Content-Range), a ?range=a-b query (200), or neither (200, full body).
func serveRange(w http.ResponseWriter, r *http.Request, payload []byte) {
	total := int64(len(payload))
	if rng := r.URL.Query().Get("range"); rng != "" {
		start, end := parseTestRange(rng, total)
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[start : end+1])
		return
	}
	if h := r.Header.Get("Range"); h != "" {
		start, end := parseTestRange(strings.TrimPrefix(h, "bytes="), total)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func parseTestRange(spec string, total int64) (start, end int64) {
	parts := strings.SplitN(spec, "-", 2)
	start, _ = strconv.ParseInt(parts[0], 10, 64)
	if len(parts) == 2 && parts[1] != "" {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	} else {
		end = total - 1
	}
	if end > total-1 {
		end = total - 1
	}
	return start, end
}

// progressSink collects Progress events safely from parallel workers.
type progressSink struct {
	mu     sync.Mutex
	events []Progress
}

func (s *progressSink) fn() ProgressFunc {
	return func(p Progress) {
		s.mu.Lock()
		s.events = append(s.events, p)
		s.mu.Unlock()
	}
}

func (s *progressSink) max() Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m Progress
	for _, e := range s.events {
		if e.BytesWritten >= m.BytesWritten {
			m = e
		}
	}
	return m
}

func TestNew_Defaults(t *testing.T) {
	d := New(Config{HTTPClient: httpx.New(httpx.Config{})})
	if d.chunkSize != defaultChunkSize {
		t.Errorf("chunkSize = %d, want %d", d.chunkSize, defaultChunkSize)
	}
	if d.parallelism != defaultParallelism {
		t.Errorf("parallelism = %d, want %d", d.parallelism, defaultParallelism)
	}
	if d.maxChunkRetries != defaultMaxChunkRetries {
		t.Errorf("maxChunkRetries = %d, want %d", d.maxChunkRetries, defaultMaxChunkRetries)
	}
	if d.maxRefreshes != defaultMaxRefreshes {
		t.Errorf("maxRefreshes = %d, want %d", d.maxRefreshes, defaultMaxRefreshes)
	}
}

func TestNew_PanicsWithoutHTTPClient(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic when HTTPClient is nil")
		}
	}()
	New(Config{})
}

func TestPlanChunks(t *testing.T) {
	tests := []struct {
		total, size int64
		want        []chunkSpan
	}{
		{13, 5, []chunkSpan{{0, 4}, {5, 9}, {10, 12}}},
		{10, 10, []chunkSpan{{0, 9}}},
		{10, 11, []chunkSpan{{0, 9}}},
		{10, 9, []chunkSpan{{0, 8}, {9, 9}}},
		{0, 5, nil},
		{1, 5, []chunkSpan{{0, 0}}},
	}
	for _, tt := range tests {
		got := planChunks(tt.total, tt.size)
		if len(got) != len(tt.want) {
			t.Fatalf("planChunks(%d,%d) = %v, want %v", tt.total, tt.size, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("planChunks(%d,%d)[%d] = %v, want %v", tt.total, tt.size, i, got[i], tt.want[i])
			}
		}
	}
}

func TestSharedSource_RefreshOncePerGeneration(t *testing.T) {
	var calls atomic.Int32
	refresh := func(context.Context, *potoken.HTTPFailure) (Source, error) {
		calls.Add(1)
		return Source{URL: "v2"}, nil
	}
	s := newSharedSource(Source{URL: "v1"}, refresh, 5)

	// 20 workers all observe generation 0 and try to renew at once; only the
	// first should call refresh, the rest should see the advanced generation.
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.renew(context.Background(), 0, nil); err != nil {
				t.Errorf("renew: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh called %d times, want 1", got)
	}
	if src, _ := s.current(); src.URL != "v2" {
		t.Fatalf("current URL = %q, want v2", src.URL)
	}
}

func TestSharedSource_RefreshExhausted(t *testing.T) {
	s := newSharedSource(Source{URL: "v1"}, func(context.Context, *potoken.HTTPFailure) (Source, error) {
		return Source{URL: "still-bad"}, nil
	}, 2)

	gen := 0
	for i := range 4 {
		// Bytes land between refreshes so the no-progress bail stays out of the
		// way: this test is about the flat budget, which only a moving stream
		// reaches.
		s.noteDelivered(1 << 10)
		_, g, err := s.renew(context.Background(), gen, nil)
		gen = g
		switch {
		case i < 2 && err != nil:
			t.Fatalf("attempt %d: unexpected error %v", i, err)
		case i >= 2 && !errors.Is(err, waxerr.ErrURLExpired):
			t.Fatalf("attempt %d: err = %v, want ErrURLExpired", i, err)
		}
	}
}

// Consecutive refreshes with nothing delivered in between mean every new
// session served nothing, so renew stops there instead of spending the rest of
// a budget that cannot help. One repeat is allowed: a rotation that works works
// on its first try. The stop is the incomplete-delivery class, not an expiry:
// nothing here expired.
func TestSharedSource_NoProgressRefusalStops(t *testing.T) {
	var calls atomic.Int32
	s := newSharedSource(Source{URL: "v1"}, func(context.Context, *potoken.HTTPFailure) (Source, error) {
		calls.Add(1)
		return Source{URL: "fresh"}, nil
	}, 5)

	gen := 0
	var err error
	for range 3 {
		_, gen, err = s.renew(context.Background(), gen, nil)
	}
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", err)
	}
	if errors.Is(err, waxerr.ErrURLExpired) {
		t.Fatalf("err = %v claims an expiry; the server refused fresh URLs, nothing expired", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("refresh called %d times, want 2 before the bail; the budget of 5 must not be spent", got)
	}
	if !strings.Contains(err.Error(), "no bytes") {
		t.Errorf("err = %q, want it to report that nothing was delivered", err)
	}
	if strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "budget") {
		t.Errorf("err = %q, want no expiry or budget prose in a refusal", err)
	}
}

// A refusal partway through names how much arrived before the stall rather than
// claiming a delivery that never started.
func TestSharedSource_NoProgressRefusalNamesDelivered(t *testing.T) {
	s := newSharedSource(Source{URL: "v1"}, func(context.Context, *potoken.HTTPFailure) (Source, error) {
		return Source{URL: "fresh"}, nil
	}, 5)

	s.noteDelivered(983040)
	gen := 0
	var err error
	for range 4 {
		_, gen, err = s.renew(context.Background(), gen, nil)
	}
	if !strings.Contains(err.Error(), "983040") {
		t.Fatalf("err = %q, want it to name the 983040 bytes that did arrive", err)
	}
}

// Genuine expiries stay out of the no-progress run entirely: two 410s at the
// same position must not prime the bail for the next ordinary 403.
func TestSharedSource_GoneRefreshesDoNotPrimeTheBail(t *testing.T) {
	var calls atomic.Int32
	s := newSharedSource(Source{URL: "v1"}, func(context.Context, *potoken.HTTPFailure) (Source, error) {
		calls.Add(1)
		return Source{URL: "fresh"}, nil
	}, 5)

	goneFailure := &potoken.HTTPFailure{StatusCode: http.StatusGone}
	gen := 0
	var err error
	for range 2 {
		if _, gen, err = s.renew(context.Background(), gen, goneFailure); err != nil {
			t.Fatalf("410 renew: %v", err)
		}
	}
	// The following ordinary 403s start their own run: one repeat is still
	// allowed before the bail, so the third 403-shaped request bails, not the
	// first.
	for range 2 {
		if _, gen, err = s.renew(context.Background(), gen, nil); err != nil {
			t.Fatalf("403 renew after 410s: %v", err)
		}
	}
	_, _, err = s.renew(context.Background(), gen, nil)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want the bail only after two no-progress 403 refreshes of their own", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("refresh called %d times, want 4 (two 410s uncounted + two 403s)", got)
	}
}

func TestSharedSource_NilRefresh(t *testing.T) {
	s := newSharedSource(Source{URL: "v1"}, nil, 2)
	if _, _, err := s.renew(context.Background(), 0, nil); !errors.Is(err, waxerr.ErrURLExpired) {
		t.Fatalf("err = %v, want ErrURLExpired", err)
	}
}

func TestFetch_403ClassifiedAsNeedRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("expired"))
	}))
	defer srv.Close()

	d := newTestDownloader(1<<10, 2)
	_, err := d.fetch(context.Background(), Source{URL: srv.URL}, 0, -1)
	nr, ok := errors.AsType[*needRefreshError](err)
	if !ok {
		t.Fatalf("err = %v, want *needRefreshError", err)
	}
	if nr.failure == nil || nr.failure.StatusCode != http.StatusForbidden {
		t.Fatalf("failure = %+v, want status 403", nr.failure)
	}
	if nr.failure.Body != "expired" {
		t.Fatalf("failure body = %q, want %q", nr.failure.Body, "expired")
	}
	if !strings.Contains(nr.Error(), "403") {
		t.Fatalf("error message = %q, want it to mention 403", nr.Error())
	}
}

func TestFetch_IgnoredRangeHeaderIsError(t *testing.T) {
	// Server answers a ranged request with the full body (200), i.e. it ignored
	// the Range header. HeaderRange must reject this to protect offset writes.
	payload := makePayload(100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	d := newTestDownloader(1<<10, 2)
	_, err := d.fetch(context.Background(), Source{URL: srv.URL}, 10, 19)
	if err == nil || !strings.Contains(err.Error(), "ignored Range") {
		t.Fatalf("err = %v, want ignored-Range error", err)
	}
}
