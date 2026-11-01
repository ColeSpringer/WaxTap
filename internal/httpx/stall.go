package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"
)

// DefaultStallTimeout is the idle window a layer falls back to when it is
// configured with zero. A healthy server never pauses this long between bytes,
// and one that has is not coming back.
const DefaultStallTimeout = 30 * time.Second

// StallTimeout resolves a configured idle window the way every layer spells
// it: zero selects DefaultStallTimeout, a negative value disables the guard and
// resolves to zero, and anything else is itself.
func StallTimeout(configured time.Duration) time.Duration {
	switch {
	case configured < 0:
		return 0
	case configured == 0:
		return DefaultStallTimeout
	default:
		return configured
	}
}

// StallError reports a response that stopped arriving: the server sent nothing
// for Idle, either before its headers (Headers false) or part way through its
// body, after Received bytes of it. Host names the server when the request went
// through Client.Do, which is where the host is known.
//
// It is a timeout, and errors.Is(err, context.DeadlineExceeded) says so, so a
// classifier that already buckets deadlines needs no new case. It is not a
// cancellation: the caller's context is still live when a stall fires, which is
// how a retry ladder tells the two apart.
type StallError struct {
	Idle     time.Duration // the window that passed with nothing received
	Received int64         // body bytes read from this response before it stalled
	Headers  bool          // whether the response headers had arrived
	Host     string        // the server waited on, or "" when not known
}

func (e *StallError) Error() string {
	who := "the server"
	if e.Host != "" {
		who = e.Host
	}
	switch {
	case !e.Headers:
		return fmt.Sprintf("%s did not answer within %v", who, e.Idle)
	case e.Received > 0:
		return fmt.Sprintf("%s sent nothing for %v after %d bytes", who, e.Idle, e.Received)
	default:
		return fmt.Sprintf("%s sent nothing for %v", who, e.Idle)
	}
}

// Is makes a stall match context.DeadlineExceeded.
func (e *StallError) Is(target error) bool { return target == context.DeadlineExceeded }

// StallWatch bounds the wait for the next byte of a response, where a deadline
// bounds the whole exchange. A deadline has to be sized for the slowest link a
// transfer may run over, and a transfer on a link slower than that fails while
// a connection that has hung outright still takes the whole deadline to fail.
// The watch instead cancels the waiting request, with a *StallError as the
// cause, when Idle passes with nothing received, so a slow link finishes and a
// hung connection fails fast.
//
// The clock runs only while something is waiting on the server: from an attempt
// going out until its headers arrive, and then for the duration of each read of
// the body. A consumer that takes its time between reads is not a server that
// has stopped, and neither is one of this package's own waits, so a backoff
// sleep or a host cooldown under the watched context holds the clock (see Sleep
// and HostLimiter.Wait). Time the transport spends before the request is on
// the wire, such as waiting for a connection, does count: from here the path to
// the server cannot be told from the server.
//
// Under Client.Do each attempt waits under its own context, so a stall before
// the headers fails that attempt alone and Do retries it like any other
// transport failure; the body Do hands back is bound to the attempt that
// produced it. A caller that sends the request some other way is guarded by the
// context WithStallWatch returned.
//
// Methods are safe on a nil watch, which is what WithStallWatch returns for a
// non-positive idle, so a caller with the guard disabled needs no branches.
type StallWatch struct {
	idle   time.Duration
	cancel context.CancelCauseFunc // releases the base context, and every attempt under it
	timer  *time.Timer

	mu       sync.Mutex
	current  context.CancelCauseFunc // what a stall cancels: the base, or the attempt in flight
	armed    bool                    // the timer will call fire; false once fire found nothing waiting
	since    time.Time               // when the current wait began
	waiting  bool                    // an attempt or a read is waiting on the server
	pauses   int                     // deliberate waits in progress, which hold the clock
	headers  bool                    // the current attempt's headers have arrived
	received int64                   // body bytes read from the current attempt
	host     string                  // the server of the current attempt, when known
	stall    *StallError             // the stall that ended the current wait, if one did
	done     bool                    // stopped; the clock never runs again
}

type stallWatchKey struct{}

// WithStallWatch derives from ctx a context whose requests are watched: idle
// passing with nothing received cancels the waiting attempt with a *StallError
// as its cause. The clock starts at once, since a request is about to go out.
// The caller attaches the response body with Body, translates a failure with
// Err, and calls Stop when the exchange is over; Body's Close does the last for
// it.
//
// A non-positive idle returns ctx unchanged and a nil watch.
func WithStallWatch(ctx context.Context, idle time.Duration) (context.Context, *StallWatch) {
	if idle <= 0 {
		return ctx, nil
	}
	base, cancel := context.WithCancelCause(ctx)
	w := &StallWatch{idle: idle, cancel: cancel, current: cancel, waiting: true, since: time.Now(), armed: true}
	w.timer = time.AfterFunc(idle, w.fire)
	return context.WithValue(base, stallWatchKey{}, w), w
}

// stallWatchFrom returns the watch a WithStallWatch context carries, or nil.
func stallWatchFrom(ctx context.Context) *StallWatch {
	w, _ := ctx.Value(stallWatchKey{}).(*StallWatch)
	return w
}

// running reports whether the clock is running. The caller holds w.mu.
func (w *StallWatch) running() bool { return !w.done && w.waiting && w.pauses == 0 }

