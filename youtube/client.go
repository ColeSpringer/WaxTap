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

	"github.com/colespringer/waxtap/v2/internal/cache"
	"github.com/colespringer/waxtap/v2/internal/clientident"
	"github.com/colespringer/waxtap/v2/internal/diskcache"
	"github.com/colespringer/waxtap/v2/internal/httpx"
	"github.com/colespringer/waxtap/v2/potoken"
	"github.com/colespringer/waxtap/v2/waxerr"
	"github.com/colespringer/waxtap/v2/youtube/internal/resolver"
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
	GL string // content-region hint
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
	// A static session is already resolved, so seed its cookies and store its
	// visitorData now. New cannot return an error, so adoption failures are stored
	// in adoptErr and surfaced by Extract. An empty visitorData must fail because
	// it breaks GVS content_binding coherence and prevents learnVisitorData from
	// recovering after the source is marked as adopted.
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
		// Preserve provider failures so callers can classify their causes.
		return "", &waxerr.ProviderError{Endpoint: "session", Cause: err}
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
	return c.ExtractExcluding(ctx, videoID, nil)
}

// ExtractExcluding behaves like Extract but skips the specified attempts. It
// returns the highest-precedence error when all remaining attempts fail.
// Cancellation and rate limiting stop the chain immediately.
func (c *Client) ExtractExcluding(ctx context.Context, videoID string, skip map[AttemptID]bool) (*Extraction, error) {
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return nil, err // fatal only under adoption; otherwise newBootstrappedSession never errors
	}
	var bestErr error

	for i, profile := range c.profiles {
		if skip[profileAttempt(i)] {
			continue
		}
		// A failed profile must not carry its PO-token binding into the next
		// attempt. The winning attempt returns with its binding intact.
		sess.resetPOBinding()

		ext, perr := c.extractProfile(ctx, sess, profile, videoID, i)
		if perr == nil {
			msg := "extraction succeeded"
			if i > 0 {
				msg = "extraction succeeded via fallback client"
			}
			c.log.DebugContext(ctx, msg, "client", profile.Name, "attempt", i)
			return ext, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(perr, waxerr.ErrRateLimited) {
			return nil, perr // throttling won't differ across clients; surface it
		}
		bestErr = waxerr.PreferErr(bestErr, perr)
	}

	// The watch page is a separate fallback attempt and participates in error
	// precedence like the profile attempts.
	if !skip[AttemptWatchPage] && ctx.Err() == nil && !errors.Is(bestErr, waxerr.ErrRateLimited) {
		// The watch page runs as WEB. Record and report any substitution for a
		// forced non-WEB client.
		substituting := c.forcedNonWebSingle()
		if substituting {
			// Keep this at Debug. Public warnings such as warnClientSubstitution and
			// noteForcedIOSIncomplete already report the substitution; a Warn log here
			// would print a second, unformatted line in the CLI.
			c.log.DebugContext(ctx, "forced client failed; trying watch-page WEB fallback", "client", c.profiles[0].Name)
		}
		ext, ferr := c.extractFromWatchPage(ctx, videoID)
		if ferr == nil {
			if substituting {
				ext.substitutedFrom = c.profiles[0].Name
			}
			c.log.DebugContext(ctx, "extracted via watch-page fallback")
			return ext, nil
		}
		bestErr = waxerr.PreferErr(bestErr, ferr)
		if substituting {
			bestErr = fmt.Errorf("forced client %s failed and the WEB watch-page fallback also failed: %w", c.profiles[0].Name, bestErr)
		}
	}

	if bestErr == nil {
		// Every attempt was excluded.
		bestErr = waxerr.ErrChainExhausted
	}
	return nil, bestErr
}

