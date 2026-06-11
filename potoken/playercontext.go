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
	// ClientVersion is the InnerTube client version the context was minted under;
	// it is echoed in the SABR streamerContext client_info.
	ClientVersion string
	// Title and Author are video metadata for the output. They may be empty; the
	// consumer falls back to the video ID for the filename when Title is empty.
	Title  string
	Author string
	// LengthSeconds is the video duration in seconds. Zero means unknown.
	LengthSeconds int
	// AudioFormats are the audio renditions available for this context.
	AudioFormats []PlayerContextFormat
}

// PlayerContextFormat is one audio rendition in a PlayerContext. Itag, LMT, and
// XTags identify the encoding as a unit: requesting an (itag, lmt, xtags) triple
// that matches no rendition makes the SABR server answer RELOAD_PLAYER_RESPONSE,
// so a consumer must carry all three together.
type PlayerContextFormat struct {
	Itag             int
	LMT              string // lastModified, distinguishes encodings sharing an itag
	XTags            string
	MimeType         string
	Bitrate          int
	AudioQuality     string // YouTube's audioQuality tier, e.g. AUDIO_QUALITY_MEDIUM
	AudioChannels    int
	AudioSampleRate  int
	ContentLength    int64
	ApproxDurationMs int64
	// IsDrc marks a DRC (dynamic-range-compressed) rendition. The SABR request
	// declares it in client_abr_state.drc_enabled when streaming one, so a
	// provider omitting it leaves DRC renditions misdescribed on the wire.
	IsDrc bool
	// AudioTrackID identifies the audio track on multi-audio videos (the
	// audioTrack.id of the /player format, e.g. "en.4"). Empty means the
	// default or only track.
	AudioTrackID string
}

// PlayerContextProvider supplies an attested player context for a video on
// demand. Implementations must honor ctx cancellation and should be safe for
// concurrent use.
type PlayerContextProvider interface {
	ProvidePlayerContext(ctx context.Context, videoID string) (PlayerContext, error)
}

// PlayerContextProviderFunc adapts an ordinary function to PlayerContextProvider,
// so a caller can supply contexts from a closure without defining a named type.
type PlayerContextProviderFunc func(ctx context.Context, videoID string) (PlayerContext, error)

// ProvidePlayerContext calls f.
func (f PlayerContextProviderFunc) ProvidePlayerContext(ctx context.Context, videoID string) (PlayerContext, error) {
	return f(ctx, videoID)
}
