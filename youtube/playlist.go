package youtube

import "time"

// PlaylistEntry is a lightweight playlist item. Enumeration does not download
// media, and full per-video metadata enrichment is opt-in.
type PlaylistEntry struct {
	VideoID   string        // YouTube video ID
	Title     string        // entry title
	Author    string        // channel or uploader name
	ChannelID string        // uploader's channel ID (UC), or "" when not resolved at enumerate time
	Duration  time.Duration // video duration, or 0 when unknown
	Index     int           // 0-based position within the playlist

	// Video is the full metadata a per-entry Info call fetched for this entry:
	// description, thumbnails, formats, and with a watch-page pass the publish
	// date and chapters. Enumeration never sets it; the waxtap facade's Enrich
	// fills it for each entry it refreshes and refreshes Title, Author, and
	// Duration from it, so those agree with it, while ChannelID keeps its
	// enumerate-time value when there was one. Nil means the entry was not
	// enriched, whether because enrichment was off, capped short of it, failed
	// for it (see Playlist.Errors), or was canceled before reaching it, in
	// which case the cancellation is the call's own error.
	Video *Video
}

// Playlist is the result of enumerating a playlist URL. Enumeration is
// tolerant: one bad entry is collected in Errors rather than failing the list.
type Playlist struct {
	ID           string          // YouTube playlist ID
	Title        string          // playlist title
	Author       string          // playlist owner name
	Entries      []PlaylistEntry // entries in playlist order
	Errors       []error         // per-entry failures, enrichment's included (partial enumeration is not fatal)
	Continuation string          // opaque token for the next page; "" when exhausted
}
