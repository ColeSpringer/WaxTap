// Package download transfers resolved media streams to files, writers, or
// callers that want an io.ReadCloser.
//
// The package works with a [Source]: a signed URL plus the headers and metadata
// needed to fetch it. It does not depend on YouTube extraction; callers that can
// resolve or refresh a stream provide that through [RefreshFunc].
//
// [Downloader.ToFile] stages data in a temp file and commits it atomically.
// [Downloader.ToWriter] streams bytes directly to a caller writer.
// [Downloader.Stream] returns the response body behind an io.ReadCloser.
//
// Signed stream URLs can expire during transfer. On HTTP 403 or 410, the
// downloader asks the refresh function for a new Source and retries the affected
// range. Without a refresh function, expiry fails with waxerr.ErrURLExpired.
//
// Every request runs under a stall guard ([Config.StallTimeout]): a response
// that has not begun, or a body that has stopped delivering, within the window
// is abandoned and the transfer resumed after the bytes already received, so a
// slow link finishes while a hung connection fails fast.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// Defaults applied by New when a Config field is left zero.
const (
	defaultChunkSize       = 10 << 20 // 10 MiB per ranged chunk
	defaultParallelism     = 4        // parallel chunks within one ToFile download
	defaultMaxChunkRetries = 3        // per-chunk retry attempts after the first
	defaultMaxRefreshes    = 3        // URL re-resolves (delivery sessions) allowed per download
	defaultBaseBackoff     = 250 * time.Millisecond
	defaultMaxBackoff      = 5 * time.Second
)

// Source is the input to a download: a resolved, signed URL plus the metadata
// needed to request it.
type Source struct {
	// URL is the signed, playable stream URL.
	URL string
	// ContentLength is the total size in bytes, or 0 if unknown. When known and
	// large enough, ToFile downloads it in parallel ranges; when unknown, all
	// sinks fall back to a single streamed GET.
	ContentLength int64
	// Headers are sent on every request. Some origins bind a signed URL to the
	// request identity used during resolution.
	Headers http.Header
	// ExpiresAt is when the signed URL is expected to lapse, if known. It is
	// advisory: the authoritative signal is a 403 on the wire, which triggers a
	// refresh regardless of this value.
	ExpiresAt time.Time
	// RangeStrategy controls how byte ranges are requested and validated. Nil
	// selects HeaderRange. Use QueryRange for origins that expect a &range= query
	// parameter instead of a Range header.
	RangeStrategy RangeStrategy
}

// RefreshFunc returns a replacement Source after a request indicates that the
// current source expired. failure contains the HTTP status and a bounded body
// sample from the response that triggered the refresh. A nil RefreshFunc
// disables refresh; expired URLs then fail with waxerr.ErrURLExpired.
type RefreshFunc func(ctx context.Context, failure *potoken.HTTPFailure) (Source, error)

// Progress is a byte-count snapshot delivered to a ProgressFunc.
type Progress struct {
	BytesWritten int64 // bytes delivered to the sink so far
	Total        int64 // total expected bytes, or 0 if unknown
}

// ProgressFunc receives best-effort byte progress. It is called synchronously
// from a download worker, so it must be fast and must not block. It may be nil.
type ProgressFunc func(Progress)

// Result reports the byte count for a completed download.
type Result struct {
	BytesWritten int64 // bytes delivered to the sink
}

// StreamInfo is the response metadata returned by Stream alongside the reader.
type StreamInfo struct {
	ContentLength int64  // total size in bytes, or 0 if unknown
	ContentType   string // Content-Type of the first response, if any
}

