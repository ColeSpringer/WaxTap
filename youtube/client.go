package youtube

import (
	"context"
	"errors"
	"log/slog"

	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube/internal/resolver"
)

// Client performs YouTube extraction. It holds configuration and injected
// dependencies, while mutable per-attempt identity lives in session. It is safe
// for concurrent use after construction.
type Client struct {
	http     *httpx.Client
	log      *slog.Logger
	profiles []ClientProfile
	resolver resolver.Resolver // stream-URL resolution; nil until configured
	hl, gl   string            // InnerTube host language / content region
}

// Config configures a Client.
type Config struct {
	// HTTP is the retrying HTTP client. If nil, a default httpx.Client is used.
	HTTP *httpx.Client
	// Logger receives debug logs. If nil, logging is discarded.
	Logger *slog.Logger
	// Profiles overrides the client strategy chain. If empty, DefaultProfiles().
	Profiles []ClientProfile
	// Resolver resolves candidate formats into playable URLs. Metadata extraction
	// does not need one; stream resolution does.
	Resolver resolver.Resolver
	// HL (host language, e.g. "en") and GL (content region, e.g. "US") set the
	// InnerTube locale. Empty defaults to en / US. These are localization hints:
	// geo-restricted availability is still governed by the request IP.
	HL string
	GL string
}

// New returns a Client, applying defaults for unset Config fields.
func New(cfg Config) *Client {
	c := &Client{
		http:     cfg.HTTP,
		log:      cfg.Logger,
		profiles: cfg.Profiles,
		resolver: cfg.Resolver,
		hl:       cfg.HL,
		gl:       cfg.GL,
	}
	if c.http == nil {
		c.http = httpx.New(httpx.Config{})
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if len(c.profiles) == 0 {
		c.profiles = DefaultProfiles()
	}
	if c.hl == "" {
		c.hl = "en"
	}
	if c.gl == "" {
		c.gl = "US"
	}
	return c
}

// Extract fetches metadata and candidate audio formats for videoID, trying the
// client strategy chain in order until one succeeds. Each attempt uses an
// immutable profile and a shared per-extraction session; the winning profile and
// session are carried in the returned Extraction so stream resolution uses the
// same identity.
func (c *Client) Extract(ctx context.Context, videoID string) (*Extraction, error) {
	sess := newSession()
	var lastErr error

	for i, profile := range c.profiles {
		body, err := c.innertubePost(ctx, profile, sess, playerEndpoint, c.newPlayerRequest(profile, sess, videoID))
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
			continue
		}
		sess.learnVisitorData(pr.ResponseContext.VisitorData)

		if perr := pr.playabilityError(); perr != nil {
			lastErr = perr
			if errors.Is(perr, waxerr.ErrLoginRequired) {
				// Age/login gate: a different client (e.g. embedded) may succeed.
				c.log.DebugContext(ctx, "login/age gate; trying next client", "client", profile.Name)
				continue
			}
			return nil, perr // private/unavailable/live are terminal
		}

		video, raw, err := pr.toVideo(videoID)
		if err != nil {
			lastErr = err
			c.log.DebugContext(ctx, "no usable formats; trying next client", "client", profile.Name, "err", err)
			continue
		}

		if i > 0 {
			c.log.DebugContext(ctx, "extracted via fallback client", "client", profile.Name)
		}
		return &Extraction{video: video, profile: profile, session: sess, rawAudio: raw}, nil
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

// extractFromWatchPage fetches the watch page and parses the embedded
// ytInitialPlayerResponse, as a fallback when the InnerTube clients fail.
func (c *Client) extractFromWatchPage(ctx context.Context, videoID string) (*Extraction, error) {
	profile := webProfile()
	sess := newSession()

	body, err := c.httpGet(ctx, profile, sess, "https://www.youtube.com/watch?v="+videoID+"&bpctr=9999999999&has_verified=1")
	if err != nil {
		return nil, err
	}
	pr, err := parseWatchPage(body)
	if err != nil {
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
	return &Extraction{video: video, profile: profile, session: sess, rawAudio: raw}, nil
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
	sess := newSession()
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
	return webProfile()
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
