package youtube

import (
	"time"

	"github.com/colespringer/waxtap/format"
)

// Video is the extracted metadata model. YouTube exposes channel metadata rather
// than authoritative artist tags, so WaxTap returns the raw fields and leaves
// tagging policy to the caller.
type Video struct {
	ID          string        // YouTube video ID (canonical, stable identity anchor)
	URL         string        // canonical watch URL (https://www.youtube.com/watch?v=<ID>)
	Title       string        // video title
	Author      string        // channel / uploader name
	ChannelID   string        // YouTube channel ID (canonical UC identity anchor)
	Duration    time.Duration // video duration, or 0 when unknown
	PublishDate time.Time     // publication date, or zero when unknown
	Description string        // video description

	Thumbnails []Thumbnail // cover-art candidates, largest first
	Chapters   []Chapter   // chapter markers in playback order

	Formats []format.Format // candidate audio (and incidental video) formats

	// LiveStatus reports the broadcast state. On a Video returned by extraction it
	// is LiveNone or LiveWasLive (a completed VOD): currently-live and upcoming
	// videos are rejected earlier and surface as ErrLiveContent / ErrLiveNotStarted
	// sentinels instead of a Video.
	LiveStatus LiveStatus
	// Availability reports whether the video is publicly listed. It is set to
	// AvailabilityPublic or AvailabilityUnlisted only on a watch-page metadata pass
	// (the microformat that carries it is WEB-only); otherwise it is
	// AvailabilityUnknown. Restricted states (private, members-only, geo-blocked,
	// removed, age-gated) surface as availability sentinels, not a Video.
	Availability Availability
}

// LiveStatus reports a video's live-broadcast state.
type LiveStatus uint8

const (
	// LiveNone marks a normal, non-live video.
	LiveNone LiveStatus = iota
	// LiveUpcoming marks a scheduled premiere or upcoming stream.
	LiveUpcoming
	// LiveNow marks a currently-live stream.
	LiveNow
	// LiveWasLive marks a completed livestream VOD.
	LiveWasLive
)

func (s LiveStatus) String() string {
	switch s {
	case LiveUpcoming:
		return "upcoming"
	case LiveNow:
		return "live"
	case LiveWasLive:
		return "was_live"
	default:
		return "none"
	}
}

// Availability reports whether a video is publicly listed. It is a tri-state so a
// caller can distinguish a public video from one whose listing state was never
// determined (no watch-page pass ran).
type Availability uint8

const (
	// AvailabilityUnknown means the listing state was not determined.
	AvailabilityUnknown Availability = iota
	// AvailabilityPublic marks a public, listed video.
	AvailabilityPublic
	// AvailabilityUnlisted marks an unlisted (link-only) video.
	AvailabilityUnlisted
)

func (a Availability) String() string {
	switch a {
	case AvailabilityPublic:
		return "public"
	case AvailabilityUnlisted:
		return "unlisted"
	default:
		return "unknown"
	}
}

// AvailabilityFromUnlisted maps a watch-page microformat isUnlisted flag to the
// listing state. It is called only after a watch-page pass has run, so the state
// is determined (Public or Unlisted), never Unknown.
func AvailabilityFromUnlisted(unlisted bool) Availability {
	if unlisted {
		return AvailabilityUnlisted
	}
	return AvailabilityPublic
}

// Thumbnail is one cover-art candidate.
type Thumbnail struct {
	URL    string // image URL
	Width  int    // pixel width, or 0 when unknown
	Height int    // pixel height, or 0 when unknown
}

// Chapter marks a titled section within the video.
type Chapter struct {
	Title string        // chapter title
	Start time.Duration // chapter start offset
	// End is the chapter's end offset, derived from the next chapter's start (or
	// the video duration for the last chapter). It is zero when the chapter is
	// open-ended: the last chapter of a video whose duration is unknown or not
	// past the chapter start.
	End time.Duration
}
