// Package waxerr defines the sentinel and structured errors shared by WaxTap's
// extraction, download, and processing packages.
//
// [ErrExtractionFailed] and [ErrCipherSolve] indicate likely extractor
// breakage. Availability, login, live-content, and rate-limit failures use
// separate errors so callers can handle them without treating them as
// maintenance incidents.
package waxerr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Extraction and resolution errors produced by the youtube package and resolver.
var (
	// ErrExtractionFailed indicates YouTube likely changed its surface and the
	// extractor needs maintenance. See [ExtractionError] for context.
	ErrExtractionFailed = errors.New("waxtap: extraction failed (YouTube may have changed)")
	// ErrCipherSolve indicates the signature / n-parameter transform could not
	// be solved from the player JS. It is a maintenance signal like
	// ErrExtractionFailed.
	ErrCipherSolve = errors.New("waxtap: cipher solve failed")
	// ErrNeedsPOToken indicates a stream URL requires a PO token that was not
	// available (no provider configured, or the provider returned none).
	ErrNeedsPOToken = errors.New("waxtap: PO token required")
	// ErrURLExpired indicates a signed stream URL expired and re-resolution failed.
	ErrURLExpired = errors.New("waxtap: stream URL expired")
	// ErrIncompleteStream indicates that a client returned a detectably truncated
	// stream. Another client may still deliver the complete stream.
	ErrIncompleteStream = errors.New("waxtap: stream ended before completion")
	// ErrChainExhausted indicates that an exclusion-aware extraction has no
	// remaining attempts. Callers should report the errors from earlier attempts.
	ErrChainExhausted = errors.New("waxtap: no extraction attempts remain")
)

// Availability errors are expected failures, not maintenance signals.
var (
	ErrVideoUnavailable = errors.New("waxtap: video unavailable")
	ErrVideoRestricted  = errors.New("waxtap: video restricted")
	ErrLoginRequired    = errors.New("waxtap: login required")
	ErrLiveContent      = errors.New("waxtap: live/upcoming content is not supported")
	ErrNoAudioFormats   = errors.New("waxtap: no audio formats available")
	// ErrRequestedFormatUnavailable indicates an explicit --itag/--codec selector
	// matched none of the available audio formats. Unlike ErrNoAudioFormats, it
	// means usable audio exists but not in the requested format.
	ErrRequestedFormatUnavailable = errors.New("waxtap: requested format unavailable")
)

// Throttling.
//
// ErrRateLimited marks an HTTP 429 / explicit backoff and is distinct from
// extraction breakage. See [RateLimitError] for the retry-after context.
var ErrRateLimited = errors.New("waxtap: rate limited")

// Input / routing.
var (
	ErrIsPlaylist        = errors.New("waxtap: URL is a playlist; use Enumerate")
	ErrInvalidVideoID    = errors.New("waxtap: invalid characters in video ID")
	ErrVideoIDTooShort   = errors.New("waxtap: video ID is too short")
	ErrInvalidPlaylistID = errors.New("waxtap: invalid or missing playlist ID")
	// ErrPlaylistParse indicates a playlist browse response that parsed as JSON
	// but matched no known item shape. Unlike ErrInvalidPlaylistID it is a
	// maintenance signal in the ErrExtractionFailed class: YouTube likely
	// changed the playlist page and the parser needs updating.
	ErrPlaylistParse = errors.New("waxtap: playlist response shape not recognized (parser may be stale)")
	// ErrPlaylistUnavailable indicates that YouTube reports a playlist as private,
	// deleted, or otherwise inaccessible. See [PlaylistUnavailableError].
	ErrPlaylistUnavailable = errors.New("waxtap: playlist unavailable")
	// ErrPlaylistEmpty indicates a valid playlist that contains no videos.
	ErrPlaylistEmpty = errors.New("waxtap: playlist has no videos")
)

