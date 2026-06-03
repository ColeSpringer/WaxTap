package youtube

import (
	"time"

	"github.com/colespringer/waxtap/format"
)

// Video is the extracted metadata model. YouTube exposes channel metadata rather
// than authoritative artist tags, so WaxTap returns the raw fields and leaves
// tagging policy to the caller.
type Video struct {
	ID          string
	Title       string
	Author      string // channel / uploader name
	ChannelID   string
	Duration    time.Duration
	PublishDate time.Time
	Description string

	Thumbnails []Thumbnail // candidates for cover art
	Chapters   []Chapter

	Formats []format.Format // candidate audio (and incidental video) formats

	IsLive     bool // currently live
	IsUpcoming bool // scheduled premiere / upcoming
}

// Thumbnail is one cover-art candidate.
type Thumbnail struct {
	URL    string
	Width  int
	Height int
}

// Chapter marks a titled section start within the video.
type Chapter struct {
	Title string
	Start time.Duration
}
