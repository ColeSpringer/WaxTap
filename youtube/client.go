package youtube

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"sync"
	"time"

	"github.com/colespringer/waxtap/internal/cache"
	"github.com/colespringer/waxtap/internal/clientident"
	"github.com/colespringer/waxtap/internal/diskcache"
	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube/internal/resolver"
)

// Client performs YouTube extraction. It holds configuration and injected
// dependencies, while mutable per-attempt identity lives in session. It is safe
// for concurrent use after construction.
type Client struct {
	http        *httpx.Client
	log         *slog.Logger
	profiles    []ClientProfile
	webFallback ClientProfile                 // built-in WEB profile for ancillary requests
	resolver    resolver.Resolver             // stream-URL resolution
	inspector   resolver.PlayerInspector      // signature timestamp lookup; nil if unsupported
	potoken     potoken.Provider              // PO-token provider, when configured
	webContext  potoken.PlayerContextProvider // attested WEB player-context provider, when configured
	webCtxTO    time.Duration                 // per-call bound on the player-context provider; 0 = none
	visitors    *cache.Store[string]          // bootstrapped guest visitorData, singleflighted
	hl, gl      string                        // InnerTube host language / content region

	// External session adoption. At most one of staticSession / sessionProvider
	// is set (the facade rejects both). When either is set, Extract adopts that
	// identity verbatim and skips the homepage bootstrap; a failed resolution is
	// fatal. The provider is resolved at most once per Client under adoptMu,
	// caching only on success so a transient failure retries on the next call.
	staticSession   *potoken.Session
	sessionProvider potoken.SessionProvider
	adoptMu         sync.Mutex
	adoptVD         string // resolved adopted visitorData
	adoptResolved   bool   // adoptVD is valid
	adoptErr        error  // construction-time adoption error, surfaced at resolve
	adoptSeeded     bool   // adopted cookies have been seeded into the jar
}

// Config configures a Client.
type Config struct {
	// HTTP is the retrying HTTP client. If nil, a default httpx.Client is used.
	HTTP *httpx.Client
	// Logger receives debug logs. If nil, logging is discarded.
	Logger *slog.Logger
	// Profiles overrides the client strategy chain. If empty, DefaultProfiles().
	Profiles []ClientProfile
	// ChromeMajor overrides the emulated Chrome major for built-in WEB-family
	// identities. It applies to the default profile chain and to built-in WEB
	// requests used for discovery and fallbacks. Zero selects the built-in
	// default. Caller-supplied Profiles are unchanged.
	ChromeMajor int
	// Resolver resolves candidate formats into playable URLs. If nil, New builds
	// a default base.js/goja resolver over the same HTTP client. Metadata
	// extraction does not need one; stream resolution (Resolve) does.
	Resolver resolver.Resolver
	// POTokenProvider supplies PO tokens when a profile requires one. WaxTap may
	// call it during extraction for a player-scope token and during resolution for
	// a GVS-scope stream token. Nil means no provider is configured.
	POTokenProvider potoken.Provider
	// PlayerContextProvider supplies an attested WEB /player streaming context,
	// enabling the opt-in WEB SABR audio path (see Client.ExtractWebContext). Nil
	// leaves WaxTap on its default extraction chain.
	PlayerContextProvider potoken.PlayerContextProvider
	// WebContextTimeout bounds each PlayerContextProvider call, both the initial
	// extraction and a mid-stream reload's re-fetch, so a hung provider cannot
	// hang a download. Zero adds no bound.
	WebContextTimeout time.Duration
	// Session is an externally supplied guest identity the Client adopts verbatim,
	// skipping its own homepage bootstrap. It is pre-resolved: its cookies are
	// seeded at New and its visitorData is used for every extraction. Use it for
	// byte-exact session coherence with a PO-token minter. The caller must select
	// a uniform client chain (the facade enforces this); nil leaves the built-in
	// bootstrap in place.
	Session *potoken.Session
	// SessionProvider resolves an external guest identity lazily, at most once per
	// Client (cached on success). It is the pull-based form of Session. At most one
	// of Session / SessionProvider may be set.
	SessionProvider potoken.SessionProvider
	// ResolveTimeout bounds each cipher JS execution during resolution. Zero
	// uses the resolver default.
	ResolveTimeout time.Duration
	// CacheDir is the base directory for on-disk caches. When set, and when
	// DisableDiskCache is false, the default resolver stores base.js source under
	// CacheDir/players. Empty leaves the resolver memory-only. Ignored when a
	// Resolver is injected.
	CacheDir string
	// DisableDiskCache turns off the on-disk base.js source cache even when
	// CacheDir is set.
	DisableDiskCache bool
	// HL (host language, e.g. "en") and GL (content region, e.g. "US") set the
	// InnerTube locale. Empty defaults to en / US. These are localization hints:
	// geo-restricted availability is still governed by the request IP.
	HL string
	GL string
}

