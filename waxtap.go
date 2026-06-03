package waxtap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/colespringer/waxtap/internal/httpx"
)

// Client is the stable WaxTap entry point used by library callers and the CLI.
// It is safe for concurrent use after construction.
type Client struct {
	opts Options
	log  *slog.Logger
	http *httpx.Client
}

// New constructs a Client from Options, applying defaults for unset fields.
func New(opts Options) (*Client, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	base := opts.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	hc := httpx.New(httpx.Config{
		HTTPClient:   base,
		Logger:       log,
		MaxRetries:   opts.Retry.MaxRetries,
		BaseBackoff:  opts.Retry.BaseBackoff,
		MaxBackoff:   opts.Retry.MaxBackoff,
		MaxRetryWait: opts.Retry.MaxRetryWait,
	})
	return &Client{opts: opts, log: log, http: hc}, nil
}

// Info returns video metadata and candidate audio formats at the requested
// depth, without downloading.
//
// Currently returns ErrNotImplemented.
func (c *Client) Info(ctx context.Context, url string, depth InfoDepth) (*Video, error) {
	return nil, fmt.Errorf("waxtap.Info: %w", ErrNotImplemented)
}

// Enumerate expands a playlist URL into lightweight entries without downloading.
//
// Currently returns ErrNotImplemented.
func (c *Client) Enumerate(ctx context.Context, url string, opts EnumerateOptions) (*Playlist, error) {
	return nil, fmt.Errorf("waxtap.Enumerate: %w", ErrNotImplemented)
}

// Download acquires and processes a single YouTube video to the configured sink.
// It is strictly single-video: a playlist URL returns ErrIsPlaylist (use
// Enumerate and loop).
//
// Currently returns ErrNotImplemented.
func (c *Client) Download(ctx context.Context, req Request) (*Result, error) {
	return nil, fmt.Errorf("waxtap.Download: %w", ErrNotImplemented)
}

// Stream acquires a single YouTube video and returns a reader for source-style
// delivery (pipe to disk or object storage). Final byte counts are known only
// after the reader is drained and closed.
//
// Currently returns ErrNotImplemented.
func (c *Client) Stream(ctx context.Context, req Request) (io.ReadCloser, StreamInfo, error) {
	return nil, StreamInfo{}, fmt.Errorf("waxtap.Stream: %w", ErrNotImplemented)
}

// Process runs the transcode/cut/normalize pipeline on a local file, with no
// YouTube access, through the same source-agnostic pipeline as Download.
//
// Currently returns ErrNotImplemented.
func (c *Client) Process(ctx context.Context, req ProcessRequest) (*Result, error) {
	return nil, fmt.Errorf("waxtap.Process: %w", ErrNotImplemented)
}

// InfoDepth selects how much work Info does. Callers do not pay for what they do
// not request.
type InfoDepth uint8

const (
	// InfoBasic returns metadata and candidate formats (the default).
	InfoBasic InfoDepth = iota
	// InfoResolved additionally resolves stream URLs and expiry. These signed
	// googlevideo URLs are temporary and sensitive; the CLI omits them from
	// human output unless --show-urls is given.
	InfoResolved
	// InfoProbe additionally runs ffprobe on the selected format only. This is
	// network-expensive (it reads the remote signed URL) and is never run on
	// every candidate.
	InfoProbe
)
