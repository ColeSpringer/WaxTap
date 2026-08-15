package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// chunkSpan is an inclusive byte range [start, end].
type chunkSpan struct {
	start int64
	end   int64
}

// planChunks splits total bytes into spans of at most chunkSize.
func planChunks(total, chunkSize int64) []chunkSpan {
	var spans []chunkSpan
	for start := int64(0); start < total; start += chunkSize {
		end := start + chunkSize - 1
		if end > total-1 {
			end = total - 1
		}
		spans = append(spans, chunkSpan{start: start, end: end})
	}
	return spans
}

// ToFile downloads src to path. When the content length is known and larger
// than the configured chunk size, it writes byte ranges in parallel through
// io.WriterAt; otherwise it uses a single resumable stream.
//
// Data is staged in a temp file next to path and renamed into place only after
// the download completes. Failed or canceled downloads remove the temp file and
// do not leave path behind.
//
// refresh re-resolves the URL if it expires mid-download; progress receives
// best-effort byte updates. Both may be nil.
func (d *Downloader) ToFile(ctx context.Context, src Source, path string, refresh RefreshFunc, progress ProgressFunc) (Result, error) {
	rep := newProgress(progress, src.ContentLength)
	shared := newSharedSource(src, refresh, d.maxRefreshes)

	f, err := tempfile.New(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Discard() // no-op after a successful Commit

	total := src.ContentLength
	parallel := total > d.chunkSize && d.parallelism > 1
	if !parallel {
		// Unknown or small length: a single streamed GET, resumable on expiry.
		n, err := d.streamToFile(ctx, shared, f, rep)
		if err != nil {
			return Result{}, err
		}
		if err := f.Commit(); err != nil {
			return Result{}, err
		}
		rep.flush()
		return Result{BytesWritten: n}, nil
	}

	if err := d.downloadChunks(ctx, shared, f, planChunks(total, d.chunkSize), rep); err != nil {
		return Result{}, err
	}
	if err := f.Commit(); err != nil {
		return Result{}, err
	}
	rep.flush()
	return Result{BytesWritten: total}, nil
}

// ReaderToFile writes r to path using the same atomic staging as ToFile. It
// commits the temporary file only after reaching EOF, so a failed copy does not
// leave a partial destination. The reader is responsible for cancellation and
// progress reporting.
func (d *Downloader) ReaderToFile(r io.Reader, path string) (Result, error) {
	f, err := tempfile.New(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Discard() // no-op after a successful Commit
	n, err := io.Copy(f, r)
	if err != nil {
		return Result{}, err
	}
	if err := f.Commit(); err != nil {
		return Result{}, err
	}
	return Result{BytesWritten: n}, nil
}

// streamToFile copies a single resumable stream into f starting at offset 0.
func (d *Downloader) streamToFile(ctx context.Context, shared *sharedSource, f *tempfile.File, rep *progressReporter) (int64, error) {
	r := d.newResumableReader(ctx, shared, rep)
	defer r.Close()
	n, err := io.Copy(f, r)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// downloadChunks fetches spans in parallel, writing each at its file offset. The
// first worker error cancels the rest; cancellation is returned even if workers
// stop between spans without a fetch returning an error.
func (d *Downloader) downloadChunks(ctx context.Context, shared *sharedSource, f *tempfile.File, spans []chunkSpan, rep *progressReporter) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	workers := min(d.parallelism, len(spans))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(spans) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				if err := d.fetchChunkToFile(ctx, shared, f, spans[i], rep); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// The caller's cancellation wins over a sibling's failure, which is usually
	// that same cancellation surfacing as a truncated read. It is checked before
	// firstErr because it can also stop workers between spans without any fetch
	// returning an error, including when the caller canceled before work started.
	// The derived ctx needs no separate check: it is only ever done because fail()
	// ran, which sets firstErr.
	//
	// A pending failure that is not itself the cancellation is kept as message
	// detail rather than dropped: a local write failure is not caused by a Ctrl-C,
	// and while the cancellation still decides the class, discarding the cause
	// would lose the only record of what went wrong.
	if err := parent.Err(); err != nil {
		if firstErr != nil && !errors.Is(firstErr, err) {
			return fmt.Errorf("%w: %v", err, firstErr)
		}
		return err
	}
	return firstErr
}

// fetchChunkToFile fetches one span and writes it at its offset, retrying
// transient failures and refreshing an expired Source. The OffsetWriter starts
// at span.start, so workers write disjoint file regions.
func (d *Downloader) fetchChunkToFile(ctx context.Context, shared *sharedSource, f *tempfile.File, span chunkSpan, rep *progressReporter) error {
	expected := span.end - span.start + 1

	// attempt counts transient retries only; a refresh (handled below) does not
	// advance it, so an expired-URL re-resolve never eats the per-chunk budget.
	attempt := 0
	for {
		src, gen := shared.current()

		cctx := ctx
		var cancel context.CancelFunc
		if d.chunkTimeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, d.chunkTimeout)
		}

		resp, err := d.fetch(cctx, src, span.start, span.end)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			if nr, ok := errors.AsType[*needRefreshError](err); ok {
				rerr := handleRefresh(ctx, shared, gen, nr.failure)
				if rerr == nil {
					continue // retry with the refreshed URL; no attempt spent
				}
				// A spent budget drops this 403 into the ordinary ladder below, which it
				// never used to reach. Every other refresh outcome is terminal, and so is
				// a 410 whatever the budget says.
				if !errors.Is(rerr, errRefreshBudgetSpent) || gone(nr.failure) {
					return rerr
				}
				err = rerr
			}
			if attempt >= d.maxChunkRetries || !retryable(ctx, err) {
				return err
			}
			if berr := d.backoff(ctx, attempt); berr != nil {
				return berr
			}
			attempt++
			continue
		}

		w := &countingWriter{w: io.NewOffsetWriter(f, span.start), rep: rep}
		n, copyErr := io.Copy(w, io.LimitReader(resp.Body, expected))
		drainClose(resp)
		if cancel != nil {
			cancel()
		}

		if copyErr == nil && n == expected {
			return nil
		}

		// Remove partial bytes from progress before retrying this span.
		rep.add(-n)
		// Preserve cancellation and deadlines so callers do not retry the download
		// with another client. copyErr is the truncated read the cancellation
		// produced and its chain need not reach context.Canceled, so both are joined:
		// the context error makes the class, copyErr keeps whatever it carries.
		if ctx.Err() != nil {
			if copyErr != nil {
				return fmt.Errorf("download: chunk at offset %d: %w", span.start, errors.Join(ctx.Err(), copyErr))
			}
			return fmt.Errorf("download: chunk at offset %d: %w", span.start, ctx.Err())
		}
		// A persistently short chunk proves that the source did not satisfy the
		// requested range. Mark it incomplete so the caller can try another client.
		if attempt >= d.maxChunkRetries {
			if w.werr != nil {
				// A local write failure must not be mistaken for a truncated source.
				return fmt.Errorf("download: write chunk at offset %d: %w", span.start, w.werr)
			}
			if copyErr != nil {
				return fmt.Errorf("%w: chunk at offset %d: %w", waxerr.ErrIncompleteStream, span.start, copyErr)
			}
			return fmt.Errorf("%w: short chunk at offset %d: got %d bytes, want %d", waxerr.ErrIncompleteStream, span.start, n, expected)
		}
		if berr := d.backoff(ctx, attempt); berr != nil {
			return berr
		}
		attempt++
	}
}
