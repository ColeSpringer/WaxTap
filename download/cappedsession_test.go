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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// cappedOrigin models googlevideo's per-session delivery cap, which the unit
// tests in resilience_test.go cannot reproduce: each signed URL carries a session
// token, the origin serves a bounded number of bytes to a token, and every later
// request on that token gets an empty-body 403. Re-signing the same token is
// futile; only a refresh that mints a new one lets the transfer continue.
type cappedOrigin struct {
	payload  []byte
	capBytes int64 // bytes one session token may be served

	mu     sync.Mutex
	served map[string]int64
}

func newCappedOrigin(payload []byte, capBytes int64) *cappedOrigin {
	return &cappedOrigin{payload: payload, capBytes: capBytes, served: map[string]int64{}}
}

func (o *cappedOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	total := int64(len(o.payload))
	start, end := int64(0), total-1
	if h := r.Header.Get("Range"); h != "" {
		start, end = parseTestRange(strings.TrimPrefix(h, "bytes="), total)
	}

	token := r.URL.Query().Get("s")
	o.mu.Lock()
	spent := o.served[token]
	if spent >= o.capBytes {
		o.mu.Unlock()
		w.WriteHeader(http.StatusForbidden) // empty body: the cap's measured signature
		return
	}
	o.served[token] = spent + (end - start + 1)
	o.mu.Unlock()

	serveRange(w, r, o.payload)
}

// sessionRefresh returns a RefreshFunc that mints a new session token each call,
// the way an identity rotation re-signs a stream URL under a fresh guest session.
func sessionRefresh(base string, total int64, calls *atomic.Int32) RefreshFunc {
	return func(context.Context, *potoken.HTTPFailure) (Source, error) {
		n := calls.Add(1)
		return Source{URL: base + "?s=" + strconv.Itoa(int(n)), ContentLength: total}, nil
	}
}

func cappedDownloader(chunkSize int64, parallelism, maxRefreshes int) *Downloader {
	return New(Config{
		HTTPClient:      httpx.New(httpx.Config{MaxRetries: -1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond}),
		ChunkSize:       chunkSize,
		Parallelism:     parallelism,
		MaxChunkRetries: 2,
		MaxRefreshes:    maxRefreshes,
		BaseBackoff:     time.Millisecond,
		MaxBackoff:      2 * time.Millisecond,
	})
}

// A capped session recovers inside the budget: each refresh mints a new token and
// the transfer continues on it. This is the escape the whole refresh path exists
// for, and the unit tests around it only ever exercised a single stale URL.
func TestToFile_CappedSessionRecoversAfterRefresh(t *testing.T) {
	const chunk = 10 << 10
	payload := makePayload(8 * chunk)
	// Two sessions carry the whole file, so one refresh is enough.
	origin := newCappedOrigin(payload, 4*chunk)
	srv := httptest.NewServer(origin)
	defer srv.Close()

	var refreshes atomic.Int32
	d := cappedDownloader(chunk, 2, 3)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?s=0", ContentLength: int64(len(payload))}
	if _, err := d.ToFile(context.Background(), src, path, sessionRefresh(srv.URL, int64(len(payload)), &refreshes), nil); err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if got := refreshes.Load(); got == 0 {
		t.Fatal("the cap was never hit; the test did not exercise a refresh")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, payload) {
		t.Fatal("output mismatch after recovering from a capped session")
	}
	assertNoTempFiles(t, dir)
}

// deadOrigin rejects every request the way a URL that will never deliver does, so
// each refresh mints a replacement that is just as dead.
type deadOrigin struct{ requests atomic.Int32 }

func (o *deadOrigin) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	o.requests.Add(1)
	w.WriteHeader(http.StatusForbidden)
}

// A dead URL is never re-signed forever: the no-progress bail stops it after
// two futile sessions, and even a budget that would allow exactly that many
// leaves the refusal, not the budget, as the reported cause. Refreshes remain
// a flat count of sessions tried, which does not scale with the file.
func TestToFile_DeadURLStopsAfterTwoSessions(t *testing.T) {
	const chunk = 10 << 10
	payload := makePayload(8 * chunk)
	srv := httptest.NewServer(&deadOrigin{})
	defer srv.Close()

	var refreshes atomic.Int32
	d := cappedDownloader(chunk, 2, 2)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?s=0", ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, sessionRefresh(srv.URL, int64(len(payload)), &refreshes), nil)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", err)
	}
	if !strings.Contains(err.Error(), "consecutive fresh sessions") {
		t.Errorf("err = %q, want the refusal named", err)
	}
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("refresh called %d times, want exactly 2", got)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("no output file should remain after a refused stream")
	}
	assertNoTempFiles(t, dir)
}