// Config configures a Downloader. The zero value is usable except for
// HTTPClient, which is required; New fills the rest with defaults.
type Config struct {
	// HTTPClient performs requests with retry/backoff and rate-limit handling.
	// It is required.
	HTTPClient *httpx.Client
	// Logger receives debug logs. Nil discards them.
	Logger *slog.Logger

	// ChunkSize is the byte length of a single ranged chunk (default 10 MiB).
	ChunkSize int64
	// Parallelism caps simultaneous chunks within one ToFile download (default 4).
	Parallelism int
	// StallTimeout bounds the wait for the next byte of any response the
	// Downloader reads: a request whose response has not begun, or a body that
	// has delivered nothing, within the window is abandoned and the transfer
	// resumed after the bytes already received. It is an idle bound, not a
	// deadline, so a slow link finishes while a hung connection fails fast. It
	// covers every method: ranged chunks, sequential streams, and ranged blocks.
	// Zero selects the default (httpx.DefaultStallTimeout, 30 s); a negative
	// value disables the guard, leaving the request context as the only bound.
	StallTimeout time.Duration

	// MaxChunkRetries is the budget of consecutive failed attempts each transfer
	// unit (a chunk, a sequential stream, a probe block) gets after its first
	// request (default 3). A request that fails and a body that adds nothing both
	// spend it, so neither ladder can run its full length in turn; bytes landing
	// refund it, since a transfer that is moving is not failing, and the next
	// attempt resumes after them. Expiry refreshes are counted separately.
	MaxChunkRetries int
	// MaxRefreshes caps the URL refreshes per download (default 3). See
	// sharedSource.renew for why the bound is a flat count.
	MaxRefreshes int

	// BaseBackoff and MaxBackoff tune the download-layer retry sleep (distinct
	// from httpx's own HTTP-level backoff).
	BaseBackoff time.Duration
	MaxBackoff  time.Duration // maximum sleep between download-layer retries
}

// Downloader fetches Sources into sinks. It is safe for concurrent use.
type Downloader struct {
	http            *httpx.Client
	log             *slog.Logger
	chunkSize       int64
	parallelism     int
	stallTimeout    time.Duration // 0 = disabled
	maxChunkRetries int
	maxRefreshes    int
	baseBackoff     time.Duration
	maxBackoff      time.Duration
	// rangeBlock overrides DefaultRangeBlock for a RangeReader. It has no
	// Config field: a block size is a property of how origins behave, not
	// something a caller tunes, and only this package's tests set it so a
	// small fixture can span several blocks.
	rangeBlock int64
}