// Processing / local files.
var (
	// ErrIncompatibleSpec indicates a ProcessSpec combination that cannot be
	// honored (e.g. stream-copy together with loudness Apply, which requires an
	// encode).
	ErrIncompatibleSpec = errors.New("waxtap: incompatible processing spec")
	// ErrUnsupportedInput indicates a local input that is corrupt, unsupported,
	// or has no usable audio stream.
	ErrUnsupportedInput = errors.New("waxtap: unsupported or unreadable input")
	// ErrFFmpegNotFound indicates the ffmpeg / ffprobe binaries were not found.
	ErrFFmpegNotFound = errors.New("waxtap: ffmpeg/ffprobe not found")
	// ErrInvalidConfig indicates invalid or conflicting configuration or option
	// values.
	ErrInvalidConfig = errors.New("waxtap: invalid configuration")
	// ErrDeliveryUnsupported indicates the selected client can extract metadata and
	// formats but cannot deliver media bytes in the current configuration.
	ErrDeliveryUnsupported = errors.New("waxtap: byte delivery not supported for this client")
)

// RateLimitError carries backoff context for an HTTP 429 (or a 403 that the
// server pairs with a Retry-After). It unwraps to [ErrRateLimited], so
// errors.Is(err, ErrRateLimited) holds.
type RateLimitError struct {
	Host       string        // host that throttled us, if known
	RetryAfter time.Duration // server-requested wait (0 if none/unknown)
	StatusCode int           // originating status code
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("waxtap: rate limited by %s (retry after %s)", hostOr(e.Host), e.RetryAfter)
	}
	return fmt.Sprintf("waxtap: rate limited by %s", hostOr(e.Host))
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// ExtractionError wraps a lower-level cause with the extraction stage and the
// player URL in play, to speed up "YouTube broke us" diagnosis. It unwraps to
// both [ErrExtractionFailed] and the underlying cause.
type ExtractionError struct {
	Stage     string // e.g. "player-response", "format-parse"
	PlayerURL string // base.js URL in play, if known
	Cause     error  // underlying extraction failure
}

func (e *ExtractionError) Error() string {
	switch {
	case e.PlayerURL != "" && e.Cause != nil:
		return fmt.Sprintf("waxtap: extraction failed at %q (player %s): %v", e.Stage, e.PlayerURL, e.Cause)
	case e.Cause != nil:
		return fmt.Sprintf("waxtap: extraction failed at %q: %v", e.Stage, e.Cause)
	default:
		return fmt.Sprintf("waxtap: extraction failed at %q", e.Stage)
	}
}

func (e *ExtractionError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrExtractionFailed, e.Cause}
	}
	return []error{ErrExtractionFailed}
}

// ProviderError reports a failed call to a player-context or session provider.
// It unwraps to the underlying cause, allowing transport and HTTP errors to keep
// their own classification.
type ProviderError struct {
	Endpoint string // provider that failed, e.g. "player-context", "session"
	Cause    error  // underlying transport or HTTP failure
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("waxtap: %s provider failed: %v", e.Endpoint, e.Cause)
}

func (e *ProviderError) Unwrap() error { return e.Cause }

// RequestedFormatError reports that an explicit itag/codec selector matched no
// available audio format. It lists the available alternatives and unwraps to
// [ErrRequestedFormatUnavailable].
type RequestedFormatError struct {
	Selector string   // the selector that found no match, e.g. "itag(999)"
	Itags    []int    // available audio itags
	Codecs   []string // available audio codec families
}

func (e *RequestedFormatError) Error() string {
	msg := fmt.Sprintf("waxtap: requested format %s is not available", e.Selector)
	switch {
	case len(e.Itags) > 0:
		parts := make([]string, len(e.Itags))
		for i, it := range e.Itags {
			parts[i] = strconv.Itoa(it)
		}
		return msg + " (available itags: " + strings.Join(parts, ", ") + ")"
	case len(e.Codecs) > 0:
		return msg + " (available codecs: " + strings.Join(e.Codecs, ", ") + ")"
	default:
		return msg
	}
}