// ExtractAttempt repeats one profile or watch-page attempt. Refresh and SABR
// reload paths use it to remain on the client that produced the original stream.
// Use ExtractWebContext to repeat the web-context attempt.
func (c *Client) ExtractAttempt(ctx context.Context, videoID string, a AttemptID) (*Extraction, error) {
	if a == AttemptWatchPage {
		return c.extractFromWatchPage(ctx, videoID)
	}
	i, ok := profileIndex(a)
	if !ok || i < 0 || i >= len(c.profiles) {
		return nil, fmt.Errorf("%w: cannot re-extract attempt %q", waxerr.ErrExtractionFailed, a)
	}
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return nil, err
	}
	sess.resetPOBinding()
	return c.extractProfile(ctx, sess, c.profiles[i], videoID, i)
}

// extractProfile performs one profile's /player extraction. The caller resets the
// session PO binding and decides whether to continue the profile chain.
func (c *Client) extractProfile(ctx context.Context, sess *session, profile ClientProfile, videoID string, i int) (*Extraction, error) {
	// WEB-family profiles need a player-scope PO token in the /player body before
	// YouTube returns usable stream URLs. Profiles that do not list ScopePlayer
	// skip the provider lookup.
	playerResp, err := c.fetchPOToken(ctx, profile, sess, videoID, potoken.ScopePlayer, nil)
	if err != nil {
		c.log.DebugContext(ctx, "player PO token unavailable; trying next attempt", "client", profile.Name, "err", err)
		return nil, err
	}
	var playerTok string
	if playerResp != nil {
		playerTok = playerResp.Token
	}

	// WEB-family clients require the base.js signature timestamp in the /player
	// body. If lookup fails, this attempt proceeds without it.
	sts := c.signatureTimestamp(ctx, profile, videoID)

	body, err := c.innertubePost(ctx, profile, sess, playerEndpoint, c.newPlayerRequest(profile, sess, playerRequestOpts{VideoID: videoID, POToken: playerTok, STS: sts}))
	if err != nil {
		c.log.DebugContext(ctx, "player request failed; trying next attempt", "client", profile.Name, "err", err)
		return nil, err
	}

	pr, err := parsePlayerResponse(body)
	if err != nil {
		c.dumpArtifact(ctx, "playerresponse-"+profile.Name+"-"+videoID+".json", body)
		c.log.DebugContext(ctx, "player-response parse failed; trying next attempt", "client", profile.Name, "err", err)
		return nil, &waxerr.ExtractionError{Stage: "player-response", Cause: err}
	}
	sess.learnVisitorData(pr.ResponseContext.VisitorData)

	if perr := pr.playabilityError(); perr != nil {
		// Playability failures can be client-specific: stale versions, sparse
		// context, and bot checks often report generic ERROR or UNPLAYABLE.
		result := perr
		switch {
		// Preserve an embed restriction before considering a missing signature
		// timestamp, which can fail during the same attempt.
		case isWebEmbedded(profile):
			if annotated := annotateEmbedError(perr); annotated != perr {
				result = annotated
			} else if profile.NeedsSignatureTimestamp && sts == 0 {
				result = missingTimestampError(perr)
			}
		// sts==0 here means the timestamp lookup failed, so the resulting
		// UNPLAYABLE is a maintenance problem, not an unavailable video. Reclassify
		// it so the terminal error names that cause instead of a generic
		// ErrVideoUnavailable.
		case profile.NeedsSignatureTimestamp && sts == 0:
			result = missingTimestampError(perr)
		}
		c.dumpArtifact(ctx, "playerresponse-"+profile.Name+"-"+videoID+".json", body)
		c.log.DebugContext(ctx, "playability failure; trying next attempt", "client", profile.Name, "err", result)
		return nil, result
	}

	video, raw, err := pr.toVideo(videoID)
	if err != nil {
		c.dumpArtifact(ctx, "playerresponse-"+profile.Name+"-"+videoID+".json", body)
		c.log.DebugContext(ctx, "no usable formats; trying next attempt", "client", profile.Name, "err", err)
		return nil, err
	}

	return buildExtraction(video, profile, sess, raw, pr, profileAttempt(i)), nil
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
	// A missing timestamp can make /player return UNPLAYABLE. Keep the lookup
	// details at debug level because the terminal error already reports the result.
	sts, err := c.inspector.SignatureTimestamp(ctx, resolver.Context{VideoID: videoID})
	if err != nil {
		c.log.DebugContext(ctx, "signature timestamp lookup failed; omitting field (expect UNPLAYABLE)", "client", profile.Name, "err", err)
		return 0
	}
	if sts == 0 {
		c.log.DebugContext(ctx, "signature timestamp resolved to zero; omitting field (expect UNPLAYABLE)", "client", profile.Name)
	}
	return sts
}

