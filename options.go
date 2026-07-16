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

	// Locale sets the InnerTube host language (hl) and content region (gl). The
	// zero value defaults to en / US.
	Locale Locale

	// CacheDir is the base directory for the on-disk player cache. Empty selects
	// os.UserCacheDir()/waxtap.
	CacheDir string
	// DisableDiskCache turns off on-disk player cache reads and writes.
	DisableDiskCache bool

	// TempDir is where intermediate/staging files are written; empty uses the OS
	// temp dir. MaxTempBytes optionally guards total staging size (0 =
	// unlimited). This is useful in constrained containers.
	TempDir string
	// MaxTempBytes limits temporary staging bytes. Zero disables the limit.
	MaxTempBytes int64

	Concurrency Concurrency // limits parallel network and audio-processing work
	Timeouts    Timeouts    // sets per-operation deadlines
	Retry       RetryPolicy // tunes HTTP retries and backoff
	Politeness  Politeness  // limits request rate and applies cooldowns

	// ProfileOverridePath points at a strict JSON file that replaces the built-in
	// YouTube client profile chain at startup. Use it to refresh client versions,
	// user agents, or device fingerprints without rebuilding.
	ProfileOverridePath string

	// ChromeMajor overrides the emulated Chrome major for the built-in WEB-family
	// client identities. Zero selects the built-in default.
	//
	// The value applies to the default profile chain and to built-in WEB requests
	// used for discovery and fallbacks. It does not modify profiles loaded from
	// ProfileOverridePath, so the two options cannot be combined. New rejects
	// values outside 0..999.
	ChromeMajor int

	SponsorBlock SponsorBlockOptions // configures SponsorBlock API access

	// POTokenProvider supplies PO tokens for profiles that require them. WaxTap may
	// call it during extraction for a player-scope token and during resolution for
	// a GVS-scope stream token. When a download refresh follows a 403, WaxTap
	// passes the failure details in the request. Nil means no provider is
	// configured.
	POTokenProvider POTokenProvider

	// PlayerContextProvider enables the opt-in WEB SABR audio path: it supplies an
	// attested /player streaming context (from an external attesting browser such
	// as WaxSeal) that WaxTap streams Go-side. When set, WaxTap tries the WEB
	// context first (even over a forced Client, whose chain stays the fallback)
	// and falls back to the normal extraction chain on a context failure. It
	// needs a GVS PO-token provider too (the stream binds a GVS token to the
	// context's visitorData); New rejects a configuration without one, because
	// the token mint happens at SABR setup, past the fallback boundary. Nil
	// leaves WaxTap on its default chain.
	PlayerContextProvider PlayerContextProvider

	// Client, when non-empty, forces a single built-in client as the whole
	// strategy chain instead of the default multi-client fallback. Valid values
	// are "web", "ios", "android_vr", and "web_embedded". It applies the built-in
	// WEB-family User-Agent / ChromeMajor treatment. It is mutually exclusive with
	// ProfileOverridePath. A configured PlayerContextProvider is tried before
	// this chain; the forced client serves as its fallback.
	Client string

	// Session is an externally supplied guest identity (visitorData + cookies)
	// WaxTap adopts verbatim instead of bootstrapping its own, for byte-exact
	// session coherence with a PO-token minter. Session.VisitorData must be the
	// browser's exact X-Goog-Visitor-Id literal (the URL-escaped form in
	// ytcfg.VISITOR_DATA); it is re-sent with no escape/unescape.
	//
	// Adoption requires a uniform client chain (set Client, or a ProfileOverridePath
	// whose profiles are all one InnerTube client); the default multi-client chain
	// is rejected so an adopted session is never routed through a different client.
	// If resolution fails, extraction aborts rather than falling back to a random
	// synthetic visitorData. Adopted cookies need an HTTPClient with a cookie jar;
	// login cookies are dropped (adoption assumes a guest session). The adopted
	// session is resolved once per Client, so long-running services should recreate
	// the Client per task. Mutually exclusive with SessionProvider.
	Session *POTokenSession

	// SessionProvider resolves the adopted guest identity lazily, at most once per
	// Client (cached on success). It is the pull-based form of Session and shares
	// its uniform-chain requirement. Mutually exclusive with Session.
	SessionProvider POTokenSessionProvider
}

// Locale sets InnerTube localization hints. The zero value uses en / US.
//
// HL affects localized UI text YouTube returns, including some error reasons.
// Availability classification of members-only and geo-blocked videos matches
// those reason strings and is tuned for English, so a non-English HL may report
// [ErrVideoUnavailable] instead of [ErrMembersOnly] or [ErrGeoBlocked] (both are
// still skip-class verdicts). GL is a content-region hint; it does not change the
// request IP or bypass geo restrictions. Titles and descriptions are usually
// returned as-authored.
type Locale struct {
	HL string // host language, e.g. "en", "de", "ja"
	GL string // content region, e.g. "US", "DE", "JP"
}

// Concurrency bounds parallel work. Zero values select conservative defaults at
// New time.
type Concurrency struct {
	// Downloads is the max simultaneous downloads (e.g. across a playlist run).
	Downloads int
	// Chunks is the max parallel ranged chunks within a single download. Kept
	// low by default, especially for CLI playlist runs.
	Chunks int
	// Procs limits concurrent in-process audio operations (transcode, remux,
	// analyze), guarding local CPU independently from network parallelism. Each
	// operation is one goroutine, so this bounds CPU and peak memory. Zero selects
	// a conservative default (GOMAXPROCS); a negative value disables the limit.
	Procs int
}

// Timeouts are per-operation deadlines applied through context. There is no
// single global download cap; each operation gets its own budget. A zero field
// means WaxTap adds no extra deadline for that operation.
type Timeouts struct {
	Extraction   time.Duration // player-response fetch + parse
	Resolve      time.Duration // stream-URL resolution (incl. cipher JS)
	WebContext   time.Duration // per attested /player-context fetch, mid-stream re-fetches included
	SponsorBlock time.Duration // SponsorBlock fetch (see also SponsorBlock.Timeout)
	ChunkRetry   time.Duration // per-chunk deadline for ranged downloads
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
	// Cooldown pauses requests to a host after HTTP 429, or after HTTP 503/403
	// with a Retry-After header. A longer Retry-After value takes precedence, up
	// to RetryPolicy.MaxRetryWait. Zero disables the cooldown.
	Cooldown time.Duration
}

// SponsorBlockOptions configures the SponsorBlock client.
type SponsorBlockOptions struct {
	// BaseURL overrides the SponsorBlock API base URL (empty = public default).
	BaseURL string
	// Timeout is a strict per-fetch timeout; if set it takes precedence over
	// Timeouts.SponsorBlock.
	Timeout time.Duration
}