// playerCacheSchema namespaces the on-disk base.js source cache. Bump it if the
// persisted representation ever changes so older files are ignored.
const playerCacheSchema = 1

// New returns a Client, applying defaults for unset Config fields.
func New(cfg Config) *Client {
	c := &Client{
		http:            cfg.HTTP,
		log:             cfg.Logger,
		profiles:        cfg.Profiles,
		resolver:        cfg.Resolver,
		potoken:         cfg.POTokenProvider,
		webContext:      cfg.PlayerContextProvider,
		webCtxTO:        cfg.WebContextTimeout,
		hl:              cfg.HL,
		gl:              cfg.GL,
		staticSession:   cfg.Session,
		sessionProvider: cfg.SessionProvider,
	}
	// Use one built-in WEB User-Agent for default profiles and ancillary WEB
	// requests. Caller-supplied profiles retain their own user agents.
	webUA := clientident.UserAgent(cfg.ChromeMajor)
	web := profileWeb
	web.UserAgent = webUA
	c.webFallback = makeProfile(web)
	if c.http == nil {
		// The default client owns a cookie jar so guest-session cookies survive
		// the visitor bootstrap and later player requests. Per-request contexts,
		// not a global client timeout, bound operations.
		jar, _ := cookiejar.New(nil)
		c.http = httpx.New(httpx.Config{HTTPClient: &http.Client{Jar: jar}})
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if len(c.profiles) == 0 {
		c.profiles = buildDefaultProfiles(webUA)
	}
	if c.resolver == nil {
		var source resolver.SourceCache
		if cfg.CacheDir != "" && !cfg.DisableDiskCache {
			source = diskcache.New(diskcache.Options{
				Dir:           filepath.Join(cfg.CacheDir, "players"),
				SchemaVersion: playerCacheSchema,
				Logger:        c.log,
			})
		}
		c.resolver = resolver.New(resolver.Config{
			HTTP:               c.http,
			Logger:             c.log,
			CipherTimeout:      cfg.ResolveTimeout,
			SourceCache:        source,
			DiscoveryUserAgent: webUA,
		})
	}
	// The default resolver supports player inspection. Injected resolvers may
	// implement only Resolver.
	if pi, ok := c.resolver.(resolver.PlayerInspector); ok {
		c.inspector = pi
	}
	if c.hl == "" {
		c.hl = "en"
	}
	if c.gl == "" {
		c.gl = "US"
	}
	c.visitors = cache.NewStore[string](cache.Options{TTL: visitorTTL, MaxEntries: 4})
	if jar := c.http.Jar(); jar != nil {
		seedConsentCookie(jar)
	}
	// A static session is pre-resolved: seed its cookies now and store its
	// visitorData so extraction needs no network for adoption. New returns no
	// error, so an empty visitorData or a cookies/no-jar mismatch is held in
	// adoptErr and surfaced when Extract resolves the session (the facade also
	// rejects an empty visitorData up front). An empty visitorData must fail, not
	// be silently adopted: it would break GVS content_binding coherence and, once
	// the source is "adopted", block learnVisitorData from recovering.
	if c.staticSession != nil {
		switch {
		case c.staticSession.VisitorData == "":
			c.adoptErr = errors.New("adopted session has an empty visitorData")
		default:
			if err := c.seedAdoptedCookies(c.staticSession.Cookies); err != nil {
				c.adoptErr = err
			} else {
				c.adoptVD = c.staticSession.VisitorData
				c.adoptResolved = true
			}
		}
	}
	return c
}

// adoptionConfigured reports whether an external session must be adopted.
func (c *Client) adoptionConfigured() bool {
	return c.staticSession != nil || c.sessionProvider != nil
}

// resolveAdoptedSession returns the adopted visitorData, resolving a
// SessionProvider at most once. The provider runs under the first caller's
// context while adoptMu is held, so concurrent extractions share one resolution;
// the result is cached only on success, so a transient provider failure is
// retried on the next call. A static session is already resolved at New.
func (c *Client) resolveAdoptedSession(ctx context.Context) (string, error) {
	c.adoptMu.Lock()
	defer c.adoptMu.Unlock()
	if c.adoptErr != nil {
		return "", c.adoptErr
	}
	if c.adoptResolved {
		return c.adoptVD, nil
	}
	sess, err := c.sessionProvider.ProvideSession(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("adopted session provider failed: %w", err)
	}
	if sess.VisitorData == "" {
		return "", errors.New("adopted session provider returned an empty visitorData")
	}
	if err := c.seedAdoptedCookies(sess.Cookies); err != nil {
		// Cookies + no jar is a permanent configuration error, not a transient
		// provider failure: cache it so later Extracts short-circuit instead of
		// re-running the remote provider on every call.
		c.adoptErr = err
		return "", err
	}
	c.adoptVD = sess.VisitorData
	c.adoptResolved = true
	return c.adoptVD, nil
}

// seedAdoptedCookies installs an adopted session's cookies into the jar, once.
// Login cookies are dropped with a warning (adoption assumes a guest session),
// and supplying guest cookies without a jar is an error rather than a silent
// drop. visitorData-only adoption needs no jar and is fine. The caller holds
// adoptMu, or calls during New before any concurrency.
func (c *Client) seedAdoptedCookies(cookies []*http.Cookie) error {
	if c.adoptSeeded {
		return nil
	}
	safe, dropped := filterLoginCookies(cookies)
	for _, name := range dropped {
		c.log.Warn("dropping login cookie from adopted session; adoption assumes a logged-out guest session", "cookie", name)
	}
	if len(safe) > 0 && c.http.Jar() == nil {
		return errors.New("adopted session supplies cookies but the HTTP client has no cookie jar: pass an *http.Client with a jar, or supply visitorData only")
	}
	seedExternalCookies(c.http.Jar(), safe)
	c.adoptSeeded = true
	return nil
}

// Extract fetches metadata and candidate audio formats for videoID, trying the
// client strategy chain in order until one succeeds. Each attempt uses an
// immutable profile and a shared per-extraction session; the winning profile and
// session are carried in the returned Extraction so stream resolution uses the
// same identity.
func (c *Client) Extract(ctx context.Context, videoID string) (*Extraction, error) {
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return nil, err // fatal only under adoption; otherwise newBootstrappedSession never errors
	}
	var lastErr error

	for i, profile := range c.profiles {
		// A failed profile must not carry its PO-token binding into the next
		// attempt. The winning attempt returns with its binding intact.
		sess.resetPOBinding()

		// WEB-family profiles need a player-scope PO token in the /player body
		// before YouTube returns usable stream URLs. Profiles that do not list
		// ScopePlayer skip the provider lookup.
		playerResp, err := c.fetchPOToken(ctx, profile, sess, videoID, potoken.ScopePlayer, nil)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = err
			c.log.DebugContext(ctx, "player PO token unavailable; trying next client", "client", profile.Name, "err", err)
			continue
		}
		var playerTok string
		if playerResp != nil {
			playerTok = playerResp.Token
		}

		// WEB-family clients require the base.js signature timestamp in the
		// /player body. If lookup fails, this attempt proceeds without it.
		sts := c.signatureTimestamp(ctx, profile, videoID)

		body, err := c.innertubePost(ctx, profile, sess, playerEndpoint, c.newPlayerRequest(profile, sess, playerRequestOpts{VideoID: videoID, POToken: playerTok, STS: sts}))
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			if errors.Is(err, waxerr.ErrRateLimited) {
				return nil, err // throttling won't differ across clients; surface it
			}
			lastErr = err
			c.log.DebugContext(ctx, "player request failed; trying next client", "client", profile.Name, "err", err)
			continue
		}

		pr, err := parsePlayerResponse(body)
		if err != nil {
			lastErr = &waxerr.ExtractionError{Stage: "player-response", Cause: err}
			c.log.DebugContext(ctx, "player-response parse failed; trying next client", "client", profile.Name, "err", err)
			c.dumpArtifact(ctx, "playerresponse-"+profile.Name+"-"+videoID+".json", body)
			continue
		}
		sess.learnVisitorData(pr.ResponseContext.VisitorData)

		if perr := pr.playabilityError(); perr != nil {
			// Playability failures can be client-specific: stale versions, sparse
			// context, and bot checks often report generic ERROR or UNPLAYABLE.
			// Keep the latest error, try the remaining clients, then fall back to
			// the watch page before returning it.
			lastErr = perr
			// sts==0 here means the timestamp lookup failed, so the resulting
			// UNPLAYABLE is a maintenance problem, not an unavailable video.
			// Reclassify it so the terminal error names that cause instead of a
			// generic ErrVideoUnavailable.
			if profile.NeedsSignatureTimestamp && sts == 0 {
				lastErr = missingTimestampError(perr)
			}
			c.log.DebugContext(ctx, "playability failure; trying next client", "client", profile.Name, "err", lastErr)
			c.dumpArtifact(ctx, "playerresponse-"+profile.Name+"-"+videoID+".json", body)
			continue
		}

		video, raw, err := pr.toVideo(videoID)
		if err != nil {
			lastErr = err
			c.log.DebugContext(ctx, "no usable formats; trying next client", "client", profile.Name, "err", err)
			c.dumpArtifact(ctx, "playerresponse-"+profile.Name+"-"+videoID+".json", body)
			continue
		}

		if i > 0 {
			c.log.DebugContext(ctx, "extracted via fallback client", "client", profile.Name)
		}
		return buildExtraction(video, profile, sess, raw, pr), nil
	}

	// Final fallback: scrape the watch page for ytInitialPlayerResponse. This
	// reaches some embed-disabled videos the InnerTube clients refused. It does
	// not override a more specific terminal error from the chain.
	if ctx.Err() == nil && !errors.Is(lastErr, waxerr.ErrRateLimited) {
		if ext, ferr := c.extractFromWatchPage(ctx, videoID); ferr == nil {
			c.log.DebugContext(ctx, "extracted via watch-page fallback")
			return ext, nil
		} else if lastErr == nil {
			lastErr = ferr
		}
	}

	if lastErr == nil {
		lastErr = waxerr.ErrExtractionFailed
	}
	return nil, lastErr
}

