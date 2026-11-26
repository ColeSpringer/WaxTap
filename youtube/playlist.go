package youtube

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/colespringer/waxtap/v3/waxerr"
)

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
	ID     string // YouTube playlist ID
	Title  string // playlist title
	Author string // playlist owner name
	// VideoCount is the total YouTube's header states, 0 when it states none.
	// It is neither a floor nor a ceiling on len(Entries): YouTube counts
	// entries the listing hides, and an uploads feed states at most 20,000
	// however many it holds. It comes with the first page a call fetches, so
	// MaxItems: 1 carries it at no extra request.
	VideoCount int
	Entries    []PlaylistEntry // entries in playlist order
	Errors     []error         // per-entry failures, enrichment's included (partial enumeration is not fatal)
	// Continuation is where a later call picks this listing up, given back as
	// the Continuation option with the same playlist. It is set when MaxItems
	// capped the listing with entries left, or a page failed (its error is in
	// Errors), and empty when the listing reached the end or Stop halted it.
	// It is opaque and holds no session state, so it outlives the Client.
	//
	// It names a position, so a playlist edited in between can repeat or miss
	// an entry around it.
	Continuation string
}

// resumePoint is the position behind Playlist.Continuation.
type resumePoint struct {
	Playlist string `json:"p"`           // the playlist it belongs to
	Token    string `json:"t,omitempty"` // YouTube's token for the page to resume in; "" is the first page
	Offset   int    `json:"o,omitempty"` // items of that page already listed
	After    string `json:"a,omitempty"` // the last entry listed, when the point is inside the page
	Index    int    `json:"i,omitempty"` // playlist position of the next entry
}

func (r resumePoint) String() string {
	b, _ := json.Marshal(r) // strings and ints cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// cut returns how many leading items of the re-read page the earlier call got
// through. It finds the last entry that call listed, so that what the page
// gained or lost above it neither repeats nor skips an entry, and takes the
// match nearest the stored offset because a playlist can list a video twice.
// With no match the offset stands.
func (r resumePoint) cut(items []playlistItem) int {
	at, gap := min(r.Offset, len(items)), -1
	for i, it := range items {
		if r.After == "" || it.itemVideoID() != r.After {
			continue
		}
		if d := max(i+1-r.Offset, r.Offset-i-1); gap < 0 || d < gap {
			at, gap = i+1, d
		}
	}
	return at
}

// CheckContinuation reports whether s has the form of a Playlist.Continuation,
// as [waxerr.ErrInvalidContinuation] when it does not. Enumerate goes on to
// check it against the playlist.
func CheckContinuation(s string) error {
	_, err := decodeResumePoint(s)
	return err
}

func decodeResumePoint(s string) (resumePoint, error) {
	var r resumePoint
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	if err != nil || r.Playlist == "" || r.Offset < 0 || r.Index < 0 {
		return resumePoint{}, fmt.Errorf("%w: not one Enumerate returned", waxerr.ErrInvalidContinuation)
	}
	return r, nil
}

// parseResumePoint decodes a Continuation and checks it against the playlist
// being listed: a token of another playlist would list that one under this ID.
func parseResumePoint(s, playlistID string) (resumePoint, error) {
	r, err := decodeResumePoint(s)
	if err != nil {
		return resumePoint{}, err
	}
	if r.Playlist != playlistID {
		return resumePoint{}, fmt.Errorf("%w: it belongs to playlist %s, not %s", waxerr.ErrInvalidContinuation, r.Playlist, playlistID)
	}
	return r, nil
}
