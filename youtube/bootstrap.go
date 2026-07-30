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
		vd, gen, err := c.resolveAdoptedSession(ctx)
		if err != nil {
			return nil, err
		}
		sess.adoptVisitorData(vd)
		sess.identityGen = gen
		c.log.DebugContext(ctx, "adopted external visitorData", "source", sess.source.String())
		return sess, nil
	}

	sess.identityGen = c.resetSeq.Load() // synthetic fallback; jarless clients never rotate
	if c.http.Jar() == nil {
		return sess, nil
	}
	vd, gen, err := c.bootstrapVisitorData(ctx)
	if err != nil {
		c.log.DebugContext(ctx, "visitor bootstrap failed; using synthetic visitorData", "err", err)
		return sess, nil
	}
	sess.learnVisitorData(vd) // no-op when empty; marks the session bootstrapped
	sess.identityGen = gen
	return sess, nil
}

// bootstrapVisitorData returns server-issued visitorData and the rotation
// generation it belongs to, loading and caching the value once across
// concurrent callers. The page response also seeds the client's jar.
//
// The load runs under resetMu so a RotateIdentity cannot interleave with an
// in-flight bootstrap: an unguarded wipe could strip the cookies the homepage
// response just installed, or land before a flight (started with the old
// cookies) re-caches the old identity, silently defeating the rotation. A reset
// therefore waits for the bootstrap to finish and then discards its result,
// which is correct: that bootstrap began under the identity the reset was asked
// to remove. Reading the generation under the same lock keeps it coherent with
// the returned identity.
func (c *Client) bootstrapVisitorData(ctx context.Context) (string, uint64, error) {
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	vd, err := c.visitors.GetOrLoad(ctx, visitorCacheKey, c.fetchVisitorData)
	return vd, c.resetSeq.Load(), err
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

// RotateIdentity discards the identity that minted a set of failing stream URLs
// so the next extraction runs under a different one.
//
// googlevideo caps stream delivery for a small share of guest sessions: every
// URL such a session resolves answers empty-body 403 to any read past roughly
// the first 1 MB, re-resolves inherit the cap because they reuse the same
// identity, and a fresh identity resolves URLs that work immediately. This
// method is that escape.
//
// A bootstrapped guest identity is discarded here (cached visitorData plus the
// youtube.com cookies anchoring it) and the next extraction bootstraps a fresh
// one. An adopted session is owned by its provider, so rotation is delegated:
// the provider is told it is unusable through [potoken.SessionInvalidator] and
// the cached adoption is dropped, letting the next extraction resolve a
// replacement. videoID is diagnostic context for the provider and may be empty;
// the guest path ignores it.
//
// gen names the identity to discard: the [Extraction.IdentityGeneration] of
// the extraction whose URLs provoked the rotation. The rotation runs only while
// that identity is still current; if another already replaced it, the call
// reports true without acting, because the identity it wanted gone is gone.
// This keys the dedup to the failing identity rather than to call timing:
// simultaneous downloads sharing this client (Concurrency.Downloads) all 403
// together on a flagged identity and collapse into one rotation, while a fresh
// identity that is itself capped (observed live) still rotates again, since
// its extraction carries the new generation.
//
// It reports whether the failing identity is gone. False leaves the caller on
// the plain re-resolve, which inherits the cap: a jarless client has no durable
// guest identity (each extraction mints a fresh synthetic visitorData), a
// static [Config.Session] is fixed by its caller, a [Config.SessionProvider]
// that does not implement [potoken.SessionInvalidator] cannot retire what it
// handed out, and a rejected invalidation leaves the session in place.
func (c *Client) RotateIdentity(ctx context.Context, gen uint64, videoID string) bool {
	if c.adoptionConfigured() {
		return c.rotateAdoptedSession(ctx, gen, videoID)
	}
	if c.http.Jar() == nil {
		return false
	}
	c.resetMu.Lock()
	defer c.resetMu.Unlock()
	if c.resetSeq.Load() != gen {
		return true // that identity was already replaced
	}
	c.visitors.Delete(visitorCacheKey)
	clearYouTubeCookies(c.http.Jar())
	seedConsentCookie(c.http.Jar())
	c.resetSeq.Add(1)
	return true
}

// clearYouTubeCookies expires every cookie the jar holds for youtube.com so the
// next homepage fetch mints a new guest identity instead of re-learning the old
// one from VISITOR_INFO1_LIVE.
//
// jar.Cookies returns name/value only (net/http/cookiejar strips Domain and
// Path), and the jar keys entries by (domain, path, name), so each name is
// expired in both forms it can be stored under: the host-only www.youtube.com
// entry and the youtube.com domain entry (the jar canonicalizes a .youtube.com
// attribute to youtube.com; VISITOR_INFO1_LIVE is stored that way). The
// enumeration sees only cookies that path-match "/", which is where YouTube
// sets its identity cookies (live-verified); a narrower-path cookie cannot be
// discovered through the CookieJar interface at all and would survive the
// wipe. Accepted: the homepage bootstrap that mints the identity is a
// path-"/" request, so a surviving narrow-path cookie cannot re-seed it.
func clearYouTubeCookies(jar http.CookieJar) {
	u := &url.URL{Scheme: "https", Host: "www.youtube.com"}
	cookies := jar.Cookies(u)
	if len(cookies) == 0 {
		return
	}
	expired := make([]*http.Cookie, 0, 2*len(cookies))
	for _, ck := range cookies {
		expired = append(expired,
			&http.Cookie{Name: ck.Name, Path: "/", MaxAge: -1},
			&http.Cookie{Name: ck.Name, Path: "/", Domain: "youtube.com", MaxAge: -1},
		)
	}
	jar.SetCookies(u, expired)
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
//
// It returns the hosts it seeded so a later rotation can expire exactly what
// this adoption installed (see Client.clearAdoptedCookies).
func seedExternalCookies(jar http.CookieJar, cookies []*http.Cookie) []string {
	if jar == nil || len(cookies) == 0 {
		return nil
	}
	byHost := make(map[string][]*http.Cookie)
	for _, ck := range cookies {
		host := strings.TrimPrefix(ck.Domain, ".")
		if host == "" {
			continue
		}
		byHost[host] = append(byHost[host], ck)
	}
	hosts := make([]string, 0, len(byHost))
	for host, cks := range byHost {
		jar.SetCookies(&url.URL{Scheme: "https", Host: host}, cks)
		hosts = append(hosts, host)
	}
	return hosts
}

// expireHostCookies expires every cookie the jar holds for host, in both forms
// it can be keyed under. jar.Cookies returns name/value only, so a cookie stored
// from a Domain attribute cannot be told apart from a host-only one.
func expireHostCookies(jar http.CookieJar, host string) {
	u := &url.URL{Scheme: "https", Host: host}
	cookies := jar.Cookies(u)
	if len(cookies) == 0 {
		return
	}
	expired := make([]*http.Cookie, 0, 2*len(cookies))
	for _, ck := range cookies {
		expired = append(expired,
			&http.Cookie{Name: ck.Name, Path: "/", MaxAge: -1},
			&http.Cookie{Name: ck.Name, Path: "/", Domain: host, MaxAge: -1},
		)
	}
	jar.SetCookies(u, expired)
}