// signatureTimestamp returns the base.js signature timestamp required by
// profile. It returns zero when the profile does not require one, the resolver
// cannot inspect players, or lookup fails. Player discovery starts with videoID,
// and the resolver caches fetched player JavaScript for reuse by later profiles
// and extractions.
func (c *Client) signatureTimestamp(ctx context.Context, profile ClientProfile, videoID string) int {
	if !profile.NeedsSignatureTimestamp || c.inspector == nil {
		return 0 // not needed for this profile (the zeros below are failures)
	}
	// A profile that needs the timestamp but gets zero produces an UNPLAYABLE
	// /player response, so both a lookup error and a resolved-zero are warned (not
	// silently swallowed) to separate "not needed" from "lookup failed".
	sts, err := c.inspector.SignatureTimestamp(ctx, resolver.Context{VideoID: videoID})
	if err != nil {
		c.log.WarnContext(ctx, "signature timestamp lookup failed; omitting field (expect UNPLAYABLE)", "client", profile.Name, "err", err)
		return 0
	}
	if sts == 0 {
		c.log.WarnContext(ctx, "signature timestamp resolved to zero; omitting field (expect UNPLAYABLE)", "client", profile.Name)
	}
	return sts
}

// missingTimestampError turns an sts=0 UNPLAYABLE into an ExtractionError
// (ErrExtractionFailed) that names the timestamp as the cause. perr's status is
// copied into the message as text, not wrapped, so the result does not also
// match errors.Is(err, ErrVideoUnavailable).
func missingTimestampError(perr error) error {
	status := "UNPLAYABLE"
	if pe, ok := errors.AsType[*waxerr.PlayabilityError](perr); ok && pe.Status != "" {
		status = pe.Status
	}
	return &waxerr.ExtractionError{
		Stage: "signature-timestamp",
		Cause: fmt.Errorf("WEB signature timestamp unavailable (sts=0): player returned %s", status),
	}
}

