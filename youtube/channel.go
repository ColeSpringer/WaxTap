package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/colespringer/waxtap/waxerr"
)

// channelIDRe matches a canonical UC channel ID (UC + 22 ID characters).
var channelIDRe = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)

// isChannelID reports whether s is a canonical UC channel ID.
func isChannelID(s string) bool { return channelIDRe.MatchString(s) }

// errNotChannelRef marks input that is not a channel reference, so Enumerate can
// fall through to playlist handling.
var errNotChannelRef = errors.New("waxtap: not a channel reference")

// ChannelRef is a classified channel reference produced by ExtractChannelRef and
// resolved to an uploads playlist by Client.ResolveUploadsPlaylist. Exactly one of
// ID or URL is set.
type ChannelRef struct {
	// ID is the UC channel ID when directly available (a bare ID or a /channel/
	// URL); a pure UC-to-UU transform yields the uploads playlist.
	ID string
	// URL is the canonical channel URL to resolve when ID is empty (a handle or a
	// /c/ or /user/ vanity name).
	URL string
}

// ExtractChannelRef classifies a channel reference: a bare UC channel ID, or a
// URL of the form /channel/<id>, /@handle, /c/name, or /user/name, each with an
// optional trailing tab segment such as /videos, /streams, or /shorts, which is
// stripped. A bare @handle (no host) is also accepted. Anything else returns an
// error, so a caller can fall through to playlist handling.
func ExtractChannelRef(input string) (ChannelRef, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return ChannelRef{}, errNotChannelRef
	}
	// Bare channel ID.
	if isChannelID(s) {
		return ChannelRef{ID: s}, nil
	}
	// Bare @handle with no host.
	if isHandle(s) {
		return ChannelRef{URL: "https://www.youtube.com/" + s}, nil
	}

	u, err := parseLoose(s)
	if err != nil || u.Host == "" || !isYouTubeHost(u.Hostname()) {
		return ChannelRef{}, errNotChannelRef
	}
	segs := pathSegments(u.Path)
	if len(segs) == 0 {
		return ChannelRef{}, errNotChannelRef
	}
	switch {
	case isHandle(segs[0]):
		return ChannelRef{URL: "https://www.youtube.com/" + segs[0]}, nil
	case segs[0] == "channel" && len(segs) >= 2:
		if isChannelID(segs[1]) {
			return ChannelRef{ID: segs[1]}, nil
		}
		return ChannelRef{}, errNotChannelRef
	case (segs[0] == "c" || segs[0] == "user") && len(segs) >= 2:
		return ChannelRef{URL: "https://www.youtube.com/" + segs[0] + "/" + segs[1]}, nil
	}
	return ChannelRef{}, errNotChannelRef
}

// isHandle reports whether s is a bare @handle: @ followed by 3-30 handle
// characters (letters, digits, _, -, .), matching YouTube's handle rules so a
// stray token like "@notes.txt" is not treated as a channel.
func isHandle(s string) bool {
	rest, ok := strings.CutPrefix(s, "@")
	if !ok || len(rest) < 3 || len(rest) > 30 {
		return false
	}
	for _, r := range rest {
		if !isIDChar(r) && r != '.' {
			return false
		}
	}
	return true
}

// ResolveUploadsPlaylist resolves a channel reference to its uploads playlist ID
// (UU) and the channel's canonical UC ID. A direct channel ID is a pure UC-to-UU
// transform; a handle or vanity name is resolved to the channel ID first, via the
// InnerTube navigation/resolve_url endpoint with a watch/channel-page scrape
// fallback.
func (c *Client) ResolveUploadsPlaylist(ctx context.Context, ref ChannelRef) (uploadsID, channelID string, err error) {
	channelID = ref.ID
	if channelID == "" {
		channelID, err = c.resolveChannelID(ctx, ref.URL)
		if err != nil {
			return "", "", err
		}
	}
	if !isChannelID(channelID) {
		return "", "", fmt.Errorf("%w: resolved channel ID %q is malformed", waxerr.ErrExtractionFailed, channelID)
	}
	// The uploads playlist shares the channel ID with a UU prefix.
	return "UU" + channelID[2:], channelID, nil
}

// resolveChannelID resolves a channel URL (handle or vanity name) to its UC ID,
// preferring the structured InnerTube resolve endpoint and falling back to a
// channel-page scrape.
func (c *Client) resolveChannelID(ctx context.Context, channelURL string) (string, error) {
	id, err := c.resolveChannelIDViaInnerTube(ctx, channelURL)
	if err == nil && isChannelID(id) {
		return id, nil
	}
	// Do not fire the fallback scrape after cancellation or a rate limit: the
	// second request would hit the same limiter, and the throttle should surface
	// as-is rather than be masked by an ErrExtractionFailed (mirrors the extraction
	// chain in ExtractExcluding).
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if errors.Is(err, waxerr.ErrRateLimited) {
		return "", err
	}
	c.log.DebugContext(ctx, "resolve_url channel resolution failed; scraping the channel page", "url", channelURL, "err", err)
	return c.resolveChannelIDViaScrape(ctx, channelURL)
}

