package youtube

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// idLen is the fixed length of a YouTube video ID.
const idLen = 11

var (
	// idExact matches a bare, well-formed video ID.
	idExact = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	// playlistID matches the known playlist-ID prefixes (PL, OL albums, RD
	// radios/mixes, UU/UL/PU uploads, LL likes, FL favorites, TL, WL
	// watch-later, EC courses). The {10,} tail keeps every accepted id
	// structurally longer than an 11-character video ID, so a video ID that
	// happens to start with a prefix pair is rejected here instead of turning
	// into a network round trip that ends in "playlist unavailable". More
	// exotic ids still work via a list= URL, which bypasses this check.
	playlistID = regexp.MustCompile(`^(?:PL|OL|RD|UU|UL|LL|FL|TL|WL|EC|PU)[A-Za-z0-9_-]{10,}$`)
)

// shortsPlaylistPrefix is the prefix for a channel's Shorts shelf playlist. The
// remaining 22 characters are the channel ID without its UC prefix.
const shortsPlaylistPrefix = "UUSH"

// shortsPlaylistIDLen distinguishes a Shorts shelf playlist from the uploads
// playlist for a UCSH... channel. Both IDs begin with UUSH, but the uploads
// playlist ID is two characters shorter.
const shortsPlaylistIDLen = len(shortsPlaylistPrefix) + 22

// isShortsPlaylistID reports whether id has the prefix and length of a channel's
// Shorts shelf playlist.
func isShortsPlaylistID(id string) bool {
	return len(id) == shortsPlaylistIDLen && strings.HasPrefix(id, shortsPlaylistPrefix)
}

// ExtractVideoID extracts an 11-character video ID from a bare ID or any common
// YouTube URL form (watch?v=, youtu.be/, /embed/, /shorts/, /v/, /live/),
// including scheme-less inputs. It also extracts a bounded 11-character ID token
// from loose text, such as an ID trailed by "&feature=...", which suits pasted
// download/info targets.
//
// An input naming a playlist and no video returns ErrIsPlaylist, and one naming
// a channel and neither returns ErrIsChannel, so the caller can route it to
// Enumerate; a /playlist URL or list= with no usable playlist ID returns
// ErrInvalidPlaylistID. Inputs that carry a candidate of the wrong shape return
// ErrInvalidVideoID; an all-ID-character token of the wrong length returns
// ErrVideoIDTooShort or ErrVideoIDTooLong. It reads the input as Classify does.
func ExtractVideoID(input string) (string, error) {
	return extractVideoID(input, true)
}

// ExtractVideoIDStrict is ExtractVideoID without the loose substring fallback. It
// still accepts a clean ID and every recognized URL form, but rejects strings that
// merely embed an 11-character run, such as "aqz-KE-bpKQ.opus" or
// "/tmp/x/aqz-KE-bpKQ". The process commands use it so a mistyped local path is
// reported as a missing file.
func ExtractVideoIDStrict(input string) (string, error) {
	return extractVideoID(input, false)
}

// extractVideoID is the video view of parseLink.
func extractVideoID(input string, allowLoose bool) (string, error) {
	l := parseLink(input, allowLoose)
	return l.videoID, l.videoErr
}

// classifyMalformedID returns the most specific error for an invalid ID token.
func classifyMalformedID(s string) error {
	switch {
	case len(s) < idLen:
		return waxerr.ErrVideoIDTooShort
	case idCharsOnly(s):
		return waxerr.ErrVideoIDTooLong
	default:
		return waxerr.ErrInvalidVideoID
	}
}

// idFromLooseText returns the first maximal run of ID characters that is exactly
// idLen long. Runs longer than idLen are skipped (not truncated), so an overlong
// bare token is treated as malformed rather than silently shortened.
func idFromLooseText(s string) (string, bool) {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return !isIDChar(r) }) {
		if len(tok) == idLen {
			return tok, true
		}
	}
	return "", false
}

func isIDChar(r rune) bool {
	return r >= 'A' && r <= 'Z' ||
		r >= 'a' && r <= 'z' ||
		r >= '0' && r <= '9' ||
		r == '_' || r == '-'
}

// idCharsOnly reports whether s is non-empty and made up only of video-ID
// characters. With a length check it distinguishes a wrong-length ID from one
// containing invalid characters.
func idCharsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !isIDChar(r) {
			return false
		}
	}
	return true
}