func (e *RequestedFormatError) Unwrap() error { return ErrRequestedFormatUnavailable }

// PlayabilityError carries YouTube's playabilityStatus. It unwraps to the
// classified sentinel (e.g. [ErrVideoRestricted], [ErrLoginRequired],
// [ErrVideoUnavailable], [ErrLiveContent]).
type PlayabilityError struct {
	Status   string // YouTube status, e.g. "LOGIN_REQUIRED", "UNPLAYABLE"
	Reason   string // human-readable reason from YouTube
	Sentinel error  // classified sentinel for errors.Is; required
	// Embed reports that the WEB_EMBEDDED client returned the error. Callers can use
	// it to provide fallback guidance without interpreting Reason.
	Embed bool
}

func (e *PlayabilityError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("waxtap: %s (status %s)", e.Reason, e.Status)
	}
	return fmt.Sprintf("waxtap: not playable (status %s)", e.Status)
}

func (e *PlayabilityError) Unwrap() error { return e.Sentinel }

// PlaylistUnavailableError reports YouTube's reason for an inaccessible
// playlist. It unwraps to [ErrPlaylistUnavailable].
type PlaylistUnavailableError struct {
	Reason string // human-readable reason from YouTube's alert
}

func (e *PlaylistUnavailableError) Error() string {
	if e.Reason != "" {
		return "waxtap: " + e.Reason
	}
	return ErrPlaylistUnavailable.Error()
}

func (e *PlaylistUnavailableError) Unwrap() error { return ErrPlaylistUnavailable }

// HTTPStatusError reports an unexpected HTTP status from a YouTube,
// googlevideo, or SponsorBlock endpoint.
type HTTPStatusError struct {
	StatusCode int    // numeric HTTP status code
	Status     string // raw status line, if available
	URL        string // endpoint that returned the status, if known
}

func (e *HTTPStatusError) Error() string {
	status := e.Status
	if status == "" {
		status = fmt.Sprintf("%d", e.StatusCode)
	}
	if e.URL != "" {
		return fmt.Sprintf("waxtap: unexpected HTTP status %s for %s", status, e.URL)
	}
	return fmt.Sprintf("waxtap: unexpected HTTP status %s", status)
}

func hostOr(host string) string {
	if host == "" {
		return "server"
	}
	return host
}

// PreferErr returns the more informative of two terminal errors. From highest to
// lowest precedence:
//
//   - availability verdicts
//   - extraction, cipher, or parse failures
//   - incomplete delivery (ErrIncompleteStream or ErrURLExpired)
//   - generic or network failures
//   - ErrNeedsPOToken
//
// A non-nil error wins over nil, and ties preserve a. Callers must handle
// cancellation and rate limiting before calling PreferErr.
func PreferErr(a, b error) error {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case errRank(b) > errRank(a):
		return b
	default:
		return a
	}
}

// errRank returns the precedence used by PreferErr.
func errRank(err error) int {
	switch {
	case errors.Is(err, ErrVideoUnavailable),
		errors.Is(err, ErrVideoRestricted),
		errors.Is(err, ErrLoginRequired),
		errors.Is(err, ErrLiveContent),
		errors.Is(err, ErrNoAudioFormats),
		// A requested-format miss proves that extraction succeeded, so it outranks
		// availability errors from other clients.
		errors.Is(err, ErrRequestedFormatUnavailable):
		return 5
	case errors.Is(err, ErrExtractionFailed),
		errors.Is(err, ErrCipherSolve),
		errors.Is(err, ErrPlaylistParse):
		return 4
	case errors.Is(err, ErrIncompleteStream),
		errors.Is(err, ErrURLExpired):
		// An expired URL is an incomplete delivery when no retry succeeds.
		return 3
	case errors.Is(err, ErrNeedsPOToken):
		return 1 // lowest: a precondition, not a diagnosis
	default:
		return 2 // generic / network
	}
}