// isWebEmbedded reports whether profile is the WEB_EMBEDDED_PLAYER client.
func isWebEmbedded(p ClientProfile) bool {
	return p.InnerTubeName == profileWebEmbedded.InnerTubeName
}

// annotateEmbedError marks a generic web_embedded error so callers can provide
// fallback guidance without changing YouTube's reason or the error
// classification.
func annotateEmbedError(perr error) error {
	pe, ok := errors.AsType[*waxerr.PlayabilityError](perr)
	if !ok || pe.Status != "ERROR" {
		return perr
	}
	return &waxerr.PlayabilityError{Status: pe.Status, Reason: pe.Reason, Sentinel: pe.Sentinel, Embed: true}
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

// fetchWatchPage GETs the consent-bypassed watch page for videoID through the
// built-in WEB profile and a bootstrapped session. It is the shared fetch path
// for the watch-page extraction fallback and the WatchPageMetadata enrichment
// pass, so both go through the same rate-limit and cooldown machinery.
func (c *Client) fetchWatchPage(ctx context.Context, videoID string) ([]byte, *session, error) {
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return nil, nil, err
	}
	body, err := c.httpGet(ctx, c.webFallback, sess, "https://www.youtube.com/watch?v="+videoID+"&bpctr=9999999999&has_verified=1")
	if err != nil {
		return nil, nil, err
	}
	return body, sess, nil
}

// extractFromWatchPage fetches the watch page and parses the embedded
// ytInitialPlayerResponse, as a fallback when the InnerTube clients fail.
func (c *Client) extractFromWatchPage(ctx context.Context, videoID string) (*Extraction, error) {
	profile := c.webFallback
	body, sess, err := c.fetchWatchPage(ctx, videoID)
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
	// The watch page carries chapters (in ytInitialData) and the availability
	// microformat, so fill them now rather than re-fetching for WithFullMetadata.
	fillWatchPageEnrichment(video, body, pr)
	return buildExtraction(video, profile, sess, raw, pr, AttemptWatchPage), nil
}

// fillWatchPageEnrichment fills chapters and availability into v from
// already-fetched watch-page HTML and its parsed player response. The watch page
// is a WEB response, so its microformat determines availability; publishDate was
// already set by toVideo from the same microformat.
func fillWatchPageEnrichment(v *Video, body []byte, pr *playerResponse) {
	v.Chapters = parseChapters(body, v.Duration)
	v.Availability = AvailabilityFromUnlisted(pr.Microformat.PlayerMicroformatRenderer.IsUnlisted)
}

// WatchPageMeta is the metadata a watch-page fetch adds beyond a /player
// response: the publish date and unlisted state from the WEB microformat, and the
// chapters from ytInitialData.
type WatchPageMeta struct {
	PublishDate time.Time // zero when the page carried none
	Chapters    []Chapter // nil when the video has no chapters
	Unlisted    bool      // the video is unlisted (link-only)
}