// ExtractPlaylistID extracts a playlist ID from a bare ID, which may carry a
// pasted query such as a share link's "&si=...", or from the list= parameter of
// a YouTube URL, whatever else the URL names. A list= value must be made of ID
// characters. It reads the input as Classify does.
func ExtractPlaylistID(input string) (string, error) {
	if id := parseLink(input, true).listID; id != "" {
		return id, nil
	}
	return "", waxerr.ErrInvalidPlaylistID
}

func validateID(candidate string) (string, error) {
	if idExact.MatchString(candidate) {
		return candidate, nil
	}
	// Match bare-ID error hints for URL path and query forms too.
	return "", classifyMalformedID(candidate)
}

// errNotYouTubeURL reports an unrelated URL while retaining the
// ErrInvalidVideoID classification.
var errNotYouTubeURL error = notYouTubeURLError{}

type notYouTubeURLError struct{}

func (notYouTubeURLError) Error() string { return "waxtap: not a recognized YouTube URL or video ID" }
func (notYouTubeURLError) Unwrap() error { return waxerr.ErrInvalidVideoID }

// errNoVideoInURL reports a recognized YouTube URL without a video or playlist
// reference while retaining the ErrInvalidVideoID classification.
var errNoVideoInURL error = noVideoInURLError{}

type noVideoInURLError struct{}

func (noVideoInURLError) Error() string {
	return "waxtap: not a recognized YouTube video URL or video ID"
}
func (noVideoInURLError) Unwrap() error { return waxerr.ErrInvalidVideoID }

// bareHost lowercases host and drops a leading "www.", in that order, so
// WWW.YOUTUBE-NOCOOKIE.COM compares as youtube-nocookie.com.
func bareHost(host string) string {
	return strings.TrimPrefix(strings.ToLower(host), "www.")
}

// isYouTubeHost reports whether host is a YouTube domain WaxTap recognizes.
func isYouTubeHost(host string) bool {
	h := bareHost(host)
	switch h {
	case "youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be", "youtube-nocookie.com":
		return true
	}
	return strings.HasSuffix(h, ".youtube.com")
}

// parseLoose parses s as a URL, supplying an https:// scheme when one is missing
// but the input still looks like a YouTube URL (has a path or a known host
// prefix). Host-less inputs come back with an empty Host.
func parseLoose(s string) (*url.URL, error) {
	if !hasScheme(s) {
		lower := strings.ToLower(s)
		if strings.Contains(s, "/") ||
			strings.HasPrefix(lower, "youtu.be") ||
			strings.HasPrefix(lower, "youtube.com") ||
			strings.HasPrefix(lower, "m.youtube.com") ||
			strings.HasPrefix(lower, "music.youtube.com") ||
			strings.HasPrefix(lower, "www.") {
			return url.Parse("https://" + s)
		}
	}
	return url.Parse(s)
}

// hasScheme reports whether s starts with a URL scheme and "://". A "://"
// further in, as in a query value carrying a URL, does not count.
func hasScheme(s string) bool {
	i := strings.Index(s, "://")
	if i <= 0 {
		return false
	}
	for j, r := range s[:i] {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (j == 0 || !(r >= '0' && r <= '9' || r == '+' || r == '-' || r == '.')) {
			return false
		}
	}
	return true
}

func videoIDFromPath(u *url.URL) string {
	host := bareHost(u.Hostname())
	segs := pathSegments(u.Path)
	if host == "youtu.be" {
		if len(segs) >= 1 {
			return segs[0]
		}
		return ""
	}
	if len(segs) >= 2 {
		switch segs[0] {
		case "embed":
			// Routes that happen to be 11 characters long: an embedded
			// playlist (list=) and a channel's live stream (channel=).
			if segs[1] == "videoseries" || segs[1] == "live_stream" {
				return ""
			}
			return segs[1]
		case "shorts", "v", "live":
			return segs[1]
		}
	}
	return ""
}

func pathSegments(p string) []string {
	var out []string
	for seg := range strings.SplitSeq(p, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func firstSegment(p string) string {
	if segs := pathSegments(p); len(segs) > 0 {
		return segs[0]
	}
	return ""
}
