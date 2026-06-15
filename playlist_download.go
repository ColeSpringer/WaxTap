package waxtap

import (
	"context"
	"fmt"
	rand "math/rand/v2"
	"sync"
	"time"
)

// maxConcurrency bounds DownloadPlaylist's worker pool and matches the CLI limit.
const maxConcurrency = 64

// PlaylistDownloadOptions configures [Client.DownloadPlaylist]. BuildRequest is
// required. Counts and durations must not be negative.
type PlaylistDownloadOptions struct {
	// MaxItems limits playlist enumeration. Zero includes all entries.
	MaxItems int
	// Concurrency limits parallel downloads. Zero uses the client's download
	// concurrency, then a default of 2. BuildRequest calls remain serial and may
	// overlap a download even when Concurrency is 1.
	Concurrency int
	// MaxDownloads limits download attempts. Zero is unlimited. Skipped entries
	// and BuildRequest errors do not count.
	MaxDownloads int
	// SleepInterval is the minimum delay before each download start after the
	// first. Zero disables pacing. With Concurrency set to 1, the delay falls
	// between completed downloads; at higher concurrency, it spaces starts.
	SleepInterval time.Duration
	// MaxSleepInterval, when greater than SleepInterval, randomizes the delay
	// uniformly within that range. It requires a non-zero SleepInterval.
	MaxSleepInterval time.Duration

	// BuildRequest prepares a download request for each entry. Calls are serial
	// and follow playlist order. Returning a non-empty skip reason or an error
	// prevents the download and does not count toward MaxDownloads. Panics are
	// recovered and recorded as BuildRequest errors.
	BuildRequest func(ctx context.Context, e PlaylistEntry) (req Request, skip string, err error)
	// OnItem receives each attempted, skipped, or failed entry. It is not called
	// for entries left in Remaining. Calls may be concurrent and out of playlist
	// order. Panics are recovered and ignored.
	OnItem func(PlaylistItemOutcome)
}

// PlaylistItemOutcome describes the result for one playlist entry.
type PlaylistItemOutcome struct {
	Entry      PlaylistEntry // playlist entry associated with this outcome
	Attempted  bool          // whether Download was called and counted against MaxDownloads
	Result     *Result       // set only after a successful download
	SkipReason string        // set when BuildRequest skipped the entry
	Err        error         // from BuildRequest when not Attempted, otherwise from Download
}

// PlaylistRunResult summarizes a playlist download.
//
// Invariant: Downloaded + Skipped + BuildRequestFailed + DownloadFailed +
// Remaining equals Enumerated. MaxDownloads counts Downloaded and DownloadFailed.
type PlaylistRunResult struct {
	Enumerated         int                   // entries returned by playlist enumeration
	Downloaded         int                   // successful downloads
	Skipped            int                   // entries skipped by BuildRequest
	BuildRequestFailed int                   // entries whose BuildRequest call failed
	DownloadFailed     int                   // entries whose Download call failed
	Remaining          int                   // entries not reached because of a limit or cancellation
	CapReached         bool                  // whether MaxDownloads stopped the run
	EnumErrors         []error               // item errors returned by playlist enumeration
	Outcomes           []PlaylistItemOutcome // reached entries, in playlist order
}

// DownloadPlaylist enumerates a playlist URL and downloads its entries with
// bounded concurrency, optional pacing, and an optional attempt limit.
//
// An enumeration failure returns an error and a nil result. Item-level
// enumeration errors do not stop downloads and are returned in EnumErrors. After
// enumeration succeeds, cancellation returns a partial result with ctx.Err().
func (c *Client) DownloadPlaylist(ctx context.Context, url string, o PlaylistDownloadOptions) (*PlaylistRunResult, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	pl, err := c.Enumerate(ctx, url, EnumerateOptions{MaxItems: o.MaxItems})
	if err != nil {
		return nil, err
	}

	conc := o.Concurrency
	if conc <= 0 {
		conc = c.opts.Concurrency.Downloads
	}
	if conc <= 0 {
		conc = 2
	}
	if conc > maxConcurrency {
		conc = maxConcurrency
	}

	res := runPlaylist(ctx, pl.Entries, conc, o.MaxDownloads, pickSleepWait(o), o.BuildRequest, o.OnItem, c.Download)
	res.EnumErrors = pl.Errors
	return res, ctx.Err()
}