// WatchPageMetadata fetches the watch page for videoID and returns the metadata
// only the WEB watch page carries: publish date, chapters, and unlisted state. It
// is the token-free enrichment behind waxtap.WithFullMetadata for extractions
// that did not already scrape the watch page.
//
// Context cancellation is returned as a fatal error. A fetch or parse failure is
// also returned, but callers treating enrichment as best-effort should keep the
// base metadata when the context was not canceled.
func (c *Client) WatchPageMetadata(ctx context.Context, videoID string) (WatchPageMeta, error) {
	// The session is not reused after this one-shot fetch, so its visitorData is not
	// learned back (unlike extractFromWatchPage, which resolves streams next).
	body, _, err := c.fetchWatchPage(ctx, videoID)
	if err != nil {
		return WatchPageMeta{}, err
	}
	pr, err := parseWatchPage(body)
	if err != nil {
		c.dumpArtifact(ctx, "watchpage-"+videoID+".html", body)
		return WatchPageMeta{}, &waxerr.ExtractionError{Stage: "watch-page-metadata", Cause: err}
	}
	return WatchPageMeta{
		PublishDate: parseDate(pr.Microformat.PlayerMicroformatRenderer.PublishDate),
		Chapters:    parseChapters(body, pr.duration()),
		Unlisted:    pr.Microformat.PlayerMicroformatRenderer.IsUnlisted,
	}, nil
}

// Info returns video metadata and candidate formats.
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

// EnumOptions configures Client.Enumerate. The facade builds one from its own
// EnumerateOptions plus any resolved channel ID.
type EnumOptions struct {
	// MaxItems caps the returned entries; <= 0 lists every entry.
	MaxItems int
	// OnPage reports the running entry count after each successfully appended page.
	OnPage func(count int)
	// Skip omits matching entries but keeps paging, for an archive cursor over an
	// arbitrary playlist. It is applied before the MaxItems cap so the cap counts
	// unseen entries, and skipped entries still advance the index so
	// PlaylistEntry.Index stays the true playlist position.
	Skip func(id string) bool
	// Stop halts pagination at the first matching entry (excluding it and every
	// entry after it) and leaves Playlist.Continuation empty. It is only correct on
	// an append-only newest-first feed (a channel uploads playlist); a curated PL
	// can insert entries anywhere, so Stop on one would drop items. Use Skip for the
	// general case. Stop is checked before Skip.
	Stop func(id string) bool
	// ChannelID, when set, is stamped onto every entry. A channel-feed enumeration
	// already knows the uploader, so no per-video Enrich is needed.
	ChannelID string
}

