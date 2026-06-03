package youtube

import "time"

// PlaylistEntry is a lightweight playlist item. Enumeration does not download
// media, and full per-video metadata enrichment is opt-in.
type PlaylistEntry struct {
	VideoID  string
	Title    string
	Author   string
	Duration time.Duration
	Index    int // 0-based position within the playlist
}

// Playlist is the result of enumerating a playlist URL. Enumeration is
// tolerant: one bad entry is collected in Errors rather than failing the list.
type Playlist struct {
	ID           string
	Title        string
	Author       string
	Entries      []PlaylistEntry
	Errors       []error // per-entry failures (partial enumeration is not fatal)
	Continuation string  // opaque token for the next page; "" when exhausted
}
