package waxtap

import (
	"log/slog"
	"net/http"
	"time"
)

// Options configures a Client. The zero value is usable; New fills in defaults
// for timeouts, retry policy, and limits. All fields are read once during New.
type Options struct {
	// HTTPClient is used for all requests. It should set a DialContext and a
	// conservative Timeout, or rely on the per-operation context deadlines
	// WaxTap applies (see Timeouts). A no-timeout client on a dead proxy cannot
	// leak goroutines because WaxTap still bounds each operation by context. If
	// nil, a default client is used.
	HTTPClient *http.Client

	// Logger receives structured logs. If nil, logging is discarded; the CLI
	// installs its own handler.
	Logger *slog.Logger

	// CacheDir holds the on-disk player cache. Empty selects
	// os.UserCacheDir()/waxtap. The disk cache is default-on for the CLI and
	// opt-in for library use via DisableDiskCache.
	CacheDir         string
	DisableDiskCache bool

	// TempDir is where intermediate/staging files are written; empty uses the OS
	// temp dir. MaxTempBytes optionally guards total staging size (0 =
	// unlimited). This is useful in constrained containers.
	TempDir      string
	MaxTempBytes int64

	Concurrency Concurrency
	Timeouts    Timeouts
	Retry       RetryPolicy
	Politeness  Politeness

	// ProfileOverridePath points at a runtime client-profile override file. It
	// allows a deployment to update client versions or headers with config and a
	// restart instead of a rebuild.
	ProfileOverridePath string

	SponsorBlock SponsorBlockOptions

	// POTokenProvider supplies PO tokens on a 403 (v1: nil = none configured). It
	// receives only stable public structs (POTokenRequest/POTokenResponse), not
	// WaxTap's internal client profile or session.
	POTokenProvider POTokenProvider
}

// Concurrency bounds parallel work. Zero values select conservative defaults at
// New time.
type Concurrency struct {
	// Downloads is the max simultaneous downloads (e.g. across a playlist run).
	Downloads int
	// Chunks is the max parallel ranged chunks within a single download. Kept
	// low by default, especially for CLI playlist runs.
	Chunks int
	// FFmpeg limits concurrent ffmpeg/ffprobe processes. This guards local CPU
	// independently from network parallelism.
	FFmpeg int
}

// Timeouts are per-operation deadlines applied through context. There is no
// single global download cap; each operation gets its own budget. A zero field
// means WaxTap adds no extra deadline for that operation.
type Timeouts struct {
	Extraction     time.Duration // player-response fetch + parse
	Resolve        time.Duration // stream-URL resolution (incl. cipher JS)
	SponsorBlock   time.Duration // SponsorBlock fetch (see also SponsorBlock.Timeout)
	ChunkRetry     time.Duration // per-chunk deadline for ranged downloads
	FFmpegShutdown time.Duration // grace period before killing ffmpeg on cancel
}

// RetryPolicy tunes HTTP retry/backoff.
type RetryPolicy struct {
	MaxRetries  int           // additional attempts after the first
	BaseBackoff time.Duration // base of the exponential backoff
	MaxBackoff  time.Duration // cap on a single backoff sleep

	// MaxRetryWait caps an honored Retry-After. Beyond it WaxTap fails fast with
	// a *RateLimitError instead of sleeping a goroutine. Some Retry-After values
	// can be hours long.
	MaxRetryWait time.Duration
}

// Politeness governs request volume and backoff. The posture is to be a
// well-behaved client (reduce load, honor backoff, stop when limited), not to
// evade detection.
type Politeness struct {
	// PerHostQPS throttles requests per host (0 = unlimited). youtube.com and
	// googlevideo.com are limited independently.
	PerHostQPS float64
	// Cooldown pauses a host's queue after a 429/403 burst.
	Cooldown time.Duration
	// MaxDownloadsPerRun caps a single playlist run (0 = unlimited).
	MaxDownloadsPerRun int
}

// SponsorBlockOptions configures the SponsorBlock client.
type SponsorBlockOptions struct {
	// BaseURL overrides the SponsorBlock API base URL (empty = public default).
	BaseURL string
	// Timeout is a strict per-fetch timeout; if set it takes precedence over
	// Timeouts.SponsorBlock.
	Timeout time.Duration
}
