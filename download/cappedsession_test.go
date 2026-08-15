package download

import (
	"bytes"
	"context"
	"errors"
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

// The bound is what stops a stream that will never deliver from being re-signed
// forever, and it is a flat count: refreshes measure how many sessions were
// tried, which does not scale with the file.
func TestToFile_DeadURLStopsAtRefreshBound(t *testing.T) {
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
	if !errors.Is(err, waxerr.ErrURLExpired) {
		t.Fatalf("err = %v, want ErrURLExpired", err)
	}
	if !strings.Contains(err.Error(), "re-resolves") {
		t.Errorf("err = %q, want it to name the spent re-resolve budget", err)
	}
	if got := refreshes.Load(); got != 2 {
		t.Fatalf("refresh called %d times, want exactly the budget of 2", got)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("no output file should remain after an exhausted refresh budget")
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
