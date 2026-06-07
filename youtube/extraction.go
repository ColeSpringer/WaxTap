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

// ResolvedStream contains a playable stream URL and the metadata needed to
// fetch it.
type ResolvedStream struct {
	URL           string
	ExpiresAt     time.Time
	ContentLength int64
	Headers       http.Header
}