// startLocked begins a wait: the clock runs from now, and the timer is armed
// unless it is already due to check in. The caller holds w.mu.
func (w *StallWatch) startLocked() {
	w.waiting = true
	w.since = time.Now()
	if !w.armed && !w.done {
		w.armed = true
		w.timer.Reset(w.idle)
	}
}

// fire is the timer callback. It judges the current state rather than the
// arming it came from: a callback that is late for a wait that has ended finds
// nothing running and stands down, and one that is early for a wait that began
// since re-arms for the remainder. Only a wait that has run the whole window
// is a stall.
func (w *StallWatch) fire() {
	w.mu.Lock()
	if !w.running() {
		w.armed = false // the next wait re-arms
		w.mu.Unlock()
		return
	}
	if rem := w.idle - time.Since(w.since); rem > 0 {
		w.timer.Reset(rem)
		w.mu.Unlock()
		return
	}
	err := &StallError{Idle: w.idle, Received: w.received, Headers: w.headers, Host: w.host}
	w.stall = err
	w.waiting = false
	w.armed = false
	cancel := w.current
	w.mu.Unlock()
	cancel(err)
}

// attempt begins one attempt of a request to host and returns the context it
// should run under: a stall cancels that context alone, with the cause recorded
// for Err, so the caller can retry under ctx. The returned cancel releases the
// attempt once it has failed; a successful attempt's context lives on with its
// body and is released by Stop.
func (w *StallWatch) attempt(ctx context.Context, host string) (context.Context, context.CancelCauseFunc) {
	if w == nil {
		return ctx, func(error) {}
	}
	actx, cancel := context.WithCancelCause(ctx)
	w.mu.Lock()
	w.current = cancel
	w.host = host
	w.stall = nil
	w.headers = false
	w.received = 0
	w.startLocked()
	w.mu.Unlock()
	return actx, cancel
}

// pause holds the clock for a deliberate wait. Pauses nest.
func (w *StallWatch) pause() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.pauses++
	w.mu.Unlock()
}

// resume ends a pause. A wait still outstanding gets a fresh window from now.
func (w *StallWatch) resume() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.pauses--
	if w.waiting && w.pauses == 0 {
		w.startLocked()
	}
	w.mu.Unlock()
}

// Body attaches the watch to a response body. The headers have arrived, so the
// clock stops until a read waits on the body; each read runs it afresh, and a
// read that fails because the clock ran out reports the *StallError in place of
// the bare cancellation the transport returns. Closing the body stops the
// watch.
func (w *StallWatch) Body(rc io.ReadCloser) io.ReadCloser {
	if w == nil {
		return rc
	}
	w.mu.Lock()
	w.headers = true
	w.waiting = false
	w.mu.Unlock()
	return &stallBody{rc: rc, w: w}
}

// Err returns the stall when the watch is what ended the current attempt, and
// err otherwise. net/http reports a cancelled request as the cancellation's
// cause inside a *url.Error, or as the bare context error, and neither says
// more than the stall does, so both are replaced by the stall alone; a
// transport error that names what it was waiting on, a proxy that could not be
// reached or a dial that hung, keeps that cause under the stall, so the proxy
// still classifies. io.EOF is never a stall: a body that completed as the clock
// ran out completed.
func (w *StallWatch) Err(err error) error {
	if w == nil || err == nil || errors.Is(err, io.EOF) {
		return err
	}
	w.mu.Lock()
	stall := w.stall
	w.mu.Unlock()
	return stall.explain(err)
}

// explain is Err for a known stall. A nil stall leaves err alone.
func (stall *StallError) explain(err error) error {
	if stall == nil {
		return err
	}
	if st, ok := errors.AsType[*stalledTransport](err); ok {
		return st
	}
	if found, ok := errors.AsType[*StallError](err); ok {
		return found // the transport passed the cause through; its wrapper adds only the URL
	}
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.Err != nil {
		err = ue.Err
	}
	if NamesTransportCause(err) {
		return &stalledTransport{stall: stall, cause: err}
	}
	return stall
}

// stalledTransport is a stall whose transport error named what it was waiting
// on. The cause is kept for its text and for IsProxyConnect, but out of the
// unwrap chain: a cancelled dial matches context.Canceled, and in the chain it
// would read as the caller giving up.
type stalledTransport struct {
	stall *StallError
	cause error
}

func (e *stalledTransport) Error() string { return e.stall.Error() + ": " + e.cause.Error() }
func (e *stalledTransport) Unwrap() error { return e.stall }

// Stop ends the watch: the clock will not fire, and the contexts it governs are
// released. A watch that has already fired keeps its cause, so Err still names
// the stall afterwards.
func (w *StallWatch) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return
	}
	w.done = true
	w.armed = false
	w.timer.Stop()
	w.mu.Unlock()
	w.cancel(nil)
}

// stallBody is a response body under a StallWatch.
type stallBody struct {
	rc io.ReadCloser
	w  *StallWatch
}

func (b *stallBody) Read(p []byte) (int, error) {
	w := b.w
	w.mu.Lock()
	w.startLocked()
	w.mu.Unlock()

	n, err := b.rc.Read(p)

	w.mu.Lock()
	w.waiting = false
	w.received += int64(n)
	stall := w.stall
	w.mu.Unlock()
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	return n, stall.explain(err)
}

func (b *stallBody) Close() error {
	err := b.rc.Close()
	b.w.Stop()
	return err
}
