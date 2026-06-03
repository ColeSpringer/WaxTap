// Package waxerr defines WaxTap's domain error taxonomy: a flat set of sentinel
// errors plus a few structured error types that carry diagnostic context.
//
// It is the single source of truth for errors that cross package boundaries:
// extraction (youtube), download, transcode, and the facade all classify
// failures into these values. Keeping the taxonomy in one leaf package (it
// imports only the standard library) lets every layer agree on the same
// errors.Is targets without import cycles, and lets the top-level waxtap package
// re-export the user-facing names.
//
// Classification intent:
//   - "YouTube changed" ([ErrExtractionFailed] / [ErrCipherSolve]) means the
//     maintainer must act. It is distinct from a video simply being unavailable,
//     restricted, or rate-limited.
//   - Expected-in-v1 failures ([ErrVideoRestricted] / [ErrLoginRequired] /
//     [ErrLiveContent]) are never reported as ErrExtractionFailed.
package waxerr

import (
	"errors"
	"fmt"
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
)

// Availability errors are expected failures, not maintenance signals.
var (
	ErrVideoUnavailable = errors.New("waxtap: video unavailable")
	ErrVideoRestricted  = errors.New("waxtap: video restricted")
	ErrLoginRequired    = errors.New("waxtap: login required")
	ErrLiveContent      = errors.New("waxtap: live/upcoming content is not supported")
	ErrNoAudioFormats   = errors.New("waxtap: no audio formats available")
)

// Throttling.
//
// ErrRateLimited marks an HTTP 429 / explicit backoff and is distinct from
// extraction breakage. See [RateLimitError] for the retry-after context.
var ErrRateLimited = errors.New("waxtap: rate limited")

// Input / routing.
var (
	ErrIsPlaylist        = errors.New("waxtap: URL is a playlist; use Enumerate")
	ErrInvalidVideoID    = errors.New("waxtap: invalid characters in video id")
	ErrVideoIDTooShort   = errors.New("waxtap: video id is too short")
	ErrInvalidPlaylistID = errors.New("waxtap: invalid or missing playlist id")
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
	Cause     error
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

// PlayabilityError carries YouTube's playabilityStatus. It unwraps to the
// classified sentinel (e.g. [ErrVideoRestricted], [ErrLoginRequired],
// [ErrVideoUnavailable], [ErrLiveContent]).
type PlayabilityError struct {
	Status   string // YouTube status, e.g. "LOGIN_REQUIRED", "UNPLAYABLE"
	Reason   string // human-readable reason from YouTube
	Sentinel error  // classified sentinel for errors.Is; required
}

func (e *PlayabilityError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("waxtap: %s (status %s)", e.Reason, e.Status)
	}
	return fmt.Sprintf("waxtap: not playable (status %s)", e.Status)
}

func (e *PlayabilityError) Unwrap() error { return e.Sentinel }

// HTTPStatusError reports an unexpected HTTP status from a YouTube,
// googlevideo, or SponsorBlock endpoint.
type HTTPStatusError struct {
	StatusCode int
	Status     string // raw status line, if available
	URL        string
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