// validate checks options before any network work.
func (o PlaylistDownloadOptions) validate() error {
	if o.BuildRequest == nil {
		return fmt.Errorf("waxtap.DownloadPlaylist: BuildRequest is required")
	}
	if o.MaxItems < 0 || o.Concurrency < 0 || o.MaxDownloads < 0 || o.SleepInterval < 0 || o.MaxSleepInterval < 0 {
		return fmt.Errorf("waxtap.DownloadPlaylist: MaxItems, Concurrency, MaxDownloads, SleepInterval, and MaxSleepInterval must be non-negative")
	}
	if o.MaxSleepInterval > 0 && o.SleepInterval == 0 {
		return fmt.Errorf("waxtap.DownloadPlaylist: MaxSleepInterval requires SleepInterval")
	}
	if o.MaxSleepInterval > 0 && o.MaxSleepInterval < o.SleepInterval {
		return fmt.Errorf("waxtap.DownloadPlaylist: MaxSleepInterval must be >= SleepInterval")
	}
	return nil
}

// runPlaylist builds requests serially and dispatches downloads to a bounded
// worker pool. Each outcome is stored in its playlist slot to preserve order.
func runPlaylist(
	ctx context.Context,
	entries []PlaylistEntry,
	conc, maxDownloads int,
	waitBeforeStart func(ctx context.Context, started int) error,
	buildRequest func(ctx context.Context, e PlaylistEntry) (Request, string, error),
	onItem func(PlaylistItemOutcome),
	dl func(ctx context.Context, req Request) (*Result, error),
) *PlaylistRunResult {
	slots := make([]*PlaylistItemOutcome, len(entries))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	capReached := false
	started := 0

	emit := func(o PlaylistItemOutcome) {
		if onItem == nil {
			return
		}
		defer func() { _ = recover() }()
		onItem(o)
	}

coordinator:
	for i := range entries {
		entry := entries[i]
		req, skip, rerr := safeBuildRequest(ctx, buildRequest, entry)
		// Cancellation during BuildRequest leaves this and later entries in
		// Remaining.
		if ctx.Err() != nil {
			break
		}
		switch {
		case rerr != nil:
			o := PlaylistItemOutcome{Entry: entry, Err: rerr}
			slots[i] = &o
			emit(o)
			continue
		case skip != "":
			o := PlaylistItemOutcome{Entry: entry, SkipReason: skip}
			slots[i] = &o
			emit(o)
			continue
		}

		// A built request does not count until Download starts.
		if maxDownloads > 0 && started == maxDownloads {
			capReached = true
			break
		}

		// Stop waiting for a worker when the run is canceled.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break coordinator
		}

		// A canceled wait releases the worker slot without counting an attempt.
		if werr := waitBeforeStart(ctx, started); werr != nil {
			<-sem
			break
		}

		started++
		wg.Add(1)
		go func(idx int, entry PlaylistEntry, req Request) {
			defer wg.Done()
			defer func() { <-sem }()
			r, derr := dl(ctx, req)
			o := PlaylistItemOutcome{Entry: entry, Attempted: true}
			if derr != nil {
				o.Err = derr
			} else {
				o.Result = r
			}
			slots[idx] = &o
			emit(o)
		}(i, entry, req)
	}
	wg.Wait()

	return tally(len(entries), slots, capReached)
}

// tally compacts reached entries in playlist order and computes the counts.
func tally(enumerated int, slots []*PlaylistItemOutcome, capReached bool) *PlaylistRunResult {
	res := &PlaylistRunResult{Enumerated: enumerated, CapReached: capReached}
	res.Outcomes = make([]PlaylistItemOutcome, 0, enumerated)
	for _, s := range slots {
		if s == nil {
			continue
		}
		res.Outcomes = append(res.Outcomes, *s)
		switch {
		case s.SkipReason != "":
			res.Skipped++
		case !s.Attempted:
			res.BuildRequestFailed++
		case s.Err != nil:
			res.DownloadFailed++
		default:
			res.Downloaded++
		}
	}
	res.Remaining = enumerated - (res.Downloaded + res.Skipped + res.BuildRequestFailed + res.DownloadFailed)
	return res
}

// safeBuildRequest converts a BuildRequest panic to an error.
func safeBuildRequest(ctx context.Context, buildRequest func(context.Context, PlaylistEntry) (Request, string, error), e PlaylistEntry) (req Request, skip string, err error) {
	defer func() {
		if r := recover(); r != nil {
			req, skip, err = Request{}, "", fmt.Errorf("waxtap: playlist BuildRequest panicked: %v", r)
		}
	}()
	return buildRequest(ctx, e)
}

// pickSleepWait builds a context-aware pacing function.
func pickSleepWait(o PlaylistDownloadOptions) func(context.Context, int) error {
	return func(ctx context.Context, started int) error {
		if started == 0 || o.SleepInterval <= 0 {
			return nil
		}
		d := o.SleepInterval
		if o.MaxSleepInterval > o.SleepInterval {
			d += time.Duration(rand.Int64N(int64(o.MaxSleepInterval-o.SleepInterval) + 1))
		}
		return sleepCtx(ctx, d)
	}
}

// sleepCtx waits d or until ctx is done, returning ctx.Err() on cancellation.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
