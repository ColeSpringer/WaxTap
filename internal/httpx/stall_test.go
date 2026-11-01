package httpx

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hungBody never delivers. Read blocks until the request context ends and then
// returns its error, which is what a transport's body does when the request
// behind it is cancelled.
type hungBody struct{ ctx context.Context }

func (b hungBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (hungBody) Close() error { return nil }

// thenHung delivers data once and hangs like hungBody after that.
type thenHung struct {
	ctx  context.Context
	data []byte
}

func (b *thenHung) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*thenHung) Close() error { return nil }

// awaitStall waits for ctx to end and returns its cause, failing the test if
// nothing happens within a generous bound.
func awaitStall(t *testing.T, ctx context.Context) error {
	t.Helper()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-time.After(5 * time.Second):
		t.Fatal("the stall watch never fired")
		return nil
	}
}

func TestWithStallWatch_ZeroIsNoWatch(t *testing.T) {
	parent := context.Background()
	ctx, w := WithStallWatch(parent, 0)
	if ctx != parent {
		t.Fatal("a zero idle should return the caller's context unchanged")
	}
	if w != nil {
		t.Fatal("a zero idle should return no watch")
	}
	// A nil watch is inert so callers need no branches.
	rc := io.NopCloser(strings.NewReader("x"))
	if w.Body(rc) != rc {
		t.Error("nil watch: Body should return the body unchanged")
	}
	boom := errors.New("boom")
	if w.Err(boom) != boom {
		t.Error("nil watch: Err should return the error unchanged")
	}
	w.Stop()
}

func TestStallWatch_FiresBeforeHeaders(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 40*time.Millisecond)
	defer w.Stop()

	cause := awaitStall(t, ctx)
	se, ok := errors.AsType[*StallError](cause)
	if !ok {
		t.Fatalf("cause = %v, want *StallError", cause)
	}
	if se.Headers {
		t.Error("Headers = true, want false: no response had arrived")
	}
	if se.Idle != 40*time.Millisecond {
		t.Errorf("Idle = %v, want 40ms", se.Idle)
	}
	if !strings.Contains(se.Error(), "did not answer") {
		t.Errorf("Error() = %q, want it to say the server did not answer", se)
	}
	// The watch translates the bare cancellation a transport reports into the
	// stall that caused it.
	if got := w.Err(context.Canceled); got != se {
		t.Errorf("Err(canceled) = %v, want the stall", got)
	}
}

// Every layer spells the configured window the same way: zero is the default,
// negative disables, anything else is itself.
func TestStallTimeout_ResolvesTheConfiguredWindow(t *testing.T) {
	if got := StallTimeout(0); got != DefaultStallTimeout {
		t.Errorf("StallTimeout(0) = %v, want the default %v", got, DefaultStallTimeout)
	}
	if got := StallTimeout(-1); got != 0 {
		t.Errorf("StallTimeout(-1) = %v, want 0 (disabled)", got)
	}
	if got := StallTimeout(time.Minute); got != time.Minute {
		t.Errorf("StallTimeout(1m) = %v, want 1m", got)
	}
}

func TestStallError_IsATimeout(t *testing.T) {
	err := error(&StallError{Idle: time.Second})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("a stall should classify as a deadline, so callers that bucket timeouts need no new case")
	}
	if errors.Is(err, context.Canceled) {
		t.Error("a stall is not a cancellation")
	}
}

func TestStallWatch_BodyReportsTheStall(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 40*time.Millisecond)
	rc := w.Body(hungBody{ctx})
	defer rc.Close()

	_, err := rc.Read(make([]byte, 8))
	se, ok := errors.AsType[*StallError](err)
	if !ok {
		t.Fatalf("Read err = %v, want *StallError", err)
	}
	if !se.Headers || se.Received != 0 {
		t.Errorf("stall = %+v, want Headers=true Received=0", se)
	}
	if !strings.Contains(se.Error(), "sent nothing") {
		t.Errorf("Error() = %q, want it to say the server sent nothing", se)
	}
}

func TestStallWatch_BodyCountsBytesBeforeTheStall(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 40*time.Millisecond)
	rc := w.Body(&thenHung{ctx: ctx, data: []byte("abc")})
	defer rc.Close()

	buf := make([]byte, 8)
	n, err := rc.Read(buf)
	if err != nil || n != 3 {
		t.Fatalf("first Read = (%d, %v), want (3, nil)", n, err)
	}
	_, err = rc.Read(buf)
	se, ok := errors.AsType[*StallError](err)
	if !ok {
		t.Fatalf("second Read err = %v, want *StallError", err)
	}
	if se.Received != 3 {
		t.Errorf("Received = %d, want 3", se.Received)
	}
	if !strings.Contains(se.Error(), "after 3 bytes") {
		t.Errorf("Error() = %q, want it to count the bytes that did arrive", se)
	}
}

