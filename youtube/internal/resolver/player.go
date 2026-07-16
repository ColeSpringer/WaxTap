package resolver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v3/internal/cache"
	"github.com/colespringer/waxtap/v3/internal/clientident"
	"github.com/colespringer/waxtap/v3/internal/iox"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// schemaVersion namespaces cached player programs and discovered player URLs.
// Bump it when the extraction or program shape changes so stale entries from an
// older build are treated as misses. v2: whole-player solver (solver.go).
const schemaVersion = 2

const (
	// defaultCipherTimeout bounds a single goja transform execution.
	defaultCipherTimeout = 5 * time.Second
	// playerURLCacheKey keys the discovered base.js URL. base.js is global to
	// YouTube (not per-video), so one live entry suffices.
	playerURLCacheKey = "current"
	// maxPlayerBytes bounds how much of base.js / a discovery page we buffer.
	maxPlayerBytes = 16 << 20
	// defaultPlayerCacheEntries caps retained compiled players when CacheSize is
	// unset. A compiled whole-player is large (tens of MB), and only a few are
	// ever live (regular + embed across a rotation), so the general cache default
	// (256) would be a multi-GB ceiling. Six is ample headroom at a modest cap.
	defaultPlayerCacheEntries = 6
)

// baseJSPathRe matches the base.js path embedded in an embed/watch page.
var baseJSPathRe = regexp.MustCompile(`/s/player/[a-zA-Z0-9_-]+/[a-zA-Z0-9_.\-/]+/base\.js`)

// expirePathRe matches an expiry encoded in a stream URL path (/expire/<unix>/),
// an alternative to the expire query parameter.
var expirePathRe = regexp.MustCompile(`/expire/(\d+)`)

// HTTPDoer is the subset of an HTTP client the resolver needs. *httpx.Client
// satisfies it; it is an interface so tests can stub transport without the
// resolver importing httpx.
type HTTPDoer interface {
	// Do executes an HTTP request.
	Do(*http.Request) (*http.Response, error)
}

// Config configures a Player resolver.
type Config struct {
	// HTTP fetches base.js and discovery pages. Required.
	HTTP HTTPDoer
	// Logger receives debug/warn logs. If nil, logging is discarded.
	Logger *slog.Logger
	// CipherTimeout bounds each goja transform execution. Zero uses the default.
	CipherTimeout time.Duration
	// CacheSize caps retained compiled players. Zero uses the cache default.
	CacheSize int
	// SourceCache persists base.js source across process runs. Nil keeps the
	// resolver memory-only.
	SourceCache SourceCache
	// DiscoveryUserAgent is sent when fetching discovery pages and base.js. Empty
	// uses the default built-in WEB User-Agent.
	DiscoveryUserAgent string
}

// Player resolves candidate formats into playable, signed stream URLs by locating
// and running YouTube's base.js cipher in goja. Compiled players and the
// discovered base.js URL are cached (with singleflight) so concurrent resolutions
// sharing a player do not stampede. It is safe for concurrent use.
type Player struct {
	http        HTTPDoer
	log         *slog.Logger
	timeout     time.Duration
	discoveryUA string // User-Agent for base.js / discovery fetches

	programs *cache.Store[*playerProgram] // in-memory, keyed by base.js URL
	urls     *cache.Store[string]         // discovered base.js URL (global)
	source   SourceCache                  // optional cross-run base.js source cache
}

