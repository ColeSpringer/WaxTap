package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// ToWriter streams src to w in order, with bounded memory and no temp file. If
// the URL expires, the downloader refreshes the Source and resumes from the
// number of bytes already written.
//
// Unlike ToFile, ToWriter gives no atomicity guarantee: bytes already handed to
// w are not retractable if a later error occurs. refresh and progress may be nil.
func (d *Downloader) ToWriter(ctx context.Context, src Source, w io.Writer, refresh RefreshFunc, progress ProgressFunc) (Result, error) {
	rep := newProgress(progress, src.ContentLength)
	shared := newSharedSource(src, refresh, d.maxRefreshes)

	r := d.newResumableReader(ctx, shared, rep)
	defer r.Close()

	n, err := io.Copy(w, r)
	if err != nil {
		return Result{}, err
	}
	rep.flush()
	return Result{BytesWritten: n}, nil
}

// Stream returns an io.ReadCloser for src. The reader resumes from its current
// offset after an expired URL or a transient mid-stream failure. The first
// request is issued before Stream returns so early failures surface here instead
// of on the first Read.
//
// The caller must keep ctx alive until Close and must Close the reader; final
// byte counts are known only after the reader is drained. refresh and progress
// may be nil.
func (d *Downloader) Stream(ctx context.Context, src Source, refresh RefreshFunc, progress ProgressFunc) (io.ReadCloser, StreamInfo, error) {
	rep := newProgress(progress, src.ContentLength)
	shared := newSharedSource(src, refresh, d.maxRefreshes)

	r := d.newResumableReader(ctx, shared, rep)
	if err := r.prime(); err != nil {
		_ = r.Close()
		return nil, StreamInfo{}, err
	}
	return r, StreamInfo{ContentLength: r.total, ContentType: r.contentType}, nil
}

// resumableReader presents one sequential byte stream over a Source that may
// need to be refreshed. On 403/410 it refreshes the Source and opens an
// open-ended range from the current offset. On a transient mid-stream failure it
// reopens from the same offset until the stall guard trips.
type resumableReader struct {
	ctx    context.Context
	d      *Downloader
	shared *sharedSource
	rep    *progressReporter

	body        io.ReadCloser
	offset      int64  // bytes already delivered to the caller
	total       int64  // content length, 0 if unknown
	contentType string // learned from the first response

	eof          bool
	err          error // sticky terminal error
	resumeOffset int64 // offset at the previous resume, to detect no-progress stalls
	stalls       int
}

func (d *Downloader) newResumableReader(ctx context.Context, shared *sharedSource, rep *progressReporter) *resumableReader {
	src, _ := shared.current()
	return &resumableReader{
		ctx:          ctx,
		d:            d,
		shared:       shared,
		rep:          rep,
		total:        src.ContentLength,
		resumeOffset: -1,
	}
}

// prime issues the first request so its metadata and any early error are known
// before the reader is handed out.
func (r *resumableReader) prime() error {
	if r.body != nil || r.eof || r.err != nil {
		return r.err
	}
	if err := r.openNext(); err != nil {
		r.err = err
		return err
	}
	return nil
}

// Read implements io.Reader, hiding expiry refreshes and transient resumes.
func (r *resumableReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.eof {
		return 0, io.EOF
	}
	for {
		if r.body == nil {
			if err := r.openNext(); err != nil {
				r.err = err
				return 0, err
			}
		}

		n, err := r.body.Read(p)
		if n > 0 {
			r.offset += int64(n)
			r.rep.add(int64(n))
			r.shared.noteDelivered(int64(n))
		}
		if err == nil {
			return n, nil
		}

		_ = r.body.Close()
		r.body = nil

		// EOF completes the stream when the known total has been reached. If the
		// total is unknown, EOF is the only completion signal available.
		if errors.Is(err, io.EOF) && (r.total <= 0 || r.offset >= r.total) {
			r.eof = true
			r.rep.flush()
			return n, io.EOF
		}

		// Otherwise the transfer is incomplete. Resume from the current offset
		// unless ctx is done or the stall guard stops the loop.
		if r.ctx.Err() != nil {
			r.err = r.ctx.Err()
		} else if resumeErr := r.guardStall(err); resumeErr != nil {
			r.err = resumeErr
		}
		if r.err != nil {
			if n > 0 {
				return n, nil // deliver these bytes; the error surfaces next Read
			}
			return 0, r.err
		}
		if n > 0 {
			return n, nil // deliver, then resume on the next Read (body is nil)
		}
		// n == 0: loop and let openNext reopen from offset.
	}
}

