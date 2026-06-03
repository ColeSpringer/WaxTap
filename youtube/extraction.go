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
}

// Video returns the extracted metadata and candidate formats.
func (e *Extraction) Video() *Video {
	if e == nil {
		return nil
	}
	return e.video
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
