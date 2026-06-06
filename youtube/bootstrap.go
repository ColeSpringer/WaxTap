package youtube

import (
	"context"
	"encoding/json"
	"errors"
	rand "math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// YouTube's no-PO-token clients, including ANDROID_VR, usually need a coherent
// logged-out identity: visitorData plus the cookies issued with it. A synthetic
// visitorData value alone can still trip the bot check.
//
// When the HTTP client has a cookie jar, WaxTap loads a YouTube page once, caches
// the visitorData it exposes, and lets the jar retain the matching Set-Cookie
// values. The bootstrap is best-effort; extraction falls back to the synthetic
// visitorData if the page fetch fails.

const (
	// visitorTTL bounds how long a bootstrapped visitorData is reused.
	visitorTTL = 6 * time.Hour
	// visitorCacheKey is global because visitorData is not video-specific.
	visitorCacheKey = "visitor"
)

// visitorDataRe extracts the logged-out visitor identity from a YouTube web page.
// ytcfg exposes it as VISITOR_DATA; embedded InnerTube contexts use visitorData.
var visitorDataRe = regexp.MustCompile(`"(?:visitorData|VISITOR_DATA)"\s*:\s*"([^"]+)"`)

// newBootstrappedSession starts a session with bootstrapped visitorData when a
// cookie-backed guest identity is available, otherwise with synthetic visitorData.
//
// Bootstrapping is skipped without a cookie jar because the matching cookies
// cannot be preserved. That also keeps injected, jarless test clients on the
// synthetic path.
func (c *Client) newBootstrappedSession(ctx context.Context) *session {
	sess := newSession(c.gl)
	if c.http.Jar() == nil {
		return sess
	}
	vd, err := c.bootstrapVisitorData(ctx)
	if err != nil {
		c.log.DebugContext(ctx, "visitor bootstrap failed; using synthetic visitorData", "err", err)
		return sess
	}
	if vd != "" {
		sess.visitorData = vd
	}
	return sess
}

// bootstrapVisitorData returns server-issued visitorData, loading and caching it
// once across concurrent callers. The page response also seeds the client's jar.
func (c *Client) bootstrapVisitorData(ctx context.Context) (string, error) {
	return c.visitors.GetOrLoad(ctx, visitorCacheKey, c.fetchVisitorData)
}

// fetchVisitorData loads the YouTube homepage and parses the visitorData embedded
// in its configuration.
func (c *Client) fetchVisitorData(ctx context.Context) (string, error) {
	body, err := c.httpGet(ctx, c.webFallback, newSession(c.gl), "https://www.youtube.com/?hl="+url.QueryEscape(c.hl))
	if err != nil {
		return "", err
	}
	m := visitorDataRe.FindSubmatch(body)
	if m == nil {
		return "", errors.New("visitorData not found on homepage")
	}
	return jsonUnescape(string(m[1])), nil
}

// jsonUnescape decodes JSON string escapes in a captured value. If the capture is
// not a valid JSON string body, the original value is returned unchanged.
func jsonUnescape(s string) string {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err == nil {
		return out
	}
	return s
}

// seedConsentCookie keeps YouTube page fetches out of the consent interstitial.
// The cookie is scoped to www.youtube.com, where the page and InnerTube requests
// are sent.
func seedConsentCookie(jar http.CookieJar) {
	jar.SetCookies(
		&url.URL{Scheme: "https", Host: "www.youtube.com"},
		[]*http.Cookie{{
			Name:  "CONSENT",
			Value: "YES+cb.20210328-17-p0.en+FX+" + strconv.Itoa(rand.IntN(900)+100),
			Path:  "/",
		}},
	)
}