// A steady trickle slower than the whole transfer's worth of idle time must
// finish: the clock measures the wait for the next byte, not the transfer.
func TestStallWatch_BytesRestartTheClock(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, w := WithStallWatch(context.Background(), 300*time.Millisecond)
	rc := w.Body(pr)
	defer rc.Close()

	go func() {
		for range 40 {
			time.Sleep(15 * time.Millisecond)
			if _, err := pw.Write([]byte{'x'}); err != nil {
				return
			}
		}
		pw.Close()
	}()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("read %d bytes, want 40", len(got))
	}
	if ctx.Err() != nil {
		t.Fatal("the watch fired on a body that kept delivering")
	}
}

// The clock runs only while a read is waiting on the server. A consumer that
// takes its time between reads is not a server that has stopped.
func TestStallWatch_SlowConsumerIsNotAStall(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 40*time.Millisecond)
	rc := w.Body(io.NopCloser(strings.NewReader("ready")))
	defer rc.Close()

	time.Sleep(120 * time.Millisecond) // three idle windows with no read issued
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != "ready" {
		t.Fatalf("ReadAll = (%q, %v), want (\"ready\", nil)", got, err)
	}
	if ctx.Err() != nil {
		t.Fatal("the watch fired while no read was outstanding")
	}
}

// Headers arriving is activity: attaching the body stops the header-phase
// clock, and only a read restarts it.
func TestStallWatch_HeadersStopTheHeaderClock(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 100*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	rc := w.Body(io.NopCloser(strings.NewReader("")))
	defer rc.Close()
	time.Sleep(200 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("the header clock kept running after the headers arrived")
	}
}

func TestStallWatch_StopReleasesWithoutAStall(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 200*time.Millisecond)
	w.Stop()
	time.Sleep(300 * time.Millisecond)
	if _, ok := errors.AsType[*StallError](context.Cause(ctx)); ok {
		t.Fatal("a stopped watch must not fire")
	}
	boom := errors.New("boom")
	if got := w.Err(boom); got != boom {
		t.Errorf("Err after Stop = %v, want the error unchanged", got)
	}
}

// Deliberate waits are not stalls: a backoff sleep under a watched context
// holds the clock, which restarts when the sleep ends.
func TestSleep_HoldsTheStallClock(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 100*time.Millisecond)
	defer w.Stop()

	if err := Sleep(ctx, 300*time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the watch fired during a deliberate sleep")
	}
	// Nothing is read after the sleep, so the restarted clock fires.
	if _, ok := errors.AsType[*StallError](awaitStall(t, ctx)); !ok {
		t.Fatal("the clock did not restart after the sleep")
	}
}

func TestHostLimiter_WaitHoldsTheStallClock(t *testing.T) {
	l := NewHostLimiter(0)
	l.Penalize("h", 300*time.Millisecond)
	ctx, w := WithStallWatch(context.Background(), 100*time.Millisecond)
	defer w.Stop()

	if err := l.Wait(ctx, "h"); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the watch fired during a host cooldown")
	}
}

// A Retry-After longer than the idle window is the server asking us to wait,
// not a stall, so Do must sleep it out and succeed on the retry.
func TestDo_RetryAfterPauseDoesNotTripTheStall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{MaxRetries: 2, MaxRetryWait: 5 * time.Second})
	ctx, w := WithStallWatch(context.Background(), 300*time.Millisecond)
	defer w.Stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the Retry-After pause", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// lateEOFBody delivers its last bytes with io.EOF only after a pause, ignoring
// the request context the way a fake transport might.
type lateEOFBody struct {
	delay time.Duration
	data  []byte
}

func (b *lateEOFBody) Read(p []byte) (int, error) {
	time.Sleep(b.delay)
	n := copy(p, b.data)
	b.data = nil
	return n, io.EOF
}

func (*lateEOFBody) Close() error { return nil }

// A body that completes as the clock runs out is complete. EOF is never
// reported as a stall, whatever the timer did meanwhile.
func TestStallWatch_EOFIsNotAStall(t *testing.T) {
	_, w := WithStallWatch(context.Background(), 40*time.Millisecond)
	rc := w.Body(&lateEOFBody{delay: 120 * time.Millisecond, data: []byte("end")})
	defer rc.Close()
	n, err := rc.Read(make([]byte, 8))
	if n != 3 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read = (%d, %v), want (3, EOF)", n, err)
	}
}