// guardStall bounds resumes that make no forward progress. It returns a
// terminal error once the retry budget is exhausted at the same offset.
func (r *resumableReader) guardStall(cause error) error {
	if r.offset != r.resumeOffset {
		r.resumeOffset = r.offset
		r.stalls = 0
		return nil
	}
	r.stalls++
	if r.stalls > r.d.maxChunkRetries {
		// Repeated resumes without progress prove that the stream is incomplete.
		return fmt.Errorf("%w: stream stalled at offset %d: %w", waxerr.ErrIncompleteStream, r.offset, cause)
	}
	return nil
}

// openNext issues an open-ended ranged GET from the current offset, retrying
// transient failures and refreshing an expired Source. On success it sets
// r.body.
func (r *resumableReader) openNext() error {
	resp, err := r.d.fetchRetrying(r.ctx, r.shared, r.offset, -1)
	if err != nil {
		return err
	}
	r.learn(resp)
	r.body = resp.Body
	return nil
}

// fetchRetrying is the request ladder every fetch of a Source climbs: a 403 or
// 410 renews the Source through handleRefresh without spending an attempt, a
// declined refresh drops into the transient ladder, and everything else backs
// off up to maxChunkRetries. On success the caller owns resp.Body.
//
// end < 0 asks for start to the end of the resource; a bounded request asks for
// [start, end]. An origin that answered a bounded request by ignoring the range
// fails terminally whatever the budget says: it will ignore the retry too, and
// the caller already knows it needs another route. An open-ended resume keeps
// its retries, since there a 200 says the origin served from zero rather than
// that it refuses ranges, and the next attempt may land on a node that does.
func (d *Downloader) fetchRetrying(ctx context.Context, shared *sharedSource, start, end int64) (*http.Response, error) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		src, gen := shared.current()
		resp, err := d.fetch(ctx, src, start, end)
		if err == nil {
			return resp, nil
		}

		if nr, ok := errors.AsType[*needRefreshError](err); ok {
			rerr := handleRefresh(ctx, shared, gen, nr.failure)
			if rerr == nil {
				continue // refreshed: retry without spending an attempt
			}
			// A declined refresh drops this 403 into the ordinary ladder below,
			// which it never used to reach. Every other refresh outcome is
			// terminal, and so is a 410 whatever the budget says.
			if !refreshDeclined(rerr) || gone(nr.failure) {
				return nil, rerr
			}
			err = rerr
		}
		if (end >= 0 && errors.Is(err, errRangeIgnored)) || attempt >= d.maxChunkRetries || !retryable(ctx, err) {
			return nil, err
		}
		if berr := d.backoff(ctx, attempt); berr != nil {
			return nil, berr
		}
		attempt++
	}
}

// learn captures the content type once and fills in the total length when the
// Source did not provide one.
func (r *resumableReader) learn(resp *http.Response) {
	if r.contentType == "" {
		r.contentType = resp.Header.Get("Content-Type")
	}
	if r.total > 0 {
		return
	}
	switch {
	case r.offset == 0 && resp.ContentLength > 0:
		r.total = resp.ContentLength
	default:
		if total := contentRangeTotal(resp); total > 0 {
			r.total = total
		}
	}
	if r.total > 0 {
		r.rep.setTotal(r.total)
	}
}

// Close releases the current response body. It does not cancel ctx, which the
// caller owns; closing the body is sufficient to stop an in-flight read.
func (r *resumableReader) Close() error {
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}