// New returns a Downloader, filling unset Config fields with defaults. It panics
// if HTTPClient is nil, since a Downloader cannot function without one.
func New(cfg Config) *Downloader {
	if cfg.HTTPClient == nil {
		panic("download: Config.HTTPClient is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = defaultChunkSize
	}
	if cfg.Parallelism <= 0 {
		cfg.Parallelism = defaultParallelism
	}
	if cfg.MaxChunkRetries < 0 {
		cfg.MaxChunkRetries = 0
	} else if cfg.MaxChunkRetries == 0 {
		cfg.MaxChunkRetries = defaultMaxChunkRetries
	}
	if cfg.MaxRefreshes < 0 {
		cfg.MaxRefreshes = 0
	} else if cfg.MaxRefreshes == 0 {
		cfg.MaxRefreshes = defaultMaxRefreshes
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = defaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	return &Downloader{
		http:            cfg.HTTPClient,
		log:             cfg.Logger,
		chunkSize:       cfg.ChunkSize,
		parallelism:     cfg.Parallelism,
		stallTimeout:    httpx.StallTimeout(cfg.StallTimeout),
		maxChunkRetries: cfg.MaxChunkRetries,
		maxRefreshes:    cfg.MaxRefreshes,
		baseBackoff:     cfg.BaseBackoff,
		maxBackoff:      cfg.MaxBackoff,
	}
}

// needRefreshError marks a 403/410 response that may be fixed by refreshing the
// Source.
type needRefreshError struct {
	failure *potoken.HTTPFailure
}

func (e *needRefreshError) Error() string {
	if e.failure != nil {
		return fmt.Sprintf("download: stream URL needs refresh (HTTP %d)", e.failure.StatusCode)
	}
	return "download: stream URL needs refresh"
}

// fetch performs one GET and applies the Source's range strategy to every
// request. A bounded request asks for [start, end]; end < 0 asks for
// bytes=start-, including the initial bytes=0- streaming request. Some media
// origins throttle plain GETs to playback speed but serve ranged requests at full
// speed. On success the caller owns resp.Body. A 403/410 is returned as
// *needRefreshError; other unexpected statuses become a *waxerr.HTTPStatusError
// via the strategy's validation.
//
// The request and the body it returns run under the stall guard: a response that
// does not begin within the window fails here with a *httpx.StallError, and a
// body that stops delivering reports one from Read, so every caller's retry
// ladder sees a stall as the transient failure it is. Closing the body releases
// the guard.
func (d *Downloader) fetch(ctx context.Context, src Source, start, end int64) (*http.Response, error) {
	ctx, watch := httpx.WithStallWatch(ctx, d.stallTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		watch.Stop()
		return nil, err
	}
	for k, vs := range src.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	strategy := src.RangeStrategy
	if strategy == nil {
		strategy = HeaderRange{}
	}
	strategy.Apply(req, start, end)

	resp, err := d.http.Do(req)
	if err != nil {
		watch.Stop()
		return nil, watch.Err(err)
	}
	resp.Body = watch.Body(resp.Body)
	// httpx leaves 403/410 responses to the download layer so expiry can be
	// handled with a Source refresh.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone {
		failure := failureFromResponse(resp)
		drainClose(resp)
		return nil, &needRefreshError{failure: failure}
	}
	if err := strategy.Validate(resp, start, end); err != nil {
		drainClose(resp)
		return nil, err
	}
	return resp, nil
}

// sharedSource holds the current Source and coordinates refreshes across
// concurrent chunk workers.
type sharedSource struct {
	mu       sync.Mutex
	src      Source
	gen      int
	refresh  RefreshFunc
	maxRef   int
	refCount int

	// delivered counts every byte a sink has received, across all workers.
	// lastDelivered is its value at the previous refresh, and sameProgress
	// counts consecutive refreshes with nothing delivered in between.
	// Together they detect a stream no amount of re-signing is moving, and
	// unlike a per-request offset they mean the same thing on the sequential
	// path and under parallel chunks. -1 means no refresh has happened yet,
	// so the first one can never look like a repeat.
	delivered     atomic.Int64
	lastDelivered int64
	sameProgress  int
}

func newSharedSource(src Source, refresh RefreshFunc, maxRef int) *sharedSource {
	return &sharedSource{src: src, refresh: refresh, maxRef: maxRef, lastDelivered: -1}
}

// noteDelivered records n bytes handed to a sink. Both delivery paths call it
// as bytes land, so renew can tell a refresh that follows real progress from
// one that follows nothing.
func (s *sharedSource) noteDelivered(n int64) {
	if n > 0 {
		s.delivered.Add(n)
	}
}

// current returns the live Source and its generation.
func (s *sharedSource) current() (Source, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src, s.gen
}

// errRefreshBudgetSpent marks a refresh declined because the per-download budget
// is used up, as opposed to one that was attempted and failed. Callers treat it as
// a transient condition worth an ordinary retry: see renew's doc for why.
//
// It is a sentinel so callers can test for it, and renew wraps it with the count
// for the user-facing text. The wording avoids claiming the URL expired: reaching
// this point means the server rejected every freshly re-resolved URL, which is a
// server-side refusal rather than a stale signature. It still wraps ErrURLExpired,
// because the exit class and the caller's client-fallback behavior are the same.
var errRefreshBudgetSpent = fmt.Errorf("%w: refresh budget spent", waxerr.ErrURLExpired)

// noProgressLimit is how many consecutive refreshes may run with nothing
// delivered in between before further re-resolves are declined. One repeat is
// allowed because a rotation that works has historically worked on its first
// try, so a single no-progress retry is the escape doing its job; a second says
// the server is refusing the stream rather than the signature.
const noProgressLimit = 2

// refusalError declines a refresh for a stream that no fresh session has
// moved. It is not terminal on its own, which looks like a weaker stop than it
// is: re-resolving is the expensive half (each one is a fresh identity
// bootstrap and player call) and that is what this stops, while the cheap
// in-place retries below it still run, because a burst of stray 403s with no
// bytes in between is indistinguishable from a refusal here and does recover
// on retry. Making this terminal reintroduced exactly that failure
// (TestToFile_TransientForbiddenSurvivesSpentRefreshBudget).
//
// It unwraps to [waxerr.ErrIncompleteStream] because nothing expired: the URLs
// were new every time and the server declined them all, which is the
// incomplete-delivery class the facade already retries on another client (and
// ranks equally with ErrURLExpired for fallback). Wrapping the budget sentinel
// instead put "stream URL expired: refresh budget spent" in front of a message
// whose whole point is that no expiry happened.
type refusalError struct {
	delivered int64 // bytes the whole download had received when it stalled
	sessions  int   // consecutive fresh sessions that added nothing
}

func (e *refusalError) Error() string {
	// Zero delivered is the shape measured in the field (2026-08-31: on the
	// guest path, videos longer than about a minute are refused from the first
	// byte), where "capped at byte 0" would describe a cap that never began
	// rather than a delivery that never started.
	if e.delivered == 0 {
		return fmt.Sprintf("download: the server delivered no bytes on %d consecutive fresh sessions; it is refusing this stream rather than expiring its URL", e.sessions)
	}
	return fmt.Sprintf("download: the server stopped after %d bytes and delivered nothing more on %d consecutive fresh sessions; the delivery cap is holding", e.delivered, e.sessions)
}

func (e *refusalError) Unwrap() error { return waxerr.ErrIncompleteStream }

// refreshDeclined reports a refresh the shared source refused to perform (the
// budget is spent, or fresh sessions are provably not moving the stream), the
// two outcomes the callers' retry ladders treat as retryable rather than
// terminal.
func refreshDeclined(err error) bool {
	if errors.Is(err, errRefreshBudgetSpent) {
		return true
	}
	_, ok := errors.AsType[*refusalError](err)
	return ok
}

// renew refreshes the Source for generation gen. If another worker has already
// advanced the generation, renew returns the current Source without calling
// refresh again. The refresh callback runs under the lock so only one refresh is
// active for a download at a time.
//
// A spent budget returns [errRefreshBudgetSpent] rather than a plain error,
// because the two cases need different handling. googlevideo answers 403 for
// transient reasons as well as for genuine expiry, and the two are
// indistinguishable from the response: a 403 arriving seconds after a fresh
// resolve is far more likely a stray edge-node rejection than a URL that has
// already expired. Since the budget is shared by every chunk of the download, a
// couple of those exhaust it, and the caller must then retry rather than fail.
//
// The bound is a flat count on purpose. An earlier design refunded a refresh
// that had been followed by forward progress, on the theory that the delivery
// cap is positional so legitimate refreshes scale with file size. Measurement
// says otherwise: a session is either capped, in which case it delivers
// essentially nothing however many times its URL is re-signed, or it is fine, in
// which case it delivers the whole file. Refreshes therefore count how many
// sessions were tried, which does not scale with size, and the refund never
// fired on the failure it was written for.
//
// The delivered counter is the same observation read the other way: a budget
// spent with nothing delivered bought nothing, so [noProgressLimit] stops
// re-resolving before the count runs out. See [refusalError] for why that stop
// is not terminal.
func (s *sharedSource) renew(ctx context.Context, gen int, failure *potoken.HTTPFailure) (Source, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return s.src, s.gen, nil // already refreshed by another worker
	}
	if s.refresh == nil {
		return s.src, s.gen, fmt.Errorf("%w: no refresh callback configured", waxerr.ErrURLExpired)
	}
	// A refresh with nothing delivered since the previous one means the session
	// in between served no bytes at all, so more of them only make the same
	// failure slower.
	//
	// A 410 is exempt from the run entirely, check and count alike: there the
	// server said this URL is gone, which is the one case where re-resolving is
	// the documented answer, and letting genuine expiries prime the bail would
	// hand the next ordinary 403 a refusal it did not earn.
	cur := s.delivered.Load()
	repeat := cur == s.lastDelivered
	if repeat && !gone(failure) && s.sameProgress >= noProgressLimit {
		return s.src, s.gen, &refusalError{delivered: cur, sessions: s.sameProgress}
	}
	if s.refCount >= s.maxRef {
		return s.src, s.gen, fmt.Errorf("%w: the server kept rejecting the stream after %d re-resolves", errRefreshBudgetSpent, s.maxRef)
	}
	newSrc, err := s.refresh(ctx, failure)
	if err != nil {
		return s.src, s.gen, err
	}
	if !gone(failure) {
		if repeat {
			s.sameProgress++
		} else {
			s.lastDelivered, s.sameProgress = cur, 1
		}
	}
	s.src = newSrc
	s.gen++
	s.refCount++
	return s.src, s.gen, nil
}

