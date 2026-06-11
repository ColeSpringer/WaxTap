package youtube

import "time"

// PlaylistEntry is a lightweight playlist item. Enumeration does not download
// media, and full per-video metadata enrichment is opt-in.
type PlaylistEntry struct {
	VideoID  string        // YouTube video ID
	Title    string        // entry title
	Author   string        // channel or uploader name
	Duration time.Duration // video duration, or 0 when unknown
	Index    int           // 0-based position within the playlist
}

// Playlist is the result of enumerating a playlist URL. Enumeration is
// tolerant: one bad entry is collected in Errors rather than failing the list.
type Playlist struct {
	ID           string          // YouTube playlist ID
	Title        string          // playlist title
	Author       string          // playlist owner name
	Entries      []PlaylistEntry // entries in playlist order
	Errors       []error         // per-entry failures (partial enumeration is not fatal)
	Continuation string          // opaque token for the next page; "" when exhausted
}
