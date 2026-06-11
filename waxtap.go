package waxtap

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/colespringer/waxtap/download"
	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/internal/clientident"
	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/sponsorblock"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/youtube"
)

// cacheDirName is the WaxTap subdirectory under the OS user cache directory. The
// CLI's cache subcommands resolve the same path so cache clean targets the
// directory the library actually writes.
const cacheDirName = "waxtap"

// Client is the main WaxTap entry point for library callers and the CLI. It is
// safe for concurrent use after construction.
//
// The same httpx.Client backs extraction, media download, and SponsorBlock. Its
// per-host limiter is shared by all request paths, while each host keeps its own
// schedule. The ffmpeg Runner is created lazily on first use, so metadata calls
// and keep-source downloads work without ffmpeg installed.
type Client struct {
	opts Options
	log  *slog.Logger
	http *httpx.Client

	yt *youtube.Client
	dl *download.Downloader
	sb *sponsorblock.Client

	runnerOnce sync.Once
	runner     *transcode.Runner
	runnerErr  error

	// WEB player-context provider cooldown (see acquire): after a provider
	// failure the attempt is skipped until webCtxDownUntil, so a dead sidecar
	// taxes a batch once per window instead of once per video.
	webCtxMu        sync.Mutex
	webCtxDownUntil time.Time
}

// New constructs a Client from Options, applying defaults for unset fields.
func New(opts Options) (*Client, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	// youtube.New cannot report invalid configuration, so validate the override
	// here.
	if !clientident.ValidChromeMajor(opts.ChromeMajor) {
		return nil, fmt.Errorf("invalid ChromeMajor %d: must be 0 (default) or 1..999", opts.ChromeMajor)
	}
	if opts.ChromeMajor != 0 && opts.ProfileOverridePath != "" {
		return nil, fmt.Errorf("ChromeMajor and ProfileOverridePath are mutually exclusive: an override file supplies its own user agents")
	}
	if opts.Client != "" && opts.ProfileOverridePath != "" {
		return nil, fmt.Errorf("Client and ProfileOverridePath are mutually exclusive: choose a single built-in client or supply an override file")
	}
	if opts.Politeness.Cooldown < 0 {
		return nil, fmt.Errorf("invalid Cooldown %s: must be >= 0", opts.Politeness.Cooldown)
	}
	// The WEB player-context path defers its GVS token mint to SABR setup, past
	// the acquire-time fallback, so a missing token provider would hard-fail
	// every download there instead of falling back. Reject it up front.
	if opts.PlayerContextProvider != nil && opts.POTokenProvider == nil {
		return nil, fmt.Errorf("PlayerContextProvider requires a POTokenProvider: the WEB stream binds a GVS PO token to the context's visitorData")
	}

	// Resolve the client strategy chain: a single forced Client, a file override,
	// or (nil) the built-in default chain.
	var profiles []youtube.ClientProfile
	switch {
	case opts.Client != "":
		var err error
		if profiles, err = youtube.BuildClientChain(opts.Client, opts.ChromeMajor); err != nil {
			return nil, err
		}
	case opts.ProfileOverridePath != "":
		var err error
		if profiles, err = loadProfileOverrides(opts.ProfileOverridePath); err != nil {
			return nil, err
		}
	}

	// External session adoption (Session/SessionProvider) requires a uniform,
	// explicitly-selected chain so the adopted identity is never routed through a
	// different client, and the two session sources are mutually exclusive.
	if opts.Session != nil && opts.SessionProvider != nil {
		return nil, fmt.Errorf("Session and SessionProvider are mutually exclusive: supply at most one external session source")
	}
	if opts.Session != nil || opts.SessionProvider != nil {
		if err := requireUniformChain(profiles); err != nil {
			return nil, err
		}
		if opts.Session != nil && opts.Session.VisitorData == "" {
			return nil, fmt.Errorf("adopted Session requires a non-empty VisitorData (the browser's exact X-Goog-Visitor-Id literal)")
		}
		if opts.Session != nil && len(opts.Session.Cookies) > 0 && !optsHasJar(opts) {
			return nil, fmt.Errorf("adopted session cookies require an HTTPClient with a cookie jar; pass one or supply visitorData only")
		}
	}

	base := opts.HTTPClient
	if base == nil {
		// The default client owns a cookie jar so the guest-session bootstrap's
		// visitorData and its cookies stay coherent across player requests.
		// Per-request context deadlines, not a client timeout, bound operations.
		jar, _ := cookiejar.New(nil)
		base = &http.Client{Jar: jar}
	}

	// Cooldown-only configurations still need a limiter.
	var limiter httpx.Limiter
	if opts.Politeness.PerHostQPS > 0 || opts.Politeness.Cooldown > 0 {
		limiter = httpx.NewHostLimiter(opts.Politeness.PerHostQPS)
	}

	hc := httpx.New(httpx.Config{
		HTTPClient:   base,
		Logger:       log,
		Limiter:      limiter,
		MaxRetries:   opts.Retry.MaxRetries,
		BaseBackoff:  opts.Retry.BaseBackoff,
		MaxBackoff:   opts.Retry.MaxBackoff,
		MaxRetryWait: opts.Retry.MaxRetryWait,
		Cooldown:     opts.Politeness.Cooldown,
	})

	c := &Client{
		opts: opts,
		log:  log,
		http: hc,
		yt: youtube.New(youtube.Config{
			HTTP:                  hc,
			Logger:                log,
			Profiles:              profiles, // nil => youtube.DefaultProfiles()
			ChromeMajor:           opts.ChromeMajor,
			ResolveTimeout:        opts.Timeouts.Resolve,
			CacheDir:              resolveCacheDir(opts, log),
			DisableDiskCache:      opts.DisableDiskCache,
			POTokenProvider:       opts.POTokenProvider,
			PlayerContextProvider: opts.PlayerContextProvider,
			WebContextTimeout:     opts.Timeouts.WebContext,
			Session:               opts.Session,
			SessionProvider:       opts.SessionProvider,
			HL:                    opts.Locale.HL,
			GL:                    opts.Locale.GL,
		}),
		dl: download.New(download.Config{
			HTTPClient:      hc,
			Logger:          log,
			Parallelism:     opts.Concurrency.Chunks,
			ChunkTimeout:    opts.Timeouts.ChunkRetry,
			MaxChunkRetries: opts.Retry.MaxRetries,
			BaseBackoff:     opts.Retry.BaseBackoff,
			MaxBackoff:      opts.Retry.MaxBackoff,
		}),
		sb: sponsorblock.New(sponsorblock.Config{
			HTTP:    hc,
			Logger:  log,
			BaseURL: opts.SponsorBlock.BaseURL,
		}),
	}
	return c, nil
}

