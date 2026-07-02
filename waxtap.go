package waxtap

import (
	"context"
	"fmt"
	"log/slog"
	"math"
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
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube"
)

// configErr wraps a configuration message with ErrInvalidConfig.
func configErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{waxerr.ErrInvalidConfig}, args...)...)
}

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
	// failure, the attempt is skipped until webCtxDownUntil.
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
		return nil, configErr("invalid ChromeMajor %d: must be 0 (default) or 1..999", opts.ChromeMajor)
	}
	if opts.ChromeMajor != 0 && opts.ProfileOverridePath != "" {
		return nil, configErr("ChromeMajor and ProfileOverridePath are mutually exclusive: an override file supplies its own user agents")
	}
	if opts.Client != "" && opts.ProfileOverridePath != "" {
		return nil, configErr("Client and ProfileOverridePath are mutually exclusive: choose a single built-in client or supply an override file")
	}
	if opts.Politeness.Cooldown < 0 {
		return nil, configErr("invalid Cooldown %s: must be >= 0", opts.Politeness.Cooldown)
	}
	if q := opts.Politeness.PerHostQPS; math.IsNaN(q) || math.IsInf(q, 0) || q < 0 {
		return nil, configErr("invalid PerHostQPS %v: must be a finite value >= 0", q)
	}
	// The WEB player-context path mints its GVS token during SABR setup. Reject a
	// missing provider during construction instead of failing each download.
	if opts.PlayerContextProvider != nil && opts.POTokenProvider == nil {
		return nil, configErr("PlayerContextProvider requires a POTokenProvider: the WEB stream binds a GVS PO token to the context's visitorData")
	}
	// A malformed SponsorBlock BaseURL would otherwise reach the HTTP client and
	// surface as an unclassified error at fetch time; reject it here so every path
	// (sponsorblock, cut, download) fails as invalid config at construction. Normalize
	// first with the same helper the client applies, then validate that value, so what
	// is validated is exactly what the client will fetch.
	if opts.SponsorBlock.BaseURL != "" {
		opts.SponsorBlock.BaseURL = sponsorblock.NormalizeBaseURL(opts.SponsorBlock.BaseURL)
		if _, err := validateHTTPBaseURL(opts.SponsorBlock.BaseURL); err != nil {
			return nil, configErr("invalid SponsorBlock BaseURL %q: %v", opts.SponsorBlock.BaseURL, err)
		}
	}

	// Resolve the client strategy chain: a single forced Client, a file override,
	// or (nil) the built-in default chain.
	var profiles []youtube.ClientProfile
	switch {
	case opts.Client != "":
		var err error
		if profiles, err = youtube.BuildClientChain(opts.Client, opts.ChromeMajor); err != nil {
			// Preserve the parser detail while classifying the option for callers.
			return nil, configErr("%v", err)
		}
	case opts.ProfileOverridePath != "":
		var err error
		if profiles, err = loadProfileOverrides(opts.ProfileOverridePath); err != nil {
			// Profile override failures are invalid configuration, even when a
			// file read caused the failure.
			return nil, configErr("%v", err)
		}
	}

	// External session adoption (Session/SessionProvider) requires a uniform,
	// explicitly-selected chain so the adopted identity is never routed through a
	// different client, and the two session sources are mutually exclusive.
	if opts.Session != nil && opts.SessionProvider != nil {
		return nil, configErr("Session and SessionProvider are mutually exclusive: supply at most one external session source")
	}
	if opts.Session != nil || opts.SessionProvider != nil {
		if err := requireUniformChain(profiles); err != nil {
			return nil, err
		}
		if opts.Session != nil && opts.Session.VisitorData == "" {
			return nil, configErr("adopted Session requires a non-empty VisitorData (the browser's exact X-Goog-Visitor-Id literal)")
		}
		if opts.Session != nil && len(opts.Session.Cookies) > 0 && !optsHasJar(opts) {
			return nil, configErr("adopted session cookies require an HTTPClient with a cookie jar; pass one or supply visitorData only")
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
		return configErr("session adoption requires a uniform client chain: set Options.Client (e.g. \"web\") or a single-client ProfileOverridePath; the default multi-client chain is rejected")
	}
	first := profiles[0].InnerTubeName
	for _, p := range profiles[1:] {
		if p.InnerTubeName != first {
			return configErr("session adoption requires a uniform client chain, but it mixes %q and %q clients", first, p.InnerTubeName)
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

// InfoResult contains extracted video metadata and the client that produced it.
// A later Resolve call may use a different client.
type InfoResult struct {
	Video  *Video // extracted metadata and candidate formats
	Client string // YouTube client that produced the metadata
	// SubstitutedFrom names a forced non-WEB client, such as WEB_EMBEDDED, that
	// the watch-page fallback replaced. When set, the metadata came from WEB
	// rather than the requested client.
	SubstitutedFrom string
	// ViaWatchPage reports that the primary metadata came from the watch-page
	// fallback. For a forced WEB client this read needs no PO token, unlike a forced
	// WEB stream; SubstitutedFrom stays empty because WEB is not substituted for
	// itself.
	ViaWatchPage bool
	// FullMetadata reports that watch-page enrichment (PublishDate, Chapters, and
	// Availability) was populated, either by the opt-in WithFullMetadata pass or
	// because the primary extraction already scraped the watch page (ViaWatchPage).
	// This is a different axis from ViaWatchPage, which reports where the base
	// metadata came from. When false, an empty Chapters slice or Unknown
	// Availability means enrichment did not run, not that the video has none.
	FullMetadata bool
	// Probed reports that InfoProbe ran ffprobe on the resolved best-audio stream,
	// so that row's sample rate, channels, bitrate, and duration are authoritative.
	// It is false for SABR streams, which have no direct URL to probe.
	Probed bool
	// BestIndex is the index into Video.Formats that InfoResolved/InfoProbe resolved
	// (and, for InfoProbe, probed in place). It is -1 at InfoBasic depth or when no
	// audio could be selected. A probe mutates that row, so callers should display
	// BestIndex rather than re-running selection on the mutated slice.
	BestIndex int
}

// ReadOption configures Info, InfoResult, and Resolve. WithNoFallback applies to
// all three; WithChannels only affects the best-audio row Info and InfoResult
// pick (Resolve takes an explicit AudioSelector and ignores it).
type ReadOption func(*readOptions)

type readOptions struct {
	noFallback   bool
	fullMetadata bool
	layout       ChannelLayout
}

// WithNoFallback prevents Info, InfoResult, and Resolve from falling back to
// watch-page extraction. Request.NoFallback provides the same behavior for
// Download and Stream.
func WithNoFallback() ReadOption {
	return func(o *readOptions) { o.noFallback = true }
}

// WithChannels sets the channel preference Info and InfoResult use to pick the
// best-audio row they resolve and probe, matching the row a default download
// would select. The facade defaults to stereo; pass WithChannels(LayoutSurround)
// for surround or WithChannels(LayoutAny) to rank purely by fidelity with no
// channel preference (a surround track may then rank highest). Resolve takes an
// explicit AudioSelector and ignores this option.
func WithChannels(layout ChannelLayout) ReadOption {
	return func(o *readOptions) { o.layout = layout }
}

// WithFullMetadata makes Info and InfoResult run a token-free watch-page pass
// that backfills PublishDate (when the primary client omitted it), Chapters, and
// Availability. The default ANDROID_VR client omits these, so this is what makes
// them reliable across clients. InfoResult.FullMetadata reports whether the data
// was populated.
//
// It costs one extra HTTP request unless the primary extraction already scraped
// the watch page (InfoResult.ViaWatchPage), in which case the data is already
// present and no extra fetch runs. Enrichment is best-effort: a parse failure
// leaves the fields zero/Unknown rather than failing the call. WithFullMetadata
// is a no-op when combined with WithNoFallback, which forbids the watch page.
func WithFullMetadata() ReadOption {
	return func(o *readOptions) { o.fullMetadata = true }
}

// defaultFacadeLayout is the channel layout the Download, Info, and Resolve facades
// impose when the caller expresses no preference, keeping the three selection seams
// identical. Callers opt out with WithChannels(LayoutAny). See
// TestFacadeDefaultsToStereo.
const defaultFacadeLayout = LayoutStereo

func newReadOptions(opts []ReadOption) readOptions {
	// Info/InfoResult default the resolved+probed best-audio row to the facade layout,
	// matching Download; WithChannels(LayoutAny) overrides it. Resolve also calls this
	// but ignores ro.layout (it selects from its explicit AudioSelector).
	ro := readOptions{layout: defaultFacadeLayout}
	for _, opt := range opts {
		opt(&ro)
	}
	return ro
}

// Info returns video metadata and candidate audio formats at the requested depth,
// without downloading.
func (c *Client) Info(ctx context.Context, url string, depth InfoDepth, opts ...ReadOption) (*Video, error) {
	r, err := c.InfoResult(ctx, url, depth, opts...)
	if err != nil {
		return nil, err
	}
	return r.Video, nil
}

// InfoResult returns video metadata, candidate audio formats, and the extraction
// client at the requested depth, without downloading.
//
// InfoBasic returns extracted metadata and candidate formats. InfoResolved
// additionally resolves the best-audio format, surfacing resolution errors (such
// as ErrNeedsPOToken) and filling in its content length. InfoProbe additionally
// runs ffprobe on that resolved stream and fills its authoritative sample rate,
// channel count, bitrate, and duration (network-expensive, and requires ffmpeg).
// The signed stream URLs themselves are not returned through Video; use Download
// or Stream to fetch bytes.
func (c *Client) InfoResult(ctx context.Context, url string, depth InfoDepth, opts ...ReadOption) (*InfoResult, error) {
	id, err := youtube.ExtractVideoID(url)
	if err != nil {
		return nil, err
	}
	ro := newReadOptions(opts)

	ectx, ecancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	defer ecancel()
	ext, err := c.yt.ExtractExcluding(ectx, id, watchPageSkip(ro.noFallback))
	if err != nil {
		return nil, err
	}
	video := ext.Video()
	res := &InfoResult{Video: video, Client: ext.ClientName(), SubstitutedFrom: ext.SubstitutedFrom(), ViaWatchPage: ext.Attempt() == youtube.AttemptWatchPage, BestIndex: -1}
	if res.ViaWatchPage {
		// The primary extraction already scraped the watch page, so chapters,
		// availability, and publish date are already on the Video.
		res.FullMetadata = true
	}
	// The watch-page enrichment pass fills PublishDate/Chapters/Availability. It is
	// metadata, independent of depth, so it runs before the InfoResolved gate. It is
	// a no-op with NoFallback (which forbids the watch page) or when the data is
	// already present.
	if ro.fullMetadata && !ro.noFallback && !res.FullMetadata {
		if err := c.fullMetadataPass(ctx, res, id); err != nil {
			return nil, err
		}
	}
	if depth < InfoResolved {
		return res, nil
	}

	// Resolve and probe the row the CLI displays as "Best audio" (the same selector,
	// with the caller's channel preference), so the content length and probed
	// numbers land on the displayed row rather than a surround track that outranks
	// it under MinimizeLoss with no preference.
	idx, serr := selectIndex(BestAudio().WithChannels(ro.layout), MinimizeLoss(), format.Target{}, video.Formats)
	if serr != nil {
		return res, nil // nothing resolvable; return the basic metadata
	}
	res.BestIndex = idx
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
		res.Probed = true
	}
	return res, nil
}

// fullMetadataPass runs the token-free watch-page metadata fetch and merges
// PublishDate (only if the primary client left it zero), Chapters, and
// Availability onto the extracted Video. Caller cancellation is fatal; any other
// failure is best-effort and leaves the base metadata with FullMetadata false.
func (c *Client) fullMetadataPass(ctx context.Context, res *InfoResult, id string) error {
	meta, err := c.watchPageMeta(ctx, id)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.DebugContext(ctx, "full-metadata watch-page pass failed; keeping base metadata", "err", err)
		return nil
	}
	mergeWatchPageMeta(res.Video, meta)
	res.FullMetadata = true
	return nil
}

// Enumerate expands a playlist or channel URL into entries without downloading
// media. A channel reference (a bare UC ID, or a /channel/, /@handle, /c/, or
// /user/ URL, with any trailing tab stripped) resolves to the channel's uploads
// feed, which is newest-first and lists Shorts and past live streams as ordinary
// entries. EnumerateOptions.MaxItems caps the listing, and Skip/Stop drive an
// archive cursor. With Enrich set, InfoBasic calls refresh entries at bounded
// concurrency. Successful calls update their entries; item-level failures are
// added to Playlist.Errors.
func (c *Client) Enumerate(ctx context.Context, url string, opts EnumerateOptions) (*Playlist, error) {
	if opts.MaxItems < 0 {
		return nil, configErr("Enumerate: MaxItems must be >= 0, got %d", opts.MaxItems)
	}
	id, channelID, err := c.resolveEnumerateTarget(ctx, url)
	if err != nil {
		return nil, err
	}
	pl, err := c.yt.Enumerate(ctx, id, youtube.EnumOptions{
		MaxItems:  opts.MaxItems,
		OnPage:    opts.OnProgress,
		Skip:      opts.Skip,
		Stop:      opts.Stop,
		ChannelID: channelID,
	})
	if err != nil {
		return nil, err
	}
	if opts.Enrich {
		if err := c.enrichEntries(ctx, pl, opts.OnEnrichProgress); err != nil {
			return pl, err
		}
	}
	return pl, nil
}

// resolveEnumerateTarget maps an Enumerate URL to a playlist ID plus, for a
// channel reference, the resolved UC ID that stamps every entry's ChannelID. An
// explicit playlist (a bare PL ID or a list= parameter, even on a channel URL)
// names a specific playlist and takes precedence over the channel's uploads feed;
// otherwise a channel URL resolves to that feed.
func (c *Client) resolveEnumerateTarget(ctx context.Context, url string) (playlistID, channelID string, err error) {
	id, plErr := youtube.ExtractPlaylistID(url)
	if plErr == nil {
		return id, "", nil
	}
	if ref, cerr := youtube.ExtractChannelRef(url); cerr == nil {
		return c.yt.ResolveUploadsPlaylist(ctx, ref)
	}
	return "", "", plErr
}

// enrichEntries refreshes playlist entries with InfoBasic. Each worker owns one
// entry; only pl.Errors is shared. Ordinary item failures stay on the playlist,
// but context cancellation is returned to the caller.
//
// onProgress reports each completed entry, successful or failed. Calls are
// serialized under a dedicated progress lock and arrive in increasing done-count
// order. The final call reaches (total, total) unless context cancellation stops
// enrichment early.
func (c *Client) enrichEntries(ctx context.Context, pl *Playlist, onProgress func(done, total int)) error {
	limit := c.opts.Concurrency.Downloads
	if limit <= 0 {
		limit = 4
	}
	total := len(pl.Entries)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var progressMu sync.Mutex
	progressDone := 0
	for i := range pl.Entries {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			// The semaphore release is registered after this defer, so it runs first.
			// A slow progress callback cannot occupy a worker slot, and progressMu
			// stays separate from the playlist error lock.
			defer func() {
				if onProgress != nil {
					progressMu.Lock()
					progressDone++
					onProgress(progressDone, total)
					progressMu.Unlock()
				}
			}()
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
			// Fill the channel ID when enumerate time did not carry a byline browseId
			// (a mixed-channel playlist), leaving a channel-feed stamp intact.
			if pl.Entries[i].ChannelID == "" {
				pl.Entries[i].ChannelID = v.ChannelID
			}
		}(i)
	}
	wg.Wait()
	return ctx.Err()
}