// handleRefresh renews the Source after a 403/410. A nil error means the Source
// was refreshed (or another worker refreshed it) and the caller should retry
// immediately against the new URL, without spending a retry attempt.
//
// A non-nil error is for the caller's ordinary ladder to judge: [retryable]
// accepts a declined refresh (budget spent, or no progress across sessions), so
// that 403 gets the backoff-and-retry it never used to get, and rejects
// everything else the refresh can report. Both download paths share this to
// keep the decision in one place.
func handleRefresh(ctx context.Context, shared *sharedSource, gen int, failure *potoken.HTTPFailure) error {
	_, _, err := shared.renew(ctx, gen, failure)
	return err
}

// gone reports a 410: the server saying this URL is not coming back. googlevideo
// answers 403 for transient reasons as well as for expiry, which is why a spent
// refresh budget falls through to the retry ladder, but a 410 is definitive and
// must not spend it.
func gone(f *potoken.HTTPFailure) bool {
	return f != nil && f.StatusCode == http.StatusGone
}

// retryable reports whether the download layer should retry err. It checks the
// parent context, not errors.Is(err, context.DeadlineExceeded), so a stall,
// which classifies as a deadline, can use the retry budget while the caller's
// context remains live.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, waxerr.ErrRateLimited) {
		return false
	}
	_, ok := errors.AsType[*needRefreshError](err)
	return !ok
}

