package waxtap

import (
	"errors"

	"github.com/colespringer/waxtap/waxerr"
)

// ErrNotImplemented marks facade methods that are declared but not implemented
// yet.
var ErrNotImplemented = errors.New("waxtap: not implemented")

// Re-exported sentinel errors. The canonical definitions live in package waxerr;
// match them with errors.Is.
var (
	// YouTube extraction maintenance signals.
	ErrExtractionFailed = waxerr.ErrExtractionFailed
	ErrCipherSolve      = waxerr.ErrCipherSolve
	ErrNeedsPOToken     = waxerr.ErrNeedsPOToken
	ErrURLExpired       = waxerr.ErrURLExpired

	// Availability failures.
	ErrVideoUnavailable = waxerr.ErrVideoUnavailable
	ErrVideoRestricted  = waxerr.ErrVideoRestricted
	ErrLoginRequired    = waxerr.ErrLoginRequired
	ErrLiveContent      = waxerr.ErrLiveContent
	ErrNoAudioFormats   = waxerr.ErrNoAudioFormats

	// Throttling.
	ErrRateLimited = waxerr.ErrRateLimited

	// Input / routing.
	ErrIsPlaylist        = waxerr.ErrIsPlaylist
	ErrInvalidVideoID    = waxerr.ErrInvalidVideoID
	ErrInvalidPlaylistID = waxerr.ErrInvalidPlaylistID

	// Processing / local files.
	ErrIncompatibleSpec = waxerr.ErrIncompatibleSpec
	ErrUnsupportedInput = waxerr.ErrUnsupportedInput
	ErrFFmpegNotFound   = waxerr.ErrFFmpegNotFound
)

// Re-exported structured error types. Use errors.As to inspect them.
type (
	RateLimitError   = waxerr.RateLimitError
	ExtractionError  = waxerr.ExtractionError
	PlayabilityError = waxerr.PlayabilityError
	HTTPStatusError  = waxerr.HTTPStatusError
)
