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
	// ErrIncompleteStream indicates that a client returned a detectably truncated
	// stream. Another client may still deliver the complete stream.
	ErrIncompleteStream = waxerr.ErrIncompleteStream

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
	ErrVideoIDTooShort   = waxerr.ErrVideoIDTooShort
	ErrInvalidPlaylistID = waxerr.ErrInvalidPlaylistID
	// ErrPlaylistParse is a maintenance signal, not a bad input: the playlist
	// response parsed but matched no known shape.
	ErrPlaylistParse = waxerr.ErrPlaylistParse

	// Processing / local files.
	ErrIncompatibleSpec = waxerr.ErrIncompatibleSpec
	ErrUnsupportedInput = waxerr.ErrUnsupportedInput
	ErrFFmpegNotFound   = waxerr.ErrFFmpegNotFound

	// ErrInvalidConfig indicates invalid or conflicting library configuration.
	ErrInvalidConfig = waxerr.ErrInvalidConfig
)

// Re-exported structured error types. Use errors.AsType to inspect them.
type (
	RateLimitError   = waxerr.RateLimitError
	ExtractionError  = waxerr.ExtractionError
	PlayabilityError = waxerr.PlayabilityError
	HTTPStatusError  = waxerr.HTTPStatusError
)
