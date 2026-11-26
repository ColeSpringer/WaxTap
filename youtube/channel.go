package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/waxerr"
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

// ExtractChannelRef classifies a channel reference: a bare UC channel ID or
// @handle, or a YouTube URL of the form /channel/<id>, /@handle, /c/name,
// or /user/name. Each may end in a tab segment such as /videos, /streams, or
// /shorts, which is stripped. youtu.be names videos only. Anything else
// returns an error, so a caller can fall through to playlist handling. It
// reads the input as Classify does.
func ExtractChannelRef(input string) (ChannelRef, error) {
	if ref := parseLink(input, true).channel; ref != (ChannelRef{}) {
		return ref, nil
	}
	return ChannelRef{}, errNotChannelRef
}

// channelFromPath reads a channel from a YouTube URL's decoded path segments,
// any trailing tab segment ignored. The name goes back into the URL escaped.
func channelFromPath(segs []string) ChannelRef {
	switch {
	case len(segs) == 0:
		return ChannelRef{}
	case isURLHandle(segs[0]):
		return ChannelRef{URL: "https://www.youtube.com/" + url.PathEscape(segs[0])}
	case segs[0] == "channel" && len(segs) >= 2 && isChannelID(segs[1]):
		return ChannelRef{ID: segs[1]}
	case (segs[0] == "c" || segs[0] == "user") && len(segs) >= 2:
		return ChannelRef{URL: "https://www.youtube.com/" + segs[0] + "/" + url.PathEscape(segs[1])}
	}
	return ChannelRef{}
}

// isURLHandle reports whether a URL path segment is an @handle. A YouTube URL's
// handle is YouTube's own, so unlike a bare one it may be short or in any
// script: letters, marks, and digits of any script, and "_", "-", ".", "·".
func isURLHandle(seg string) bool {
	rest, ok := strings.CutPrefix(seg, "@")
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if !unicode.In(r, unicode.L, unicode.M, unicode.N) && !strings.ContainsRune("_-.·", r) {
			return false
		}
	}
	return true
}

// isHandle reports whether s is a bare @handle: @ followed by 3-30 ASCII
// letters, digits, "_", "-", or ".". Only the shape is checked, so a token
// like "@notes.txt" passes too; only loose callers read a bare handle.
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
	if httpx.IsProxyConnect(err) {
		// The scrape would bootstrap again and dial the same dead proxy; the
		// chain stops on this failure (ExtractExcluding) and so does this.
		return "", err
	}
	c.log.DebugContext(ctx, "resolve_url channel resolution failed; scraping the channel page", "url", channelURL, "err", err)
	return c.resolveChannelIDViaScrape(ctx, channelURL)
}

// resolveChannelIDViaInnerTube resolves a channel URL through the locale-
// independent navigation/resolve_url endpoint, reading the channel ID from the
// returned browse endpoint.
func (c *Client) resolveChannelIDViaInnerTube(ctx context.Context, channelURL string) (string, error) {
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return "", err
	}
	profile := adoptedProfile(c.playlistProfile(), sess)
	body, err := c.innertubePost(ctx, profile, sess, resolveEndpoint, c.newResolveRequest(profile, sess, channelURL))
	if err != nil {
		return "", explainBareDeadline(err, sess)
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
	body, err := c.httpGet(ctx, adoptedProfile(c.webFallback, sess), sess, consentBypassURL(channelURL))
	if err != nil {
		// A 404 means the handle or vanity name does not resolve to a channel: a
		// user-input problem, classified alongside other availability verdicts,
		// rather than an extractor-maintenance signal.
		if hse, ok := errors.AsType[*waxerr.HTTPStatusError](err); ok && hse.StatusCode == http.StatusNotFound {
			return "", channelNotFoundError(channelURL)
		}
		return "", explainBareDeadline(err, sess)
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

// redactChannelURL trims a channel URL to its host and path for error
// messages, unescaped so a non-ASCII handle reads as written.
func redactChannelURL(raw string) string {
	if s, err := url.PathUnescape(raw); err == nil {
		raw = s
	}
	if i := strings.Index(raw, "youtube.com"); i >= 0 {
		return raw[i:]
	}
	return raw
}