// Resolve selects and resolves an audio stream without downloading it. The zero
// AudioSelector means best audio, defaulting to stereo like Download; pass
// BestAudio().WithChannels(LayoutSurround) for surround or WithChannels(LayoutAny)
// for any-fidelity. Direct streams include a temporary googlevideo URL and its
// request metadata. SABR streams set IsSABR and leave URL empty.
//
// It is exposed for diagnostics: the CLI's info --show-url and doctor. Most
// callers use Download or Stream, which never expose the raw URL.
func (c *Client) Resolve(ctx context.Context, url string, sel AudioSelector, opts ...ReadOption) (ResolvedStream, error) {
	id, err := youtube.ExtractVideoID(url)
	if err != nil {
		return ResolvedStream{}, err
	}
	ro := newReadOptions(opts)
	ectx, ecancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	defer ecancel()
	ext, err := c.yt.ExtractExcluding(ectx, id, watchPageSkip(ro.noFallback))
	if err != nil {
		return ResolvedStream{}, err
	}
	idx, err := selectIndex(sel.WithDefaultChannels(defaultFacadeLayout), MinimizeLoss(), format.Target{}, ext.Video().Formats)
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
	// them from human output unless --show-url is given.
	InfoResolved
	// InfoProbe additionally runs ffprobe on the selected format only. This is
	// network-expensive (it reads the remote signed URL) and is never run on
	// every candidate.
	InfoProbe
)