// ladder is the budget one transfer unit (a chunk span, a sequential stream, a
// ranged block) has for consecutive attempts that fail. A request that fails
// and a body that adds nothing both spend it, so the request ladder and the
// resume loop cannot each run their full length in turn, and bytes landing
// refund it in full, since a transfer that is moving is not failing.
type ladder struct {
	failed int // consecutive attempts that added nothing
}

// retry records a failed attempt. When the budget allows another it waits that
// attempt's backoff and reports true; when the budget is spent it reports
// false. A context error ending the backoff is returned.
func (d *Downloader) retry(ctx context.Context, l *ladder) (bool, error) {
	l.failed++
	if l.failed > d.maxChunkRetries {
		return false, nil
	}
	if err := d.backoff(ctx, l.failed-1); err != nil {
		return false, err
	}
	return true, nil
}

// resume decides whether a transfer whose body ended early may open another
// request, and waits before it does. An attempt that delivered bytes refunds
// the budget but still waits one base step, so an origin that cuts every
// response short is not asked again at once; one that added nothing spends a
// step of the budget.
func (d *Downloader) resume(ctx context.Context, l *ladder, delivered int64) (bool, error) {
	if delivered > 0 {
		l.failed = 0
		return true, d.backoff(ctx, 0)
	}
	return d.retry(ctx, l)
}

// exhausted reports a request ladder that ran out of attempts. Once the source
// has delivered anything, that is an incomplete delivery whatever the last
// failure was: the stream ended before completion, and another client may
// finish it. A source that never delivered fails as its last attempt did, since
// nothing says a stream was there to finish.
func exhausted(shared *sharedSource, cause error) error {
	if n := shared.delivered.Load(); n > 0 {
		return fmt.Errorf("%w: the source stopped answering after %d bytes: %w", waxerr.ErrIncompleteStream, n, cause)
	}
	return cause
}

// backoff sleeps an exponential, attempt-scaled duration, honoring ctx.
func (d *Downloader) backoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(httpx.Backoff(d.baseBackoff, d.maxBackoff, attempt))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// failureFromResponse snapshots an HTTP failure for the refresh callback,
// reading a bounded prefix of the body for diagnostics.
func failureFromResponse(resp *http.Response) *potoken.HTTPFailure {
	f := &potoken.HTTPFailure{StatusCode: resp.StatusCode, Status: resp.Status}
	if resp.Request != nil && resp.Request.URL != nil {
		f.URL = resp.Request.URL.String()
	}
	if resp.Body != nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		f.Body = string(b)
	}
	return f
}

// drainClose reads a small bounded prefix so the connection can be reused, then
// closes the body.
func drainClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
}