// requireUniformChain rejects a chain that an adopted session cannot coherently
// drive. A single forced client passes; a multi-client chain (including the
// nil/default chain, which expands to several clients) is rejected so the adopted
// identity is never routed through a client it was not minted for.
func requireUniformChain(profiles []youtube.ClientProfile) error {
	if len(profiles) == 0 {
		return fmt.Errorf("session adoption requires a uniform client chain: set Options.Client (e.g. \"web\") or a single-client ProfileOverridePath; the default multi-client chain is rejected")
	}
	first := profiles[0].InnerTubeName
	for _, p := range profiles[1:] {
		if p.InnerTubeName != first {
			return fmt.Errorf("session adoption requires a uniform client chain, but it mixes %q and %q clients", first, p.InnerTubeName)
		}
	}
	return nil
}

// optsHasJar reports whether the effective HTTP client will carry a cookie jar. A
// nil HTTPClient means WaxTap installs its own jar-backed default.
func optsHasJar(opts Options) bool {
	return opts.HTTPClient == nil || opts.HTTPClient.Jar != nil
}

// resolveCacheDir returns the base directory for WaxTap's on-disk caches, or ""
// when disk caching is disabled or no location can be determined. An empty
// CacheDir defaults to os.UserCacheDir()/waxtap, matching the CLI's cache
// subcommands. A failure to locate the user cache dir degrades to memory-only
// caching rather than failing construction.
func resolveCacheDir(opts Options, log *slog.Logger) string {
	if opts.DisableDiskCache {
		return ""
	}
	if opts.CacheDir != "" {
		return opts.CacheDir
	}
	base, err := os.UserCacheDir()
	if err != nil {
		log.Debug("disk cache disabled: cannot locate user cache dir", "err", err)
		return ""
	}
	return filepath.Join(base, cacheDirName)
}

// ffmpeg returns the shared transcode runner, creating it on first use. Metadata
// calls and source-only downloads do not require ffmpeg on PATH.
func (c *Client) ffmpeg() (*transcode.Runner, error) {
	c.runnerOnce.Do(func() {
		// Options.Concurrency.FFmpeg uses zero for the default limit, while
		// RunnerConfig uses zero for unlimited. Translate that once at the facade
		// boundary.
		procs := c.opts.Concurrency.FFmpeg
		switch {
		case procs == 0:
			procs = runtime.GOMAXPROCS(0)
		case procs < 0:
			procs = 0 // RunnerConfig treats 0 as unlimited
		}
		c.runner, c.runnerErr = transcode.NewRunner(transcode.RunnerConfig{
			MaxProcs:      procs,
			ShutdownGrace: c.opts.Timeouts.FFmpegShutdown,
			Logger:        c.log,
		})
	})
	return c.runner, c.runnerErr
}

