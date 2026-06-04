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

	"github.com/colespringer/waxtap/download"
	"github.com/colespringer/waxtap/format"
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
}

// New constructs a Client from Options, applying defaults for unset fields.
func New(opts Options) (*Client, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	var profiles []youtube.ClientProfile
	if opts.ProfileOverridePath != "" {
		var err error
		if profiles, err = loadProfileOverrides(opts.ProfileOverridePath); err != nil {
			return nil, err
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

	var limiter httpx.Limiter
	if opts.Politeness.PerHostQPS > 0 {
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
	})

	c := &Client{
		opts: opts,
		log:  log,
		http: hc,
		yt: youtube.New(youtube.Config{
			HTTP:             hc,
			Logger:           log,
			Profiles:         profiles, // nil => youtube.DefaultProfiles()
			ResolveTimeout:   opts.Timeouts.Resolve,
			CacheDir:         resolveCacheDir(opts, log),
			DisableDiskCache: opts.DisableDiskCache,
			POTokenProvider:  opts.POTokenProvider,
			HL:               opts.Locale.HL,
			GL:               opts.Locale.GL,
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
	rs, rerr := c.yt.Resolve(rctx, ext, idx)
	if rerr != nil {
		return nil, rerr
	}
	if rs.ContentLength > 0 {
		video.Formats[idx].ContentLength = rs.ContentLength
	}

	if depth >= InfoProbe {
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
// AudioSelector means best audio. The returned ResolvedStream contains a
// temporary googlevideo URL, its expiry, content length, and request headers.
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
	return c.yt.Resolve(rctx, ext, idx)
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
