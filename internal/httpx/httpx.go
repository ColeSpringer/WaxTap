// Package httpx is WaxTap's internal HTTP client wrapper. It adds retry,
// exponential backoff, rate-limit handling, and an optional per-host limiter on
// top of an injected *http.Client.
//
// Design notes:
//   - No global timeout is imposed here. Callers set per-operation deadlines on
//     request contexts, so a long legitimate download is not killed by an
//     unrelated global cap while a hung socket still cannot leak a goroutine.
//   - Retry-After is honored but capped by MaxRetryWait: YouTube can demand
//     hours on an IP ban, so beyond the cap we fail fast with
//     *waxerr.RateLimitError instead of sleeping a worker.
//   - Retried requests must be replayable: a request with a body but no
//     GetBody is attempted exactly once.
package httpx

import (
	"context"
	"io"
	"log/slog"
	rand "math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// Default tuning, applied by New when a Config field is left zero.
const (
	defaultMaxRetries   = 3
	defaultBaseBackoff  = 250 * time.Millisecond
	defaultMaxBackoff   = 10 * time.Second
	defaultMaxRetryWait = 30 * time.Second
)

// Limiter gates outbound requests. Wait blocks until a request may proceed or
// ctx is done. Penalize pauses requests to one host for at least d.
type Limiter interface {
	// Wait blocks until host may receive another request or ctx is canceled.
	Wait(ctx context.Context, host string) error
	// Penalize pauses requests to host for at least d.
	Penalize(host string, d time.Duration)
}

// NopLimiter performs no rate limiting.
type NopLimiter struct{}

// Wait implements Limiter.
func (NopLimiter) Wait(context.Context, string) error { return nil }

// Penalize implements Limiter.
func (NopLimiter) Penalize(string, time.Duration) {}

// ThrottlePhase identifies when a throttle event occurred.
type ThrottlePhase int

const (
	// ThrottleDetected is reported after a rate-limit response is received and
	// the host is penalized.
	ThrottleDetected ThrottlePhase = iota
	// ThrottleRetryStarted is reported when a retry begins after backoff.
	ThrottleRetryStarted
)

// ThrottleEvent describes rate limiting by one host.
type ThrottleEvent struct {
	Host       string        // request host
	StatusCode int           // rate-limit response status
	RetryAfter time.Duration // parsed Retry-After; zero if missing or invalid
	Penalty    time.Duration // duration passed to Limiter.Penalize
	Phase      ThrottlePhase // point in the throttle lifecycle
}

// ThrottleHook receives throttle events. It may be called from parallel request
// workers, so it must be safe for concurrent use.
type ThrottleHook func(ThrottleEvent)

type throttleHookKey struct{}

// WithThrottleHook returns a context that reports throttle events to h. A nil
// hook clears an inherited hook.
func WithThrottleHook(ctx context.Context, h ThrottleHook) context.Context {
	return context.WithValue(ctx, throttleHookKey{}, h)
}

// throttleHookFrom returns the throttle hook on ctx.
func throttleHookFrom(ctx context.Context) ThrottleHook {
	h, _ := ctx.Value(throttleHookKey{}).(ThrottleHook)
	return h
}

// fireThrottle sends ev to the context's hook, if any.
func fireThrottle(ctx context.Context, ev ThrottleEvent) {
	if h := throttleHookFrom(ctx); h != nil {
		h(ev)
	}
}

// Config configures a Client. The zero value is usable; New fills sane defaults.
type Config struct {
	// HTTPClient is the underlying client. It should set a DialContext and a
	// conservative Timeout (or rely on per-request context deadlines). If nil,
	// http.DefaultClient is used.
	HTTPClient *http.Client
	// Logger receives debug logs for retries/backoff. If nil, logs are discarded.
	Logger *slog.Logger
	// Limiter gates requests per host. If nil, no limiting is applied.
	Limiter Limiter

	// MaxRetries is the number of additional attempts after the first.
	MaxRetries int
	// BaseBackoff is the base of the exponential backoff schedule.
	BaseBackoff time.Duration
	// MaxBackoff caps a single backoff sleep.
	MaxBackoff time.Duration
	// MaxRetryWait caps an honored Retry-After. Beyond it, Do fails fast with
	// *waxerr.RateLimitError rather than sleeping.
	MaxRetryWait time.Duration

	// Cooldown is the minimum host penalty after a rate-limit response. A longer
	// Retry-After value takes precedence, up to MaxRetryWait. Zero disables the
	// base cooldown.
	Cooldown time.Duration
}

// Client performs HTTP requests with retry, backoff, and rate-limit handling.
type Client struct {
	http *http.Client
	log  *slog.Logger
	lim  Limiter
	cfg  Config
}

// Jar returns the cookie jar of the underlying *http.Client, or nil if it has
// none. Higher layers use this to decide whether a cookie-dependent session
// bootstrap is worthwhile (and where to seed cookies).
func (c *Client) Jar() http.CookieJar { return c.http.Jar }

// New returns a Client, filling unset Config fields with defaults.
func New(cfg Config) *Client {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Limiter == nil {
		cfg.Limiter = NopLimiter{}
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaultMaxRetries
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = defaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	if cfg.MaxRetryWait <= 0 {
		cfg.MaxRetryWait = defaultMaxRetryWait
	}
	return &Client{http: cfg.HTTPClient, log: cfg.Logger, lim: cfg.Limiter, cfg: cfg}
}

// Do executes req with retry/backoff and rate-limit handling. The request
// context governs cancellation and per-operation deadlines; a context error is
// never retried. On a capped-out Retry-After it returns *waxerr.RateLimitError.
//
// On success the caller owns the returned response body. On retried or
// rate-limited responses the intermediate bodies are drained and closed here.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	host := req.URL.Hostname()

	attempts := c.cfg.MaxRetries + 1
	if req.Body != nil && req.GetBody == nil {
		attempts = 1 // non-replayable body: attempt once
	}

	var lastErr error
	var rlRetryStatus int // status of a pending rate-limit retry, 0 if none
	for attempt := 0; attempt < attempts; attempt++ {
		if rlRetryStatus != 0 {
			// Reaching the next iteration means backoff completed without
			// cancellation and the retry is starting.
			fireThrottle(ctx, ThrottleEvent{Host: host, StatusCode: rlRetryStatus, Phase: ThrottleRetryStarted})
			rlRetryStatus = 0
		}
		if attempt > 0 && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}

		if err := c.lim.Wait(ctx, host); err != nil {
			return nil, err
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			if attempt < attempts-1 {
				c.log.DebugContext(ctx, "httpx: transport error, retrying", "host", host, "attempt", attempt, "err", err)
				if werr := c.backoff(ctx, attempt); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, err
		}

		// Rate limited: 429 always; 503/403 only when paired with Retry-After.
		// A bare 403 passes through so resolver/download code can classify it
		// as PO-token-required, URL-expired, or another failure. A 403 carrying
		// Retry-After is treated as throttling here.
		retryAfterPresent := resp.Header.Get("Retry-After") != ""
		if resp.StatusCode == http.StatusTooManyRequests ||
			(retryAfterPresent && (resp.StatusCode == http.StatusServiceUnavailable ||
				resp.StatusCode == http.StatusForbidden)) {
			wait, ok := parseRetryAfter(resp)
			status := resp.StatusCode
			drain(resp)
			rlErr := &waxerr.RateLimitError{Host: host, RetryAfter: wait, StatusCode: status}

			// Retry-After can extend the base cooldown, but not beyond the maximum
			// retry wait.
			penalty := c.cfg.Cooldown
			if ok {
				penalty = max(penalty, min(wait, c.cfg.MaxRetryWait))
			}
			c.lim.Penalize(host, penalty)
			fireThrottle(ctx, ThrottleEvent{
				Host: host, StatusCode: status, RetryAfter: wait, Penalty: penalty, Phase: ThrottleDetected,
			})

			if ok && wait > c.cfg.MaxRetryWait {
				return nil, rlErr // fail fast rather than sleep for a ban
			}
			lastErr = rlErr
			if attempt < attempts-1 {
				sleepFor := wait
				if !ok {
					sleepFor = c.backoffDuration(attempt)
				}
				rlRetryStatus = status
				c.log.DebugContext(ctx, "httpx: rate limited, backing off", "host", host, "wait", sleepFor)
				if werr := Sleep(ctx, sleepFor); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, rlErr
		}

		// Retryable server-side error.
		if attempt < attempts-1 && retryableStatus(resp.StatusCode) {
			drain(resp)
			lastErr = &waxerr.HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: req.URL.String()}
			c.log.DebugContext(ctx, "httpx: server error, retrying", "host", host, "status", resp.StatusCode, "attempt", attempt)
			if werr := c.backoff(ctx, attempt); werr != nil {
				return nil, werr
			}
			continue
		}

		return resp, nil
	}
	return nil, lastErr
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		return false
	}
}

// backoffDuration computes a full-jitter exponential backoff for the attempt.
func (c *Client) backoffDuration(attempt int) time.Duration {
	shift := min(attempt, 16)
	d := c.cfg.BaseBackoff << shift
	if d <= 0 {
		d = c.cfg.MaxBackoff // overflow safety
	} else {
		d = min(d, c.cfg.MaxBackoff)
	}
	// Full jitter in (0, d].
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

func (c *Client) backoff(ctx context.Context, attempt int) error {
	return Sleep(ctx, c.backoffDuration(attempt))
}

// Sleep waits for d or until ctx is done, whichever comes first, returning the
// context error on cancellation. A non-positive d returns immediately.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseRetryAfter reads the Retry-After header as either delta-seconds or an
// HTTP date. The bool reports whether a value was present and parseable.
func parseRetryAfter(resp *http.Response) (time.Duration, bool) {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0), true
	}
	return 0, false
}

// drain reads a bounded amount of the body so the connection can be reused,
// then closes it.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)
	_ = resp.Body.Close()
}