// Info returns video metadata and candidate audio formats at the requested depth,
// without downloading.
//
// InfoBasic returns extracted metadata and candidate formats. InfoResolved
// additionally resolves the best-audio format, surfacing resolution errors (such
// as ErrNeedsPOToken) and filling in its content length. InfoProbe additionally
// runs ffprobe on that resolved stream and fills its authoritative sample rate,
// channel count, bitrate, and duration (network-expensive, and requires ffmpeg).
// The signed stream URLs themselves are not returned through Video; use Download
// or Stream to fetch bytes.
func (c *Client) Info(ctx context.Context, url string, depth InfoDepth) (*Video, error) {
	id, err := youtube.ExtractVideoID(url)
	if err != nil {
		return nil, err
	}

	ectx, ecancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	defer ecancel()
	ext, err := c.yt.Extract(ectx, id)
	if err != nil {
		return nil, err
	}
	video := ext.Video()
	if depth < InfoResolved {
		return video, nil
	}

	idx, serr := format.BestForTarget(video.Formats, format.MinimizeLoss(), format.Target{})
	if serr != nil {
		return video, nil // nothing resolvable; return the basic metadata
	}
	rctx, rcancel := withTimeout(ctx, c.opts.Timeouts.Resolve)
	defer rcancel()
	plan, rerr := c.yt.Resolve(rctx, ext, idx)
	if rerr != nil {
		return nil, rerr
	}
	rs := plan.Diagnostic()
	if rs.ContentLength > 0 {
		video.Formats[idx].ContentLength = rs.ContentLength
	}

	// ffprobe cannot inspect SABR streams because they have no direct URL.
	if depth >= InfoProbe && rs.Probeable() {
		runner, ferr := c.ffmpeg()
		if ferr != nil {
			return nil, ferr
		}
		probe, perr := runner.ProbeURL(ctx, rs.URL, rs.Headers)
		if perr != nil {
			return nil, perr
		}
		applyProbe(&video.Formats[idx], probe)
	}
	return video, nil
}

// Enumerate expands a playlist URL into entries without downloading media.
// EnumerateOptions.MaxItems caps the listing. With Enrich set, entries are
// refreshed with bounded-parallel InfoBasic calls; item-level failures are kept
// on Playlist.Errors.
func (c *Client) Enumerate(ctx context.Context, url string, opts EnumerateOptions) (*Playlist, error) {
	id, err := youtube.ExtractPlaylistID(url)
	if err != nil {
		return nil, err
	}
	pl, err := c.yt.Enumerate(ctx, id, opts.MaxItems)
	if err != nil {
		return nil, err
	}
	if opts.Enrich {
		if err := c.enrichEntries(ctx, pl); err != nil {
			return pl, err
		}
	}
	return pl, nil
}

// enrichEntries refreshes playlist entries with InfoBasic. Each worker owns one
// entry; only pl.Errors is shared. Ordinary item failures stay on the playlist,
// but context cancellation is returned to the caller.
func (c *Client) enrichEntries(ctx context.Context, pl *Playlist) error {
	limit := c.opts.Concurrency.Downloads
	if limit <= 0 {
		limit = 4
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range pl.Entries {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			v, err := c.Info(ctx, pl.Entries[i].VideoID, InfoBasic)
			if err != nil {
				// Return cancellation through ctx.Err(), not as an item error.
				if ctx.Err() == nil {
					mu.Lock()
					pl.Errors = append(pl.Errors, fmt.Errorf("enrich %s: %w", pl.Entries[i].VideoID, err))
					mu.Unlock()
				}
				return
			}
			pl.Entries[i].Title = v.Title
			pl.Entries[i].Author = v.Author
			pl.Entries[i].Duration = v.Duration
		}(i)
	}
	wg.Wait()
	return ctx.Err()
}

// Resolve selects and resolves an audio stream without downloading it. The zero
// AudioSelector means best audio. Direct streams include a temporary googlevideo
// URL and its request metadata. SABR streams set IsSABR and leave URL empty.
//
// It is exposed for diagnostics: the CLI's info --show-urls and doctor. Most
// callers use Download or Stream, which never expose the raw URL.
func (c *Client) Resolve(ctx context.Context, url string, sel AudioSelector) (ResolvedStream, error) {
	id, err := youtube.ExtractVideoID(url)
	if err != nil {
		return ResolvedStream{}, err
	}
	ectx, ecancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	defer ecancel()
	ext, err := c.yt.Extract(ectx, id)
	if err != nil {
		return ResolvedStream{}, err
	}
	idx, err := selectIndex(sel, MinimizeLoss(), format.Target{}, ext.Video().Formats)
	if err != nil {
		return ResolvedStream{}, err
	}
	rctx, rcancel := withTimeout(ctx, c.opts.Timeouts.Resolve)
	defer rcancel()
	plan, err := c.yt.Resolve(rctx, ext, idx)
	if err != nil {
		return ResolvedStream{}, err
	}
	return plan.Diagnostic(), nil
}

// InfoDepth selects how much work Info does. Callers do not pay for what they do
// not request.
type InfoDepth uint8

const (
	// InfoBasic returns metadata and candidate formats (the default).
	InfoBasic InfoDepth = iota
	// InfoResolved additionally resolves the best-audio stream URL and expiry.
	// These signed googlevideo URLs are temporary and sensitive; the CLI omits
	// them from human output unless --show-urls is given.
	InfoResolved
	// InfoProbe additionally runs ffprobe on the selected format only. This is
	// network-expensive (it reads the remote signed URL) and is never run on
	// every candidate.
	InfoProbe
)