// New returns a Player with defaults applied.
func New(cfg Config) *Player {
	p := &Player{
		http:        cfg.HTTP,
		log:         cfg.Logger,
		timeout:     cfg.CipherTimeout,
		discoveryUA: cfg.DiscoveryUserAgent,
		source:      cfg.SourceCache,
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.timeout <= 0 {
		p.timeout = defaultCipherTimeout
	}
	if p.discoveryUA == "" {
		p.discoveryUA = clientident.UserAgent(0)
	}
	programCacheEntries := cfg.CacheSize
	if programCacheEntries <= 0 {
		programCacheEntries = defaultPlayerCacheEntries
	}
	p.programs = cache.NewStore[*playerProgram](cache.Options{
		MaxEntries:    programCacheEntries,
		SchemaVersion: schemaVersion,
		TTL:           6 * time.Hour,
	})
	p.urls = cache.NewStore[string](cache.Options{
		MaxEntries:    4,
		SchemaVersion: schemaVersion,
		TTL:           time.Hour,
	})
	return p
}

// Resolve turns a candidate into a playable stream URL. It deciphers
// signatureCipher bundles, best-effort decodes the throttling n parameter,
// attaches any supplied PO token, and reports URL metadata from the final query.
func (p *Player) Resolve(ctx context.Context, rc Context, cand Candidate) (Stream, error) {
	if cand.URL == "" && cand.SignatureCipher == "" {
		return Stream{}, fmt.Errorf("%w: candidate has neither URL nor signatureCipher", waxerr.ErrExtractionFailed)
	}

	// Open one solving session lazily and share it between the signature and n
	// transforms, so the (large) player executes once per resolution rather than
	// once per transform. A ciphered URL cannot be resolved without it, but a
	// direct URL only needs it to decode n and should survive player discovery or
	// base.js fetch failures, keeping the original n value.
	var (
		sess       *solveSession
		sessErr    error
		sessLoaded bool
	)
	session := func() (*solveSession, error) {
		if !sessLoaded {
			sessLoaded = true
			var playerURL string
			if playerURL, sessErr = p.playerURL(ctx, rc); sessErr == nil {
				var prog *playerProgram
				if prog, sessErr = p.program(ctx, playerURL); sessErr == nil {
					sess, sessErr = prog.openSession(ctx, p.timeout)
				}
			}
		}
		return sess, sessErr
	}
	defer func() {
		if sess != nil {
			sess.close()
		}
	}()

	base, sigParam, sigValue := cand.URL, "", ""
	if cand.SignatureCipher != "" {
		s, err := session()
		if err != nil {
			return Stream{}, err // a ciphered URL is unusable without the player
		}
		if base, sigParam, sigValue, err = decipherCipher(s, cand.SignatureCipher); err != nil {
			return Stream{}, err
		}
	}

	u, err := url.Parse(base)
	if err != nil {
		return Stream{}, fmt.Errorf("%w: parse stream URL: %v", waxerr.ErrExtractionFailed, err)
	}
	q := u.Query()
	if sigParam != "" {
		q.Set(sigParam, sigValue)
	}

	// Decode n when possible. If the player is unavailable or decode fails, keep
	// the original throttled value; caller cancellation still aborts resolution.
	if n := q.Get("n"); n != "" {
		if s, perr := session(); perr != nil {
			if ctx.Err() != nil {
				return Stream{}, ctx.Err()
			}
			p.log.WarnContext(ctx, "player unavailable; n not decoded, stream may be throttled", "err", perr)
		} else if decoded, derr := s.solve(kindN, n); derr != nil {
			if ctx.Err() != nil {
				return Stream{}, ctx.Err()
			}
			p.log.WarnContext(ctx, "n-parameter decode failed; stream may be throttled", "err", derr)
		} else {
			q.Set("n", decoded)
		}
	}

	headers := cloneHeader(rc.Headers)
	applyToken(q, headers, rc.Token)
	u.RawQuery = q.Encode()

	return Stream{
		URL:           u.String(),
		ExpiresAt:     streamExpiry(q, u.Path, rc.Token),
		ContentLength: parseInt64(q.Get("clen")),
		Headers:       headers,
	}, nil
}

// SignatureTimestamp returns the signature timestamp embedded in base.js.
// rc.PlayerURL selects the player directly; otherwise discovery starts from
// rc.VideoID. If the compiled player has no recognized timestamp, it returns
// zero without an error.
func (p *Player) SignatureTimestamp(ctx context.Context, rc Context) (int, error) {
	prog, err := p.inspectProgram(ctx, rc)
	if err != nil {
		return 0, err
	}
	if !prog.stsOK {
		return 0, nil
	}
	return prog.sts, nil
}

// DescrambleN rewrites rawURL's throttling n parameter using the current base.js.
// It returns URLs without an n parameter unchanged.
//
// Player selection uses rc.PlayerURL when present and otherwise discovers the
// player from rc.VideoID. The compiled program is shared with SignatureTimestamp
// and Resolve.
func (p *Player) DescrambleN(ctx context.Context, rc Context, rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: parse URL before descrambling n: %v", waxerr.ErrExtractionFailed, err)
	}
	q := u.Query()
	n := q.Get("n")
	if n == "" {
		return rawURL, nil // nothing to solve
	}
	prog, err := p.inspectProgram(ctx, rc)
	if err != nil {
		return "", err
	}
	decoded, err := prog.decodeN(ctx, n, p.timeout)
	if err != nil {
		return "", err
	}
	q.Set("n", decoded)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// inspectProgram loads the compiled player selected by rc without resolving a
// stream candidate.
func (p *Player) inspectProgram(ctx context.Context, rc Context) (*playerProgram, error) {
	resolved, err := p.playerURL(ctx, rc)
	if err != nil {
		return nil, err
	}
	return p.program(ctx, resolved)
}

// decipherCipher resolves a signatureCipher bundle into a base stream URL plus
// the signature query-parameter name and deciphered value to attach. The session
// deciphers the signature against the already-loaded player.
func decipherCipher(sess *solveSession, cipher string) (base, sigParam, sigValue string, err error) {
	params, err := url.ParseQuery(cipher)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: parse signatureCipher: %v", waxerr.ErrExtractionFailed, err)
	}
	base = params.Get("url")
	if base == "" {
		return "", "", "", fmt.Errorf("%w: signatureCipher missing url", waxerr.ErrExtractionFailed)
	}
	s := params.Get("s")
	if s == "" {
		return base, "", "", nil // already signed / no signature component
	}
	decoded, err := sess.solve(kindSig, s)
	if err != nil {
		return "", "", "", err
	}
	sigParam = params.Get("sp")
	if sigParam == "" {
		sigParam = "signature"
	}
	return base, sigParam, decoded, nil
}