// extractFromWatchPage fetches the watch page and parses the embedded
// ytInitialPlayerResponse, as a fallback when the InnerTube clients fail.
func (c *Client) extractFromWatchPage(ctx context.Context, videoID string) (*Extraction, error) {
	profile := c.webFallback
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return nil, err
	}

	body, err := c.httpGet(ctx, profile, sess, "https://www.youtube.com/watch?v="+videoID+"&bpctr=9999999999&has_verified=1")
	if err != nil {
		return nil, err
	}
	pr, err := parseWatchPage(body)
	if err != nil {
		c.dumpArtifact(ctx, "watchpage-"+videoID+".html", body)
		return nil, &waxerr.ExtractionError{Stage: "watch-page", Cause: err}
	}
	sess.learnVisitorData(pr.ResponseContext.VisitorData)
	if perr := pr.playabilityError(); perr != nil {
		return nil, perr
	}
	video, raw, err := pr.toVideo(videoID)
	if err != nil {
		return nil, err
	}
	return buildExtraction(video, profile, sess, raw, pr), nil
}

// Info returns just the video metadata and candidate formats.
func (c *Client) Info(ctx context.Context, videoID string) (*Video, error) {
	ext, err := c.Extract(ctx, videoID)
	if err != nil {
		return nil, err
	}
	return ext.video, nil
}

