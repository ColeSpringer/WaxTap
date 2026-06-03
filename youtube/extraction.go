package youtube

import (
	"net/http"
	"time"
)

// Extraction is the result of a successful extract: the video plus the opaque
// context (which client profile and session won) needed to resolve and download
// in the same identity. Its internals are unexported, so the facade holds it as
// an opaque handle and passes it back into resolve/download without reaching in.
type Extraction struct {
	video   *Video
	profile ClientProfile
	session *session
	// rawAudio stores the resolver input for each public Format. It is kept in
	// the same order as Video.Formats because itag is not unique on videos with
	// multiple languages or DRC variants.
	rawAudio []rawFormat
	// expiresAt is when the player response's stream URLs are expected to expire
	// (now + streamingData.expiresInSeconds at extraction time). It is a fallback
	// for resolution when the signed URL itself carries no expire parameter.
	expiresAt time.Time
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

// ResolvedStream is a resolved, playable stream URL with the metadata needed to
// fetch it. youtube owns this type: resolver.Stream is mapped into it by the
// youtube package, and download.Source is mapped from it by the facade.
type ResolvedStream struct {
	URL           string
	ExpiresAt     time.Time
	ContentLength int64
	Headers       http.Header
}
