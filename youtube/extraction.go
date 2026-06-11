package youtube

import (
	"net/http"
	"time"
)

// Extraction contains video metadata plus the client profile and session needed
// to resolve and download its formats with the same identity.
type Extraction struct {
	video   *Video
	profile ClientProfile
	session *session
	// rawAudio stores the resolver input for each public Format. It is kept in
	// the same order as Video.Formats because itag is not unique on videos with
	// multiple languages or DRC variants.
	rawAudio []rawFormat
	// expiresAt is the fallback expiry derived from
	// streamingData.expiresInSeconds. A signed URL's expire parameter takes
	// precedence during resolution.
	expiresAt time.Time
	// serverAbrURL and ustreamerConfig are retained for SABR-backed formats. They
	// are empty for direct streams and are not used until the SABR download path
	// is connected.
	serverAbrURL    string
	ustreamerConfig string
	// playerURL pins the base.js the SABR n-descramble must use. It is set on the
	// WEB-context path (YouTube A/Bs base.js per visitor, so the context's n is
	// only coherent with the player its /player referenced). Empty means discover
	// the player independently, as the normal extraction chain does.
	playerURL string
	// webContext marks an Extraction built from an attested WEB /player context
	// (see Client.ExtractWebContext) rather than the normal InnerTube chain. A
	// mid-stream SABR reload must re-fetch the same kind of context to keep the
	// URL, session, and GVS-token binding coherent.
	webContext bool
}

// buildExtraction keeps the InnerTube and watch-page extraction paths in sync.
func buildExtraction(video *Video, profile ClientProfile, sess *session, raw []rawFormat, pr *playerResponse) *Extraction {
	return &Extraction{
		video:           video,
		profile:         profile,
		session:         sess,
		rawAudio:        raw,
		expiresAt:       pr.expiresAt(time.Now()),
		serverAbrURL:    pr.serverAbrURL(),
		ustreamerConfig: pr.ustreamerConfig(),
	}
}

// Video returns the extracted metadata and candidate formats.
func (e *Extraction) Video() *Video {
	if e == nil {
		return nil
	}
	return e.video
}

// rawFormatByIndex returns the raw resolver input for Video.Formats[i].
func (e *Extraction) rawFormatByIndex(i int) (rawFormat, bool) {
	if i < 0 || i >= len(e.rawAudio) {
		return rawFormat{}, false
	}
	return e.rawAudio[i], true
}

// ResolvedStream contains the metadata available after resolution. Direct
// streams include a URL and request headers; SABR streams set IsSABR and leave
// URL empty.
type ResolvedStream struct {
	URL           string
	ExpiresAt     time.Time
	ContentLength int64
	Headers       http.Header
	// IsSABR reports whether the stream must be fetched through SABR.
	IsSABR bool
}

// Probeable reports whether the stream has a direct URL suitable for ffprobe.
func (rs ResolvedStream) Probeable() bool {
	return rs.URL != "" && !rs.IsSABR
}

// MediaPlan describes how to fetch a selected format. Exactly one of Direct or
// SABR is non-nil.
type MediaPlan struct {
	Direct *ResolvedStream
	SABR   *SABRStream
}

// Diagnostic returns the metadata available without opening the stream.
func (m MediaPlan) Diagnostic() ResolvedStream {
	switch {
	case m.SABR != nil:
		return ResolvedStream{IsSABR: true, ContentLength: m.SABR.contentLength, ExpiresAt: m.SABR.expiresAt}
	case m.Direct != nil:
		return *m.Direct
	default:
		return ResolvedStream{}
	}
}
