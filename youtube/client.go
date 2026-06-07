package youtube

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
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
	webFallback ClientProfile            // built-in WEB profile for ancillary requests
	resolver    resolver.Resolver        // stream-URL resolution
	inspector   resolver.PlayerInspector // signature timestamp lookup; nil if unsupported
	potoken     potoken.Provider         // PO-token provider, when configured
	visitors    *cache.Store[string]     // bootstrapped guest visitorData, singleflighted
	hl, gl      string                   // InnerTube host language / content region
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
		http:     cfg.HTTP,
		log:      cfg.Logger,
		profiles: cfg.Profiles,
		resolver: cfg.Resolver,
		potoken:  cfg.POTokenProvider,
		hl:       cfg.HL,
		gl:       cfg.GL,
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
	return c
}

// Extract fetches metadata and candidate audio formats for videoID, trying the
// client strategy chain in order until one succeeds. Each attempt uses an
// immutable profile and a shared per-extraction session; the winning profile and
// session are carried in the returned Extraction so stream resolution uses the
// same identity.
func (c *Client) Extract(ctx context.Context, videoID string) (*Extraction, error) {
	sess := c.newBootstrappedSession(ctx)
	var lastErr error

	for i, profile := range c.profiles {
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
			c.log.DebugContext(ctx, "playability failure; trying next client", "client", profile.Name, "err", perr)
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
		return &Extraction{video: video, profile: profile, session: sess, rawAudio: raw, expiresAt: pr.expiresAt(time.Now())}, nil
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
// cannot inspect players, or lookup fails. Player discovery starts from videoID.
func (c *Client) signatureTimestamp(ctx context.Context, profile ClientProfile, videoID string) int {
	if !profile.NeedsSignatureTimestamp || c.inspector == nil {
		return 0
	}
	sts, err := c.inspector.SignatureTimestamp(ctx, resolver.Context{VideoID: videoID})
	if err != nil {
		c.log.DebugContext(ctx, "signature timestamp lookup failed; omitting field", "client", profile.Name, "err", err)
		return 0
	}
	return sts
}

// extractFromWatchPage fetches the watch page and parses the embedded
// ytInitialPlayerResponse, as a fallback when the InnerTube clients fail.
func (c *Client) extractFromWatchPage(ctx context.Context, videoID string) (*Extraction, error) {
	profile := c.webFallback
	sess := c.newBootstrappedSession(ctx)

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
	return &Extraction{video: video, profile: profile, session: sess, rawAudio: raw, expiresAt: pr.expiresAt(time.Now())}, nil
}

// Info returns just the video metadata and candidate formats.
func (c *Client) Info(ctx context.Context, videoID string) (*Video, error) {
	ext, err := c.Extract(ctx, videoID)
	if err != nil {
		return nil, err
	}
	return ext.video, nil
}

// Enumerate expands a playlist into lightweight entries without downloading. It
// pages through continuations until exhausted or maxItems (<= 0 means all) is
// reached. Per-page failures after the first page are collected in Playlist.Errors
// rather than discarding the entries already gathered.
func (c *Client) Enumerate(ctx context.Context, playlistID string, maxItems int) (*Playlist, error) {
	profile := c.playlistProfile()
	sess := c.newBootstrappedSession(ctx)
	pl := &Playlist{ID: playlistID}

	body, err := c.innertubePost(ctx, profile, sess, browseEndpoint, c.newPlaylistRequest(profile, sess, playlistID, ""))
	if err != nil {
		return nil, err
	}
	meta, items, token, err := parseBrowseInitial(body)
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