// A capped download that outlasts the budget fails as an incomplete delivery, so
// the facade can try another client (and, past that, another whole chain pass).
func TestToFile_CappedBeyondBudgetIsIncomplete(t *testing.T) {
	const chunk = 10 << 10
	payload := makePayload(16 * chunk)
	origin := newCappedOrigin(payload, chunk) // one chunk per session, far short
	srv := httptest.NewServer(origin)
	defer srv.Close()

	var refreshes atomic.Int32
	d := cappedDownloader(chunk, 2, 2)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?s=0", ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, sessionRefresh(srv.URL, int64(len(payload)), &refreshes), nil)
	if !errors.Is(err, waxerr.ErrURLExpired) && !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want an incomplete delivery the caller can retry elsewhere", err)
	}
	if got := refreshes.Load(); got != 2 {
		t.Errorf("refresh called %d times, want the budget of 2", got)
	}
	assertNoTempFiles(t, dir)
}

// A server that serves nothing proves itself on the second refresh with no
// bytes in between: re-signing the URL a third time cannot move a stream that
// has never moved. The ladder stops there rather than at the budget, and the
// error is the incomplete-delivery class with no expiry or budget prose,
// because nothing expired and the budget is not what stopped it.
func TestToFile_RefusalBailsBeforeBudget(t *testing.T) {
	payload := makePayload(64 << 10)
	srv := httptest.NewServer(&deadOrigin{})
	defer srv.Close()

	var refreshes atomic.Int32
	d := cappedDownloader(1<<20, 1, 5) // sequential; budget well above the no-progress limit
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?s=0", ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, sessionRefresh(srv.URL, int64(len(payload)), &refreshes), nil)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", err)
	}
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("refresh called %d times, want 2: re-resolving stops on the second no-progress refusal, not at the budget of 5", got)
	}
	if !strings.Contains(err.Error(), "no bytes") {
		t.Errorf("err = %q, want it to report that the server delivered no bytes", err)
	}
	if !strings.Contains(err.Error(), "consecutive fresh sessions") {
		t.Errorf("err = %q, want it to name the consecutive fresh sessions", err)
	}
	for _, deny := range []string{"expired", "budget"} {
		if strings.Contains(err.Error(), deny) {
			t.Errorf("err = %q, want no %q prose in a refusal", err, deny)
		}
	}
	assertNoTempFiles(t, dir)
}

// The chunked path reads the same signal: its workers hold different spans, but
// bytes delivered download-wide is one number, so a server serving nothing
// bails just as early under parallelism. This is the long-form shape the
// duration-gated refusal actually targets, which a per-offset rule left inert.
func TestToFile_ChunkedRefusalBailsBeforeBudget(t *testing.T) {
	const chunk = 10 << 10
	payload := makePayload(8 * chunk)
	srv := httptest.NewServer(&deadOrigin{})
	defer srv.Close()

	var refreshes atomic.Int32
	d := cappedDownloader(chunk, 2, 5)
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?s=0", ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, sessionRefresh(srv.URL, int64(len(payload)), &refreshes), nil)
	if !errors.Is(err, waxerr.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream", err)
	}
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("refresh called %d times, want 2: the bail must work under parallel chunks, not only on the sequential path", got)
	}
	assertNoTempFiles(t, dir)
}

// steppedOrigin serves one bounded slice per session token and rejects that
// token afterwards, so a sequential reader makes real forward progress between
// refreshes. That is the ordinary-expiry shape the same-offset rule must not
// mistake for a refusal.
type steppedOrigin struct {
	payload []byte
	step    int64

	mu   sync.Mutex
	used map[string]bool
}

func (o *steppedOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	total := int64(len(o.payload))
	start, _ := parseTestRange(strings.TrimPrefix(r.Header.Get("Range"), "bytes="), total)

	token := r.URL.Query().Get("s")
	o.mu.Lock()
	spent := o.used[token]
	o.used[token] = true
	o.mu.Unlock()
	if spent {
		w.WriteHeader(http.StatusForbidden) // empty body: this session is done
		return
	}

	end := min(start+o.step-1, total-1)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(o.payload[start : end+1])
}

// Forward progress between refreshes is an ordinary expiry, not a refusal, so
// the flat budget stays available: the early bail must not fire on a stream
// that is still moving.
func TestToFile_ProgressBetweenRefreshesKeepsFullBudget(t *testing.T) {
	const step = 8 << 10
	payload := makePayload(16 * step)
	srv := httptest.NewServer(&steppedOrigin{payload: payload, step: step, used: map[string]bool{}})
	defer srv.Close()

	var refreshes atomic.Int32
	d := cappedDownloader(1<<20, 1, 4) // sequential; 4 refreshes cannot cover 16 steps
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")

	src := Source{URL: srv.URL + "?s=0", ContentLength: int64(len(payload))}
	_, err := d.ToFile(context.Background(), src, path, sessionRefresh(srv.URL, int64(len(payload)), &refreshes), nil)
	if err == nil {
		t.Fatal("ToFile succeeded; the origin cannot deliver the file inside the budget")
	}
	if got := refreshes.Load(); got != 4 {
		t.Fatalf("refresh called %d times, want the full budget of 4: an advancing offset must not trip the same-offset bail", got)
	}
	if strings.Contains(err.Error(), "consecutive fresh sessions") {
		t.Errorf("err = %q, want the spent-budget message, not the same-offset bail", err)
	}
	assertNoTempFiles(t, dir)
}
