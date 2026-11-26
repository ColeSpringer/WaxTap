package youtube

import (
	"net/url"
	"strings"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// LinkKind is what an input names, as WaxTap's commands route it.
type LinkKind uint8

const (
	// LinkUnknown is the kind of an input Classify refused.
	LinkUnknown LinkKind = iota
	// LinkVideo is an input carrying a video ID, with or without a list.
	LinkVideo
	// LinkPlaylist is an input naming a playlist and no video.
	LinkPlaylist
	// LinkChannel is an input naming a channel and neither.
	LinkChannel
)

func (k LinkKind) String() string {
	switch k {
	case LinkVideo:
		return "video"
	case LinkPlaylist:
		return "playlist"
	case LinkChannel:
		return "channel"
	default:
		return "unknown"
	}
}

// Link is what an input names, from Classify.
type Link struct {
	Kind       LinkKind
	VideoID    string // set when the input names a video
	PlaylistID string // set when it names a list, a watch URL's list= included
	ChannelID  string // the UC id, when the input states it
	ChannelURL string // canonical handle or vanity URL; its UC id takes a request
}

// Classify reports what input names, the way WaxTap's commands route it,
// without a request. Kind is a video when the input carries a video ID, else a
// playlist, else a channel, and every ID found is returned: watch?v=X&list=Y
// is a video with PlaylistID Y.
//
// An input naming none of them returns the error ExtractVideoID gives (an
// invalid, too short, or too long ID, or not a YouTube URL), or
// ErrInvalidPlaylistID for a playlist URL with no usable list=; never
// ErrIsPlaylist or ErrIsChannel. The Link returned with an error still holds
// any playlist or channel the input names, with Kind LinkUnknown.
//
// It is as lenient as the commands: any 11-character token shaped like an ID
// reads as a video, so "information" and "please download information now"
// both do. And it refuses watch?v=bad&list=Y, whose list Enumerate accepts.
func Classify(input string) (Link, error) {
	l := parseLink(input, true)
	out := Link{VideoID: l.videoID, PlaylistID: l.listID, ChannelID: l.channel.ID, ChannelURL: l.channel.URL}
	switch {
	case l.videoID != "":
		out.Kind = LinkVideo
	case l.videoErr == waxerr.ErrIsPlaylist:
		out.Kind = LinkPlaylist
	case l.videoErr == waxerr.ErrIsChannel:
		out.Kind = LinkChannel
	default:
		return out, l.videoErr
	}
	return out, nil
}

// link is one reading of an input: every ID it names, and the error
// ExtractVideoID reports when it names no video. The extractors and Classify
// are views of it, so they cannot disagree about what an input names.
type link struct {
	videoID  string
	videoErr error // why videoID is ""
	listID   string
	channel  ChannelRef // zero when the input names no channel
}

// parseLink reads input once. loose lets a host-less input name a video by an
// 11-character token inside other text, or a channel by a bare UC ID or
// @handle; strict callers (the process commands) take only a clean ID or URL.
func parseLink(input string, loose bool) link {
	s := strings.TrimSpace(input)
	if s == "" {
		return link{videoErr: waxerr.ErrVideoIDTooShort}
	}
	if idExact.MatchString(s) {
		return link{videoID: s}
	}
	if loose {
		if u, ok := bareChannelPath(s); ok {
			return urlLink(u)
		}
	}
	if u, err := parseLoose(s); err == nil && u.Host != "" {
		return urlLink(u)
	}
	return bareLink(s, loose)
}

// bareChannelPath reads a bare channel ID or @handle with a path after it,
// "@name/videos?list=...", as that path on youtube.com. Parsed as a URL it would
// make the name its host. A handle with a dot reads as the host it looks like,
// "@youtu.be/ID" or "@example.com/x", and parses as that URL with an empty
// userinfo.
func bareChannelPath(s string) (*url.URL, bool) {
	head, _, ok := strings.Cut(s, "/")
	var raw string
	switch {
	case !ok:
		return nil, false
	case isChannelID(head):
		raw = "https://www.youtube.com/channel/" + s
	case isHandle(head) && !strings.Contains(head, "."):
		raw = "https://www.youtube.com/" + s
	default:
		return nil, false
	}
	u, err := url.Parse(raw)
	return u, err == nil
}

// urlLink reads a URL with a host.
func urlLink(u *url.URL) link {
	if !isYouTubeHost(u.Hostname()) {
		return link{videoErr: errNotYouTubeURL}
	}
	var l link
	q := u.Query()
	if list := q.Get("list"); idCharsOnly(list) {
		l.listID = list
	}
	// youtu.be names videos only: YouTube redirects youtu.be/@x to watch?v=@x.
	if bareHost(u.Hostname()) != "youtu.be" {
		l.channel = channelFromPath(pathSegments(u.Path))
	}
	v := q.Get("v")
	if v == "" {
		v = videoIDFromPath(u)
	}
	// A video candidate decides alone, malformed or not; a playlist and then a
	// channel come next.
	switch {
	case v != "":
		l.videoID, l.videoErr = validateID(v)
	case l.listID != "":
		l.videoErr = waxerr.ErrIsPlaylist
	case l.channel != (ChannelRef{}):
		l.videoErr = waxerr.ErrIsChannel
	case q.Get("list") != "" || firstSegment(u.Path) == "playlist":
		// A playlist route that names no playlist ID YouTube could serve.
		l.videoErr = waxerr.ErrInvalidPlaylistID
	default:
		l.videoErr = errNoVideoInURL
	}
	return l
}

// bareLink reads an input with no host: an ID, a playlist ID, a channel ID or
// @handle, or (loose) text with an ID in it.
func bareLink(s string, loose bool) link {
	if loose {
		// A bare channel ID or @handle is a channel, checked ahead of the loose
		// token so an @handle whose body is an 11-character run is not read as a
		// video. Strict callers skip it, so a mistyped @-prefixed or UC-shaped
		// local path reports a missing file rather than a channel.
		switch {
		case isChannelID(s):
			return link{channel: ChannelRef{ID: s}, videoErr: waxerr.ErrIsChannel}
		case isHandle(s):
			return link{channel: ChannelRef{URL: "https://www.youtube.com/" + s}, videoErr: waxerr.ErrIsChannel}
		}
		// A bare playlist ID trailed by a pasted query is that playlist, not
		// an 11-character token the query happens to hold, like a share
		// link's si=.
		if i := strings.IndexAny(s, "&?#"); i > 0 && playlistID.MatchString(s[:i]) {
			return link{listID: s[:i], videoErr: waxerr.ErrIsPlaylist}
		}
		// An exactly-11-character token inside text, such as an ID trailed by
		// "&feature=...". A longer run is skipped rather than truncated into a
		// different valid ID.
		if id, ok := idFromLooseText(s); ok {
			return link{videoID: id}
		}
	}
	if playlistID.MatchString(s) {
		return link{listID: s, videoErr: waxerr.ErrIsPlaylist}
	}
	return link{videoErr: classifyMalformedID(s)}
}
