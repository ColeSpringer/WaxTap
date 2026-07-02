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
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/colespringer/waxtap/v2/internal/httpx"
	"github.com/colespringer/waxtap/v2/potoken"
	"github.com/colespringer/waxtap/v2/waxerr"
)

// Defaults applied by New when a Config field is left zero.
const (
	defaultChunkSize       = 10 << 20 // 10 MiB per ranged chunk
	defaultParallelism     = 4        // parallel chunks within one ToFile download
	defaultMaxChunkRetries = 3        // per-chunk retry attempts after the first
	defaultMaxRefreshes    = 2        // URL re-resolves allowed per download
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
	// ChunkTimeout bounds one ToFile chunk request, including headers and body. 0
	// means no extra deadline beyond the request context. Streaming methods do
	// not apply this timeout.
	ChunkTimeout time.Duration

	// MaxChunkRetries is the per-chunk retry budget for transient failures after
	// the first request (default 3). Expiry refreshes are counted separately.
	MaxChunkRetries int
	// MaxRefreshes caps URL refreshes per download (default 2).
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
	chunkTimeout    time.Duration
	maxChunkRetries int
	maxRefreshes    int
	baseBackoff     time.Duration
	maxBackoff      time.Duration
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
		chunkTimeout:    cfg.ChunkTimeout,
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
func (d *Downloader) fetch(ctx context.Context, src Source, start, end int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
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
		return nil, err
	}
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
}

func newSharedSource(src Source, refresh RefreshFunc, maxRef int) *sharedSource {
	return &sharedSource{src: src, refresh: refresh, maxRef: maxRef}
}

// current returns the live Source and its generation.
func (s *sharedSource) current() (Source, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src, s.gen
}

// renew refreshes the Source for generation gen. If another worker has already
// advanced the generation, renew returns the current Source without calling
// refresh again. The refresh callback runs under the lock so only one refresh is
// active for a download at a time.
func (s *sharedSource) renew(ctx context.Context, gen int, failure *potoken.HTTPFailure) (Source, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return s.src, s.gen, nil // already refreshed by another worker
	}
	if s.refresh == nil {
		return s.src, s.gen, fmt.Errorf("%w: no refresh callback configured", waxerr.ErrURLExpired)
	}
	if s.refCount >= s.maxRef {
		return s.src, s.gen, fmt.Errorf("%w: exhausted %d refresh attempts", waxerr.ErrURLExpired, s.maxRef)
	}
	newSrc, err := s.refresh(ctx, failure)
	if err != nil {
		return s.src, s.gen, err
	}
	s.src = newSrc
	s.gen++
	s.refCount++
	return s.src, s.gen, nil
}

// retryable reports whether the download layer should retry err. It checks the
// parent context, not errors.Is(err, context.DeadlineExceeded), so a per-chunk
// timeout can use the retry budget while the caller's context remains live.
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

// backoff sleeps an exponential, attempt-scaled duration, honoring ctx.
func (d *Downloader) backoff(ctx context.Context, attempt int) error {
	shift := min(attempt, 16)
	dur := d.baseBackoff << shift
	if dur <= 0 || dur > d.maxBackoff {
		dur = d.maxBackoff
	}
	t := time.NewTimer(dur)
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
