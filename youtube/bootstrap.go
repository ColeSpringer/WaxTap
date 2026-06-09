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
	"strings"
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

// newBootstrappedSession starts a session for one extraction. When an external
// session is configured it is adopted verbatim and the homepage bootstrap is
// skipped; otherwise a cookie-backed guest identity is bootstrapped, falling back
// to synthetic visitorData.
//
// The error is non-nil only under adoption: a failed adoption is fatal because
// falling back to a random synthetic visitorData would send the wrong
// content_binding to the PO-token minter and guarantee a GVS mismatch. Without
// adoption a failed bootstrap is best-effort and never returns an error.
//
// Bootstrapping is skipped without a cookie jar because the matching cookies
// cannot be preserved. That also keeps injected, jarless test clients on the
// synthetic path.
func (c *Client) newBootstrappedSession(ctx context.Context) (*session, error) {
	sess := newSession(c.gl)

	if c.adoptionConfigured() {
		vd, err := c.resolveAdoptedSession(ctx)
		if err != nil {
			return nil, err
		}
		sess.adoptVisitorData(vd)
		c.log.DebugContext(ctx, "adopted external visitorData", "source", sess.source.String())
		return sess, nil
	}

	if c.http.Jar() == nil {
		return sess, nil
	}
	vd, err := c.bootstrapVisitorData(ctx)
	if err != nil {
		c.log.DebugContext(ctx, "visitor bootstrap failed; using synthetic visitorData", "err", err)
		return sess, nil
	}
	sess.learnVisitorData(vd) // no-op when empty; marks the session bootstrapped
	return sess, nil
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

// loginCookieBaseNames are Google account-authentication cookies, keyed by their
// base name after the __Secure-/__Host- and 1P/3P partition prefixes are stripped.
// Matching on the base catches the whole family (SID, __Secure-1PSID,
// __Secure-3PSID, APISID, SIDCC, SIDTS, and siblings), so a new 1P/3P variant
// cannot slip through a flat denylist. These must never enter an adopted guest
// session: a logged-in identity is account-bound (data_sync_id) and raises ban
// risk, so adoption assumes a genuine guest session.
var loginCookieBaseNames = map[string]bool{
	"SID": true, "HSID": true, "SSID": true, "APISID": true, "SAPISID": true,
	"SIDTS": true, "SIDCC": true, "LOGIN_INFO": true,
}

// isLoginCookie reports whether name is a Google account-auth cookie, regardless
// of its __Secure-/__Host- or 1P/3P prefix.
func isLoginCookie(name string) bool {
	base := strings.TrimPrefix(name, "__Secure-")
	base = strings.TrimPrefix(base, "__Host-")
	base = strings.TrimPrefix(base, "1P")
	base = strings.TrimPrefix(base, "3P")
	return loginCookieBaseNames[base]
}

// filterLoginCookies splits adopted cookies into the guest-safe set and the names
// of dropped login cookies, so the caller can warn about each drop.
func filterLoginCookies(cookies []*http.Cookie) (safe []*http.Cookie, dropped []string) {
	for _, ck := range cookies {
		if isLoginCookie(ck.Name) {
			dropped = append(dropped, ck.Name)
			continue
		}
		safe = append(safe, ck)
	}
	return safe, dropped
}

// seedExternalCookies installs adopted cookies into jar, grouped by domain so each
// SetCookies call has a single coherent origin. It is nil-jar safe (a no-op, since
// visitorData-only adoption needs no jar) and skips domain-less cookies, which the
// jar cannot place without an origin URL.
func seedExternalCookies(jar http.CookieJar, cookies []*http.Cookie) {
	if jar == nil || len(cookies) == 0 {
		return
	}
	byHost := make(map[string][]*http.Cookie)
	for _, ck := range cookies {
		host := strings.TrimPrefix(ck.Domain, ".")
		if host == "" {
			continue
		}
		byHost[host] = append(byHost[host], ck)
	}
	for host, cks := range byHost {
		jar.SetCookies(&url.URL{Scheme: "https", Host: host}, cks)
	}
}
