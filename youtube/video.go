package youtube

import (
	"time"

	"github.com/colespringer/waxtap/format"
)

// Video is the extracted metadata model. YouTube exposes channel metadata rather
// than authoritative artist tags, so WaxTap returns the raw fields and leaves
// tagging policy to the caller.
type Video struct {
	ID          string        // YouTube video ID
	Title       string        // video title
	Author      string        // channel / uploader name
	ChannelID   string        // YouTube channel ID
	Duration    time.Duration // video duration, or 0 when unknown
	PublishDate time.Time     // publication date, or zero when unknown
	Description string        // video description

	Thumbnails []Thumbnail // candidates for cover art
	Chapters   []Chapter   // chapter markers in playback order

	Formats []format.Format // candidate audio (and incidental video) formats

	IsLive     bool // currently live
	IsUpcoming bool // scheduled premiere / upcoming
}

// Thumbnail is one cover-art candidate.
type Thumbnail struct {
	URL    string // image URL
	Width  int    // pixel width, or 0 when unknown
	Height int    // pixel height, or 0 when unknown
}

// Chapter marks a titled section start within the video.
type Chapter struct {
	Title string        // chapter title
	Start time.Duration // chapter start offset
}