// A timer callback that runs when nothing is waiting does nothing: the fire is
// validated against the current state, not against the arming it came from.
func TestStallWatch_FireWithNothingWaitingIsIgnored(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), time.Hour)
	rc := w.Body(io.NopCloser(strings.NewReader(""))) // headers arrived; no read outstanding
	defer rc.Close()
	w.fire()
	if ctx.Err() != nil {
		t.Fatal("a fire with no wait outstanding cancelled the request")
	}
}

// A callback arriving before the window has passed on the current wait re-arms
// for the remainder instead of firing.
func TestStallWatch_EarlyFireReArms(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 80*time.Millisecond)
	defer w.Stop()
	w.fire()
	if ctx.Err() != nil {
		t.Fatal("fired before the window had passed")
	}
	if _, ok := errors.AsType[*StallError](awaitStall(t, ctx)); !ok {
		t.Fatal("the re-armed clock did not fire once the window passed")
	}
}

// A stalled attempt whose transport error names a cause keeps that cause: a
// proxy that never answered still reads as a proxy failure. A bare cancellation
// names nothing and is replaced by the stall alone.
func TestStallWatch_ErrKeepsATransportCause(t *testing.T) {
	ctx, w := WithStallWatch(context.Background(), 30*time.Millisecond)
	defer w.Stop()
	awaitStall(t, ctx)

	proxied := &url.Error{Op: "Get", URL: "https://media.example/x", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("operation was canceled")}}
	err := w.Err(proxied)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Err(proxyconnect) = %v, want the stall in the chain", err)
	}
	if !IsProxyConnect(err) {
		t.Errorf("Err(proxyconnect) = %v, want the proxy cause kept", err)
	}

	if errors.Is(err, context.Canceled) {
		t.Errorf("Err(proxyconnect) = %v, want no cancellation in the chain: a cancelled dial matches context.Canceled on its own", err)
	}

	bare := &url.Error{Op: "Get", URL: "https://media.example/x", Err: context.Canceled}
	got := w.Err(bare)
	if _, ok := got.(*StallError); !ok {
		t.Errorf("Err(bare cancellation) = %v, want the stall alone", got)
	}

	// net/http reports the cancellation's cause, so the stall arrives inside a
	// *url.Error naming the URL; the wrapper adds nothing worth keeping.
	passed := &url.Error{Op: "Get", URL: "https://media.example/x?sig=secret", Err: context.Cause(ctx)}
	got = w.Err(passed)
	if _, ok := got.(*StallError); !ok {
		t.Errorf("Err(passed-through cause) = %v, want the stall alone", got)
	}
	if strings.Contains(got.Error(), "secret") {
		t.Errorf("Err(passed-through cause) = %q, want the URL dropped", got)
	}
}

func TestStallError_NamesTheHost(t *testing.T) {
	anon := &StallError{Idle: time.Second}
	if got := anon.Error(); !strings.HasPrefix(got, "the server did not answer") {
		t.Errorf("anonymous = %q", got)
	}
	named := &StallError{Idle: time.Second, Headers: true, Received: 7, Host: "cdn.example"}
	if got := named.Error(); got != "cdn.example sent nothing for 1s after 7 bytes" {
		t.Errorf("named = %q", got)
	}
}

// Do retries an attempt that stalled before answering the way it retries any
// transport failure, so one hung connection does not end a request that still
// has attempts.
func TestDo_RetriesAHeaderStall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-r.Context().Done() // never answers; the client gives up
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{MaxRetries: 2, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	ctx, w := WithStallWatch(context.Background(), 60*time.Millisecond)
	defer w.Stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

// When every attempt stalls, Do reports the stall with the host named, and the
// caller's context is untouched.
func TestDo_ExhaustedStallsNameTheHost(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := New(Config{MaxRetries: 1, BaseBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond})
	parent := context.Background()
	ctx, w := WithStallWatch(parent, 40*time.Millisecond)
	defer w.Stop()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	_, err := c.Do(req)
	se, ok := errors.AsType[*StallError](err)
	if !ok {
		t.Fatalf("Do err = %v, want *StallError", err)
	}
	if se.Host != req.URL.Hostname() {
		t.Errorf("Host = %q, want %q", se.Host, req.URL.Hostname())
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one retry)", calls.Load())
	}
	if parent.Err() != nil {
		t.Error("the caller's context was cancelled")
	}
}

// bottomlessReader always fills the buffer, so a read costs nothing but the
// wrapper.
type bottomlessReader struct{}

func (bottomlessReader) Read(p []byte) (int, error) { return len(p), nil }

func BenchmarkStallBodyRead(b *testing.B) {
	_, w := WithStallWatch(context.Background(), time.Hour)
	defer w.Stop()
	rc := w.Body(io.NopCloser(bottomlessReader{}))
	buf := make([]byte, 32<<10)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := rc.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
}
