package youtube

import "time"

// PlaylistEntry is a lightweight playlist item. Enumeration does not download
// media, and full per-video metadata enrichment is opt-in.
type PlaylistEntry struct {
	VideoID   string        // YouTube video ID
	Title     string        // entry title
	Author    string        // channel or uploader name
	ChannelID string        // uploader's channel ID (UC), or "" when neither enumeration nor Enrich resolved it
	Duration  time.Duration // video duration, or 0 when unknown
	Index     int           // 0-based position within the playlist

	// LiveStatus is what the listing said about the entry's broadcast state:
	// LiveNow or LiveUpcoming from the thumbnail badge or overlay, else
	// LiveNone. A finished stream lists as an ordinary video, so LiveWasLive
	// appears here only after Enrich overlays it from the fetched Video. It is
	// the one signal that tells a live item from a video of unknown length
	// without a fetch, and Enrich does not fetch live or upcoming entries,
	// which Info would refuse. A download still attempts them: the listing
	// badge can lag a stream that just ended, and /player stays the authority
	// for a delivery.
	LiveStatus LiveStatus

	// Video is the full metadata a per-entry Info call fetched for this entry:
	// description, thumbnails, formats, and with a watch-page pass the publish
	// date and chapters. Enumeration never sets it; the waxtap facade's Enrich
	// fills it for each entry it refreshes and overlays the entry from it by one
	// rule: a fetched Title, Author, or Duration replaces the listing's, one the
	// fetch left empty keeps the listing's, and ChannelID is filled only when
	// the listing had none, so a channel-feed stamp is never replaced. Video
	// itself is what came back, unchanged, so where the two differ the entry
	// holds the listing's value and Video the fetch's. Nil means the entry was
	// not enriched, whether because enrichment was off, capped short of it,
	// failed for it (see Playlist.Errors), or was canceled before reaching it,
	// in which case the cancellation is the call's own error.
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