// resolveChannelIDViaInnerTube resolves a channel URL through the locale-
// independent navigation/resolve_url endpoint, reading the channel ID from the
// returned browse endpoint.
func (c *Client) resolveChannelIDViaInnerTube(ctx context.Context, channelURL string) (string, error) {
	profile := c.playlistProfile()
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return "", err
	}
	body, err := c.innertubePost(ctx, profile, sess, resolveEndpoint, c.newResolveRequest(profile, sess, channelURL))
	if err != nil {
		return "", err
	}
	var r struct {
		Endpoint struct {
			BrowseEndpoint struct {
				BrowseID string `json:"browseId"`
			} `json:"browseEndpoint"`
		} `json:"endpoint"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", err
	}
	id := r.Endpoint.BrowseEndpoint.BrowseID
	if !isChannelID(id) {
		return "", fmt.Errorf("resolve_url returned browseId %q, not a channel", id)
	}
	return id, nil
}

// resolveChannelIDViaScrape fetches the channel page and reads the channel ID from
// its canonical link, channel-ID meta tag, or embedded externalId. The
// consent-bypass query keeps an EU interstitial from hiding the markup.
func (c *Client) resolveChannelIDViaScrape(ctx context.Context, channelURL string) (string, error) {
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return "", err
	}
	body, err := c.httpGet(ctx, c.webFallback, sess, consentBypassURL(channelURL))
	if err != nil {
		// A 404 means the handle or vanity name does not resolve to a channel: a
		// user-input problem, classified alongside other availability verdicts,
		// rather than an extractor-maintenance signal.
		if hse, ok := errors.AsType[*waxerr.HTTPStatusError](err); ok && hse.StatusCode == http.StatusNotFound {
			return "", channelNotFoundError(channelURL)
		}
		return "", err
	}
	if id := channelIDFromHTML(body); id != "" {
		return id, nil
	}
	return "", channelNotFoundError(channelURL)
}

// channelNotFoundError reports that a handle or vanity name does not resolve to a
// channel. It wraps ErrVideoUnavailable so the CLI reports it as an unavailable
// target (exit 3), not an extractor defect.
func channelNotFoundError(channelURL string) error {
	return fmt.Errorf("%w: channel %s not found", waxerr.ErrVideoUnavailable, redactChannelURL(channelURL))
}

// consentBypassURL appends YouTube's consent-bypass query so a channel-page scrape
// is not defeated by an EU consent interstitial.
func consentBypassURL(rawURL string) string {
	sep := "?"
	if strings.Contains(rawURL, "?") {
		sep = "&"
	}
	return rawURL + sep + "bpctr=9999999999&has_verified=1"
}

var (
	// canonicalChannelRe reads the channel ID from a <link rel="canonical"> href.
	canonicalChannelRe = regexp.MustCompile(`href="[^"]*/channel/(UC[A-Za-z0-9_-]{22})"`)
	// metaChannelRe reads the channel ID from a <meta itemprop="channelId|identifier"> tag.
	metaChannelRe = regexp.MustCompile(`itemprop="(?:channelId|identifier)"[^>]*content="(UC[A-Za-z0-9_-]{22})"`)
	// externalChannelRe reads the channel ID from the page's embedded config.
	externalChannelRe = regexp.MustCompile(`"externalId":"(UC[A-Za-z0-9_-]{22})"`)
)

// channelIDFromHTML extracts a channel ID from channel-page HTML, trying the
// canonical link, the channel-ID meta tag, then the embedded externalId.
func channelIDFromHTML(body []byte) string {
	for _, re := range []*regexp.Regexp{canonicalChannelRe, metaChannelRe, externalChannelRe} {
		if m := re.FindSubmatch(body); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// newResolveRequest builds a navigation/resolve_url request body.
func (c *Client) newResolveRequest(p ClientProfile, s *session, channelURL string) innertubeRequest {
	return innertubeRequest{
		Context: c.newInnertubeContext(p, s),
		URL:     channelURL,
	}
}

// redactChannelURL trims a channel URL to its path for error messages.
func redactChannelURL(raw string) string {
	if i := strings.Index(raw, "youtube.com"); i >= 0 {
		return raw[i:]
	}
	return raw
}
