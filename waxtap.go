package waxtap

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"runtime"
	"sync"

	"github.com/colespringer/waxtap/download"
	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/sponsorblock"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/youtube"
)

// Client is the stable WaxTap entry point used by library callers and the CLI.
// It is safe for concurrent use after construction.
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
			HTTP:            hc,
			Logger:          log,
			ResolveTimeout:  opts.Timeouts.Resolve,
			POTokenProvider: opts.POTokenProvider,
			HL:              opts.Locale.HL,
			GL:              opts.Locale.GL,
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

// ffmpeg returns the shared transcode Runner, creating it on first use. Keeping
// this lazy lets metadata calls and source-only downloads run without ffmpeg
// installed.
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

// Enumerate expands a playlist URL into lightweight entries without downloading.
// EnumerateOptions.MaxItems caps the result. Enrichment of per-entry metadata is
// not performed; entries carry the lightweight fields the listing provides.
func (c *Client) Enumerate(ctx context.Context, url string, opts EnumerateOptions) (*Playlist, error) {
	id, err := youtube.ExtractPlaylistID(url)
	if err != nil {
		return nil, err
	}
	return c.yt.Enumerate(ctx, id, opts.MaxItems)
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