// playerURL returns the base.js URL: the caller-supplied one if present, else the
// discovered (and cached) current player URL.
func (p *Player) playerURL(ctx context.Context, rc Context) (string, error) {
	if rc.PlayerURL != "" {
		return stripTCEVariant(absolutePlayerURL(rc.PlayerURL)), nil
	}
	return p.urls.GetOrLoad(ctx, playerURLCacheKey, func(ctx context.Context) (string, error) {
		return p.discoverPlayerURL(ctx, rc.VideoID)
	})
}

// stripTCEVariant rewrites a base.js URL that points at a "trusted client
// experiment" (_tce) player build to the regular build of the same player. The
// _tce build is interpreter-obfuscated: its signature/n transforms are encoded
// as data rather than statically locatable functions, so the whole-player solver
// cannot drive them. The regular build of the same player hash implements the
// identical transforms (same signatureTimestamp, same output), and the solver
// handles it, so this keeps WaxTap on the solvable variant now that YouTube
// serves _tce from the watch page. A URL without the _tce marker is unchanged.
func stripTCEVariant(playerURL string) string {
	return strings.Replace(playerURL, "_tce.vflset", ".vflset", 1)
}

// discoverPlayerURL finds the current base.js URL from the watch page, falling
// back to the embed page. The watch page now serves the "trusted client
// experiment" (_tce) build; stripTCEVariant rewrites it to the regular
// player_es6 build of the same player, which the whole-player solver and
// signature-timestamp patterns target. The embedded player_embed_es6 build
// (served from /embed) is minified differently and is only a fallback for the
// rare video whose watch page omits the player.
func (p *Player) discoverPlayerURL(ctx context.Context, videoID string) (string, error) {
	sources := []string{
		// Regular player_es6 build (yt-dlp parity). bpctr/has_verified clear the
		// consent interstitial that otherwise returns a non-player page (parity
		// with extractFromWatchPage), the most likely cause of a WEB-forced
		// discovery missing base.js.
		"https://www.youtube.com/watch?v=" + url.QueryEscape(videoID) + "&bpctr=9999999999&has_verified=1",
		"https://www.youtube.com/embed/" + url.PathEscape(videoID), // fallback (some videos disable embedding)
	}
	var lastErr error
	for _, src := range sources {
		body, err := p.get(ctx, src)
		if err != nil {
			lastErr = err
			continue
		}
		if path := baseJSPathRe.Find(body); path != nil {
			return stripTCEVariant("https://www.youtube.com" + string(path)), nil
		}
		lastErr = fmt.Errorf("%w: base.js URL not found at %s", waxerr.ErrExtractionFailed, src)
	}
	return "", lastErr
}