// Bounds for retrying the initial playlist browse page. Continuation pages
// soft-fail per page instead.
const (
	browseRetries    = 2 // extra attempts after the first
	browseRetryDelay = 500 * time.Millisecond
)

// Enumerate expands a playlist into lightweight entries without downloading. It
// pages through continuations until exhausted or maxItems (<= 0 means all) is
// reached. Per-page failures after the first page are collected in Playlist.Errors
// rather than discarding the entries already gathered.
func (c *Client) Enumerate(ctx context.Context, playlistID string, maxItems int) (*Playlist, error) {
	profile := c.playlistProfile()
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return nil, err
	}
	pl := &Playlist{ID: playlistID}

	meta, items, token, err := c.browseInitial(ctx, profile, sess, playlistID)
	if err != nil {
		return nil, err
	}
	sess.learnVisitorData(meta.visitorData)
	pl.Title = meta.title
	pl.Author = meta.author
	truncated := c.appendPlaylistItems(pl, items, maxItems)

	for token != "" && !reachedLimit(pl, maxItems) {
		if err := ctx.Err(); err != nil {
			return pl, err
		}
		body, err := c.innertubePost(ctx, profile, sess, browseEndpoint, c.newPlaylistRequest(profile, sess, "", token))
		if err != nil {
			pl.Errors = append(pl.Errors, err)
			break
		}
		var items []playlistItem
		items, token, err = parseBrowseContinuation(body)
		if err != nil {
			pl.Errors = append(pl.Errors, err)
			break
		}
		truncated = c.appendPlaylistItems(pl, items, maxItems)
	}

	// Continuation tokens are page-granular. Only expose one when the current
	// page was fully consumed: a mid-page maxItems cutoff has no precise resume
	// point, and surfacing the next-page token would silently skip this page's
	// unreturned entries.
	if reachedLimit(pl, maxItems) && !truncated {
		pl.Continuation = token
	}
	return pl, nil
}

// browseInitial fetches and parses the first browse page of a playlist,
// retrying transient failures a bounded number of times so a single flaky
// page does not fail the whole enumeration.
func (c *Client) browseInitial(ctx context.Context, profile ClientProfile, sess *session, playlistID string) (playlistMeta, []playlistItem, string, error) {
	for attempt := 0; ; attempt++ {
		body, err := c.innertubePost(ctx, profile, sess, browseEndpoint, c.newPlaylistRequest(profile, sess, playlistID, ""))
		if err == nil {
			meta, items, token, perr := parseBrowseInitial(body)
			if perr == nil {
				return meta, items, token, nil
			}
			err = perr
		}
		if attempt >= browseRetries || !retryableBrowse(err) {
			return playlistMeta{}, nil, "", err
		}
		c.log.DebugContext(ctx, "initial browse failed; retrying", "attempt", attempt+1, "playlist", playlistID, "err", err)
		if err := httpx.Sleep(ctx, browseRetryDelay); err != nil {
			return playlistMeta{}, nil, "", err
		}
	}
}

// retryableBrowse reports whether an initial-browse failure is worth another
// same-session attempt: only an unrecognized page shape (A/B experiments can
// be per-request). Transient HTTP statuses are not re-retried here: httpx
// already retried them with backoff, so a surfaced status error is exhausted.
// Context errors and hard bad-id failures are surfaced as-is.
func retryableBrowse(err error) bool {
	return errors.Is(err, waxerr.ErrPlaylistParse)
}

// playlistProfile returns the first configured profile that supports browse
// requests, falling back to the built-in WEB profile.
func (c *Client) playlistProfile() ClientProfile {
	for _, p := range c.profiles {
		if p.SupportsPlaylists {
			return p
		}
	}
	return c.webFallback
}

// appendPlaylistItems adds entries up to maxItems. It returns true when the
// limit is reached before the current page is fully consumed; continuation
// tokens cannot resume from that mid-page position.
func (c *Client) appendPlaylistItems(pl *Playlist, items []playlistItem, maxItems int) (truncated bool) {
	for _, it := range items {
		if reachedLimit(pl, maxItems) {
			return true
		}
		entry, err := it.toEntry(len(pl.Entries))
		if err != nil {
			pl.Errors = append(pl.Errors, err) // partial enumeration is not fatal
			continue
		}
		pl.Entries = append(pl.Entries, entry)
	}
	return false
}

func reachedLimit(pl *Playlist, maxItems int) bool {
	return maxItems > 0 && len(pl.Entries) >= maxItems
}
