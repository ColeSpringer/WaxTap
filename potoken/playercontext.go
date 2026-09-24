package potoken

import "context"

// PlayerContext is an attested /player streaming context handed off from an
// external attesting browser (for example WaxSeal) so WaxTap can stream WEB SABR
// audio Go-side. Its serverAbrStreamingUrl carries an entitled
// (STREAM_PROTECTION_STATUS=1) grade by provenance (the grade is baked into the
// signed URL by the attested /player call), so WaxTap descrambles the URL's n and
// streams the full audio against it rather than the ~first-minute preview an
// unattested client receives. Like [Session], this is a richer browser-attested
// handoff than a bare PO token, which is why it lives beside the token contracts.
type PlayerContext struct {
	// ServerAbrURL is the raw serverAbrStreamingUrl from the /player response,
	// with its n parameter still scrambled. WaxTap descrambles n before streaming.
	ServerAbrURL string
	// PlayerURL is the base.js the attesting browser's /player referenced for this
	// context. YouTube A/B-tests base.js per visitor, so n must be descrambled with
	// THIS player, not one the consumer discovers independently. Empty falls back
	// to independent discovery (older providers).
	PlayerURL string
	// UstreamerConfig is the base64 videoPlaybackUstreamerConfig sent in every
	// SABR request.
	UstreamerConfig string
	// VisitorData is the session identity the URL is bound to. WaxTap streams
	// under it and binds the GVS PO token's content binding to it, so the URL,
	// the visitor-id header, and the token stay coherent to the byte.
	VisitorData string
	// UserAgent and ClientVersion are the browser identity the context was minted
	// under: the exact navigator.userAgent, and the InnerTube client version its
	// player ran, which is echoed in the SABR streamerContext client_info. When
	// UserAgent is set, the WEB requests under this context (the SABR stream and
	// the GVS token request's [Request]) carry it in place of WaxTap's own Chrome
	// identity, so Options.ChromeMajor does not apply and the URL, the visitor id,
	// the token, and the requests present one browser. Empty means WaxTap's own.
	UserAgent     string
	ClientVersion string
	// Title and Author are video metadata for the output. They may be empty; the
	// consumer falls back to the video ID for the filename when Title is empty.
	Title  string
	Author string // channel or uploader name
	// LengthSeconds is the video duration in seconds. Zero means unknown.
	LengthSeconds int
	// ChannelID and Description are videoDetails.channelId and shortDescription.
	ChannelID   string
	Description string
	// Thumbnails is the videoDetails thumbnail ladder in the provider's own order
	// (the player response's, smallest first). WaxTap sorts it largest first.
	Thumbnails []PlayerContextThumbnail
	// IsLiveContent, IsLiveNow, and IsUpcoming are the videoDetails live flags. A
	// context for a broadcast that is live or upcoming is refused the way a
	// /player response for one is (ErrLiveContent, ErrLiveNotStarted); a finished
	// broadcast (IsLiveContent alone) is a VOD and reports LiveWasLive.
	IsLiveContent bool
	IsLiveNow     bool
	IsUpcoming    bool
	// PublishDate is the microformat's publishDate as sent, RFC 3339 or a bare
	// 2006-01-02 date; WaxTap parses it. Empty when the response carried none.
	PublishDate string
	// AudioFormats are the audio renditions available for this context.
	AudioFormats []PlayerContextFormat
	// Generation names the provider session that minted this context, so WaxTap
	// can report it unusable through [SessionInvalidator]. It is opaque to
	// WaxTap and echoed back verbatim. Zero means the provider does not version
	// its sessions, which leaves the session unreportable.
	Generation uint64
}

// PlayerContextThumbnail is one rung of a context's thumbnail ladder. Width and
// Height are zero when the player response omits them.
type PlayerContextThumbnail struct {
	URL    string
	Width  int
	Height int
}

// PlayerContextFormat is one audio rendition in a PlayerContext. Itag, LMT, and
// XTags identify the encoding as a unit: requesting an (itag, lmt, xtags) triple
// that matches no rendition makes the SABR server answer RELOAD_PLAYER_RESPONSE,
// so a consumer must carry all three together. XTags must be the player
// response's value verbatim: WaxTap also reads the audio role (acont) from it to
// rank the original track.
type PlayerContextFormat struct {
	Itag             int    // YouTube format identifier
	LMT              string // lastModified, distinguishes encodings sharing an itag
	XTags            string // the player response's xtags, verbatim
	MimeType         string // raw YouTube MIME type
	Bitrate          int    // bits per second
	AudioQuality     string // YouTube's audioQuality tier, e.g. AUDIO_QUALITY_MEDIUM
	AudioChannels    int    // channel count
	AudioSampleRate  int    // samples per second
	ContentLength    int64  // bytes, or 0 when unknown
	ApproxDurationMs int64  // approximate duration in milliseconds
	// IsDrc marks a DRC (dynamic-range-compressed) rendition. The SABR request
	// declares it in client_abr_state.drc_enabled when streaming one, so a
	// provider omitting it leaves DRC renditions misdescribed on the wire.
	IsDrc bool
	// AudioTrackID identifies the audio track (the audioTrack.id of the
	// /player format, e.g. "en.4"). Empty means the video has a single track:
	// every entry of a multi-track video carries one, the original's included.
	AudioTrackID string
	// AudioIsDefault is the player's audioTrack.audioIsDefault, which marks the
	// default track, a dub included. WaxTap falls back to it to rank the
	// original track when XTags carries no audio role, and reads it only beside
	// an AudioTrackID, where the player marks every track: true on the default,
	// false on the rest. Nil means the provider stated nothing, which leaves the
	// verdict unknown rather than false.
	AudioIsDefault *bool
}

// PlayerContextProvider supplies an attested player context for a video on
// demand. A provider that also implements [SessionInvalidator] can be told when
// its contexts deliver capped streams, so it can retire the session behind them
// and mint later contexts from a fresh one. Implementations must honor ctx
// cancellation and should be safe for concurrent use.
type PlayerContextProvider interface {
	// ProvidePlayerContext returns an attested streaming context for videoID.
	ProvidePlayerContext(ctx context.Context, videoID string) (PlayerContext, error)
}

// PlayerContextProviderFunc adapts an ordinary function to PlayerContextProvider,
// so a caller can supply contexts from a closure without defining a named type.
type PlayerContextProviderFunc func(ctx context.Context, videoID string) (PlayerContext, error)

// ProvidePlayerContext calls f.
func (f PlayerContextProviderFunc) ProvidePlayerContext(ctx context.Context, videoID string) (PlayerContext, error) {
	return f(ctx, videoID)
}