// program returns the compiled player for playerURL, using the in-memory program
// cache first, then the optional source cache, then the network. Only base.js
// source is persisted; goja programs are rebuilt in each process.
//
// A fetched player may be returned even when one transform failed to extract.
// The missing-transform error is surfaced only if that transform is used, which
// avoids refetching a real player on every resolution.
//
// Source cache entries are accepted only after at least one transform compiles.
// That filters out bogus HTTP 200 bodies such as HTML interstitials, captive
// portals, or truncated responses without discarding a real player whose other
// transform is temporarily unrecognized.
func (p *Player) program(ctx context.Context, playerURL string) (*playerProgram, error) {
	return p.programs.GetOrLoad(ctx, playerURL, func(ctx context.Context) (*playerProgram, error) {
		if p.source != nil {
			if body, ok := p.source.Get(playerURL); ok {
				if prog := compilePlayerProgram(playerURL, string(body)); prog.extractedTransform() {
					return prog, nil
				}
				// Older cache files may predate validation. Treat a cached body
				// with no transforms as a miss and fetch a fresh player.
			}
		}
		body, err := p.get(ctx, playerURL)
		if err != nil {
			return nil, err
		}
		prog := compilePlayerProgram(playerURL, string(body))
		if !prog.stsOK {
			// Bug B observability: capture the body shape at the fetch site so a
			// consent/HTML interstitial (discovery problem) is distinguishable
			// from real base.js whose sts regex missed (extraction problem).
			p.log.WarnContext(ctx, "signature timestamp not found in fetched player",
				"player", playerURL, "bodyLen", len(body), "bodyPrefix", bodyPrefix(body, 120))
		}
		if p.source != nil && prog.extractedTransform() {
			p.source.Put(playerURL, body)
		}
		return prog, nil
	})
}

// get performs a bounded GET with the configured discovery user agent.
func (p *Player) get(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", p.discoveryUA)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &waxerr.HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: rawURL}
	}
	// Cap the read so an over-limit body fails clearly instead of silently truncating
	// into corrupt JavaScript the cipher solver would later reject with a cryptic
	// error. Classify a cap breach as an extraction failure, like the other
	// resolve-path errors here.
	buf, err := iox.ReadAllCapped(resp.Body, maxPlayerBytes, "player resource")
	if err != nil {
		if errors.Is(err, iox.ErrResponseTooLarge) {
			return nil, fmt.Errorf("%w: %v", waxerr.ErrExtractionFailed, err)
		}
		return nil, err
	}
	// get serves both base.js and the watch/embed discovery page. This fires only on
	// a real fetch (not a cache hit), so it marks the happy path: base.js is fetched
	// to decode n during stream resolution.
	p.log.DebugContext(ctx, "player resource fetched", "url", rawURL, "bytes", len(buf))
	return buf, nil
}

// absolutePlayerURL upgrades a root-relative player path to an absolute URL.
func absolutePlayerURL(playerURL string) string {
	if strings.HasPrefix(playerURL, "/") {
		return "https://www.youtube.com" + playerURL
	}
	return playerURL
}

// streamExpiry returns the earlier of the signed URL expiry and the PO-token
// expiry. A URL with an expired PO token can 403 before its expire parameter.
func streamExpiry(q url.Values, path string, tok *Token) time.Time {
	exp := ExpiryFromURL(q, path)
	if tok != nil && !tok.Expires.IsZero() && (exp.IsZero() || tok.Expires.Before(exp)) {
		return tok.Expires
	}
	return exp
}

// ExpiryFromURL parses a signed googlevideo URL's expiry, preferring the
// expire query parameter and falling back to an /expire/<unix>/ path segment.
// The zero time means no expiry was found.
func ExpiryFromURL(q url.Values, path string) time.Time {
	if secs := parseInt64(q.Get("expire")); secs > 0 {
		return time.Unix(secs, 0).UTC()
	}
	if m := expirePathRe.FindStringSubmatch(path); m != nil {
		if secs, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return time.Unix(secs, 0).UTC()
		}
	}
	return time.Time{}
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

// bodyPrefix returns up to n bytes of body as a quoted, single-line string for
// logging, so a fetched body's shape (markup vs base.js) is legible in one line.
func bodyPrefix(body []byte, n int) string {
	if len(body) > n {
		body = body[:n]
	}
	return strconv.Quote(string(body))
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
