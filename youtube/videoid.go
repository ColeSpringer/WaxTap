package youtube

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/colespringer/waxtap/waxerr"
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

// ExtractVideoID extracts an 11-character video ID from a bare ID or any common
// YouTube URL form (watch?v=, youtu.be/, /embed/, /shorts/, /v/, /live/),
// including scheme-less inputs.
//
// A playlist-only URL (a list= parameter or /playlist path with no video)
// returns ErrIsPlaylist so the caller can route it to Enumerate. Inputs that
// carry a candidate of the wrong shape return ErrInvalidVideoID; inputs too
// short to contain an ID return ErrVideoIDTooShort.
func ExtractVideoID(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", waxerr.ErrVideoIDTooShort
	}
	if idExact.MatchString(s) {
		return s, nil
	}

	if u, err := parseLoose(s); err == nil && u.Host != "" {
		// Distinguish an unrelated URL from a malformed YouTube video ID.
		if !isYouTubeHost(u.Hostname()) {
			return "", errNotYouTubeURL
		}
		q := u.Query()
		if v := q.Get("v"); v != "" {
			return validateID(v)
		}
		if id := videoIDFromPath(u); id != "" {
			return validateID(id)
		}
		// A URL on a known host with no video, but a playlist reference.
		if q.Get("list") != "" || firstSegment(u.Path) == "playlist" {
			return "", waxerr.ErrIsPlaylist
		}
		return "", waxerr.ErrInvalidVideoID
	}

	// No recognizable host: accept a bounded, exactly-11-character ID token from
	// the text (e.g. an ID trailed by "&feature=..."). A longer contiguous token
	// such as a mistyped 12-char ID is rejected rather than silently truncated
	// into a different valid video ID.
	if id, ok := idFromLooseText(s); ok {
		return id, nil
	}
	if countIDChars(s) < idLen {
		return "", waxerr.ErrVideoIDTooShort
	}
	return "", waxerr.ErrInvalidVideoID
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

// ExtractPlaylistID extracts a playlist ID from a bare ID or a URL carrying a
// list= parameter.
func ExtractPlaylistID(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", waxerr.ErrInvalidPlaylistID
	}
	if u, err := parseLoose(s); err == nil && u.Host != "" {
		if list := u.Query().Get("list"); list != "" {
			return list, nil
		}
	}
	if playlistID.MatchString(s) {
		return s, nil
	}
	return "", waxerr.ErrInvalidPlaylistID
}

func validateID(candidate string) (string, error) {
	if idExact.MatchString(candidate) {
		return candidate, nil
	}
	return "", waxerr.ErrInvalidVideoID
}

// errNotYouTubeURL reports an unrelated URL while retaining the
// ErrInvalidVideoID classification.
var errNotYouTubeURL error = notYouTubeURLError{}

type notYouTubeURLError struct{}

func (notYouTubeURLError) Error() string { return "waxtap: not a recognized YouTube URL or video ID" }
func (notYouTubeURLError) Unwrap() error { return waxerr.ErrInvalidVideoID }

// isYouTubeHost reports whether host is a YouTube domain WaxTap recognizes.
func isYouTubeHost(host string) bool {
	h := strings.ToLower(strings.TrimPrefix(host, "www."))
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
	if !strings.Contains(s, "://") {
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

func videoIDFromPath(u *url.URL) string {
	host := strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
	segs := pathSegments(u.Path)
	if host == "youtu.be" {
		if len(segs) >= 1 {
			return segs[0]
		}
		return ""
	}
	if len(segs) >= 2 {
		switch segs[0] {
		case "embed", "shorts", "v", "live":
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

func countIDChars(s string) int {
	n := 0
	for _, r := range s {
		if isIDChar(r) {
			n++
		}
	}
	return n
}
