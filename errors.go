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
	// ErrRequestedFormatUnavailable indicates an explicit itag/codec selector
	// matched none of the available audio formats. It is distinct from
	// ErrNoAudioFormats, which means no usable audio formats exist.
	ErrRequestedFormatUnavailable = waxerr.ErrRequestedFormatUnavailable

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
	// ErrPlaylistUnavailable indicates that a playlist is private, deleted, or
	// otherwise inaccessible. See [PlaylistUnavailableError].
	ErrPlaylistUnavailable = waxerr.ErrPlaylistUnavailable
	// ErrPlaylistEmpty indicates that a valid playlist contains no videos.
	ErrPlaylistEmpty = waxerr.ErrPlaylistEmpty

	// Processing / local files.
	ErrIncompatibleSpec = waxerr.ErrIncompatibleSpec
	ErrUnsupportedInput = waxerr.ErrUnsupportedInput
	ErrFFmpegNotFound   = waxerr.ErrFFmpegNotFound

	// ErrInvalidConfig indicates invalid or conflicting library configuration.
	ErrInvalidConfig = waxerr.ErrInvalidConfig

	// ErrDeliveryUnsupported indicates that a selected client can extract metadata
	// and formats but cannot deliver media bytes. It is retained for compatibility;
	// built-in clients attempt delivery and report transfer errors instead.
	ErrDeliveryUnsupported = waxerr.ErrDeliveryUnsupported
)

// Re-exported structured error types. Use errors.AsType to inspect them.
type (
	RateLimitError   = waxerr.RateLimitError
	ExtractionError  = waxerr.ExtractionError
	PlayabilityError = waxerr.PlayabilityError
	HTTPStatusError  = waxerr.HTTPStatusError
	// ProviderError reports a failed player-context or session provider call.
	ProviderError = waxerr.ProviderError
	// RequestedFormatError reports that an explicit itag/codec selector matched no
	// available audio format and lists the available alternatives.
	RequestedFormatError = waxerr.RequestedFormatError
	// PlaylistUnavailableError reports why YouTube considers a playlist
	// inaccessible.
	PlaylistUnavailableError = waxerr.PlaylistUnavailableError
)
