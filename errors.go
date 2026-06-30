package waxtap

import "github.com/colespringer/waxtap/waxerr"

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
	ErrIsPlaylist = waxerr.ErrIsPlaylist
	// ErrIsChannel indicates a channel URL was passed where a single video is
	// required.
	ErrIsChannel      = waxerr.ErrIsChannel
	ErrInvalidVideoID = waxerr.ErrInvalidVideoID
	// ErrVideoIDTooShort and ErrVideoIDTooLong indicate an all-ID-character token
	// of the wrong length (a video ID is exactly 11 characters).
	ErrVideoIDTooShort   = waxerr.ErrVideoIDTooShort
	ErrVideoIDTooLong    = waxerr.ErrVideoIDTooLong
	ErrInvalidPlaylistID = waxerr.ErrInvalidPlaylistID
	// ErrPlaylistParse is a maintenance signal, not a bad input: the playlist
	// response parsed but matched no known shape.
	ErrPlaylistParse = waxerr.ErrPlaylistParse
	// ErrPlaylistUnavailable indicates that a playlist is private, deleted, or
	// otherwise inaccessible. See [PlaylistUnavailableError].
	ErrPlaylistUnavailable = waxerr.ErrPlaylistUnavailable
	// ErrPlaylistEmpty indicates that a valid playlist contains no videos.
	ErrPlaylistEmpty = waxerr.ErrPlaylistEmpty
	// ErrShortsPlaylist indicates that WaxTap cannot enumerate a channel's Shorts
	// shelf playlist. It wraps [ErrUnsupportedInput].
	ErrShortsPlaylist = waxerr.ErrShortsPlaylist

	// Processing / local files.
	ErrIncompatibleSpec = waxerr.ErrIncompatibleSpec
	ErrUnsupportedInput = waxerr.ErrUnsupportedInput
	ErrFFmpegNotFound   = waxerr.ErrFFmpegNotFound

	// ErrInvalidConfig indicates invalid or conflicting library configuration.
	ErrInvalidConfig = waxerr.ErrInvalidConfig
)

// Re-exported structured error types. Inspect them with errors.AsType, or with
// errors.As using a double-pointer target: each satisfies error on its pointer
// type (like *os.PathError), so the value form errors.As(err, &PlayabilityError{})
// panics. Use:
//
//	var pe *PlayabilityError
//	if errors.As(err, &pe) { /* pe.Status, pe.Reason */ }
//
// equivalently errors.AsType[*PlayabilityError](err).
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