// Enumerate expands a playlist into lightweight entries without downloading. It
// pages through continuations until exhausted, MaxItems is reached, or Stop halts
// paging. Per-page failures after the first page are collected in Playlist.Errors
// rather than discarding the entries already gathered.
func (c *Client) Enumerate(ctx context.Context, playlistID string, o EnumOptions) (*Playlist, error) {
	// Reject negative caps instead of treating them as an unlimited request.
	if o.MaxItems < 0 {
		return nil, fmt.Errorf("%w: maxItems must be >= 0, got %d", waxerr.ErrInvalidConfig, o.MaxItems)
	}
	profile := c.playlistProfile()
	sess, err := c.newBootstrappedSession(ctx)
	if err != nil {
		return nil, err
	}
	pl := &Playlist{ID: playlistID}

	meta, items, token, err := c.browseInitial(ctx, profile, sess, playlistID)
	if err != nil {
		// Map YouTube's browse status to a user-facing playlist error. The raw error
		// carries the internal endpoint URL, so keep it on the debug log for
		// --verbose diagnosis and leave the CLI to render a clean message.
		if hse, ok := errors.AsType[*waxerr.HTTPStatusError](err); ok {
			c.log.DebugContext(ctx, "initial playlist browse failed", "playlist", playlistID, "status", hse.StatusCode, "err", err)
			switch hse.StatusCode {
			case http.StatusBadRequest:
				return nil, fmt.Errorf("%w: %v", waxerr.ErrInvalidPlaylistID, err)
			case http.StatusNotFound:
				// A deleted or nonexistent playlist. Private playlists return HTTP 200
				// with an in-body alert, and a 403 is an anti-bot or attestation block,
				// so neither case is folded into this status mapping.
				return nil, fmt.Errorf("%w: %v", waxerr.ErrPlaylistUnavailable, err)
			}
		}
		return nil, err
	}
	sess.learnVisitorData(meta.visitorData)
	pl.Title = meta.title
	pl.Author = meta.author

	// rawPos is the true playlist position, advanced for every video entry (kept or
	// skipped) so PlaylistEntry.Index survives Skip. It lives here, not in
	// appendPlaylistItems, so it persists across pages.
	rawPos := 0
	truncated, halted := c.appendPlaylistItems(pl, items, o, &rawPos)
	if o.OnPage != nil {
		o.OnPage(len(pl.Entries))
	}

	for token != "" && !halted && !reachedLimit(pl, o.MaxItems) {
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
		truncated, halted = c.appendPlaylistItems(pl, items, o, &rawPos)
		if o.OnPage != nil {
			o.OnPage(len(pl.Entries))
		}
	}

	// Continuation tokens are page-granular. Only expose one when the current
	// page was fully consumed at the MaxItems boundary: a mid-page cutoff
	// (truncated) has no precise resume point, and an early Stop (halted) has no
	// meaningful resume point at all.
	if reachedLimit(pl, o.MaxItems) && !truncated && !halted {
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
			// Shorts shelf responses use a known unsupported shape. Reclassify the
			// parse error so it is not reported or retried as an unrecognized shape.
			err = shortsOrParseError(playlistID, perr)
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

// shortsOrParseError maps parse failures for Shorts shelf playlists to
// ErrShortsPlaylist. Other playlist IDs and non-parse errors pass through.
func shortsOrParseError(playlistID string, perr error) error {
	if errors.Is(perr, waxerr.ErrPlaylistParse) && isShortsPlaylistID(playlistID) {
		return waxerr.ErrShortsPlaylist
	}
	return perr
}

// forcedNonWebSingle reports whether the chain contains one non-WEB client.
func (c *Client) forcedNonWebSingle() bool {
	return len(c.profiles) == 1 && c.profiles[0].InnerTubeName != profileWeb.InnerTubeName
}

// ForcedSingleClient returns the InnerTube name when exactly one client profile
// is configured. It also returns true for a single profile override.
func (c *Client) ForcedSingleClient() (string, bool) {
	if len(c.profiles) != 1 {
		return "", false
	}
	return c.profiles[0].InnerTubeName, true
}

// IsWebClient reports whether name is the built-in WEB InnerTube client name.
// It does not match WEB_EMBEDDED.
func IsWebClient(name string) bool { return name == profileWeb.InnerTubeName }

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

// appendPlaylistItems adds entries up to MaxItems, applying the Skip/Stop
// predicates and the channel stamp. rawPos advances for every video entry (kept
// or skipped) so PlaylistEntry.Index stays the true playlist position. It returns
// truncated when the MaxItems cap cut the page short (no precise resume point) and
// halted when Stop matched (pagination must stop with no resume point).
func (c *Client) appendPlaylistItems(pl *Playlist, items []playlistItem, o EnumOptions, rawPos *int) (truncated, halted bool) {
	for _, it := range items {
		if reachedLimit(pl, o.MaxItems) {
			return true, false
		}
		id := it.itemVideoID()
		if o.Stop != nil && id != "" && o.Stop(id) {
			return false, true
		}
		if o.Skip != nil && id != "" && o.Skip(id) {
			*rawPos++ // a skipped entry still consumes a playlist position
			continue
		}
		entry, err := it.toEntry(*rawPos)
		if err != nil {
			pl.Errors = append(pl.Errors, err) // partial enumeration is not fatal
			// A video item that failed to parse still occupied a playlist position,
			// so advance the index to keep later entries at their true position. A
			// non-video shape (id == "") occupies none.
			if id != "" {
				*rawPos++
			}
			continue
		}
		if o.ChannelID != "" {
			entry.ChannelID = o.ChannelID
		}
		pl.Entries = append(pl.Entries, entry)
		*rawPos++
	}
	return false, false
}

func reachedLimit(pl *Playlist, maxItems int) bool {
	return maxItems > 0 && len(pl.Entries) >= maxItems
}
