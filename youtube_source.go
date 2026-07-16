package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxtap/v3/download"
	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/httpx"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
	"github.com/colespringer/waxtap/v3/youtube"
)

// SponsorBlockSegments returns skip segments for videoURL using the client's
// SponsorBlock settings and shared HTTP client. An empty categories slice uses
// [DefaultCategories]. The method does not cut or download media.
func (c *Client) SponsorBlockSegments(ctx context.Context, videoURL string, categories []Category) ([]Segment, error) {
	id, err := youtube.ExtractVideoID(videoURL)
	if err != nil {
		return nil, err
	}
	d := c.opts.Timeouts.SponsorBlock
	if c.opts.SponsorBlock.Timeout > 0 {
		d = c.opts.SponsorBlock.Timeout
	}
	sbCtx, cancel := withTimeout(ctx, d)
	defer cancel()
	return c.sb.FetchSegments(sbCtx, id, categories)
}

// acquired contains a selected format, its transfer backend, and the extraction
// attempt that produced it.
type acquired struct {
	video    *youtube.Video
	fmtSel   Format
	transfer mediaTransfer
	attempt  youtube.AttemptID
	client   string // display name for logs and warnings
	// substitutedFrom names the forced client replaced by the WEB watch-page
	// fallback. It is reported only after delivery succeeds.
	substitutedFrom string
}

// webContextCooldown limits a failing WEB player-context provider to one attempt
// per window during batch downloads.
const webContextCooldown = 30 * time.Second

// isIncompleteDelivery reports whether another client may be able to complete a
// download that ended early.
func isIncompleteDelivery(err error) bool {
	return errors.Is(err, ErrIncompleteStream) || errors.Is(err, ErrURLExpired)
}

// watchPageSkip returns the extraction attempts disabled when watch-page
// fallback is not allowed.
func watchPageSkip(noFallback bool) map[youtube.AttemptID]bool {
	skip := map[youtube.AttemptID]bool{}
	if noFallback {
		skip[youtube.AttemptWatchPage] = true
	}
	return skip
}

// baseSkip returns the extraction attempts disabled before a request starts.
func baseSkip(req Request) map[youtube.AttemptID]bool {
	return watchPageSkip(req.NoFallback)
}

// forcedSingleWeb reports whether the configured chain contains only the
// built-in WEB client.
func (c *Client) forcedSingleWeb() bool {
	name, ok := c.yt.ForcedSingleClient()
	return ok && youtube.IsWebClient(name)
}

// acquire extracts, selects, and resolves a single transfer. It is used for sinks
// that cannot discard bytes after an incomplete delivery.
func (c *Client) acquire(ctx context.Context, req Request, id string, em *emitter) (*acquired, error) {
	target := transcodeTarget(req.Transcode)

	// Try the optional WEB player context before the configured client chain.
	// Caller cancellation and NoFallback stop before the chain is attempted.
	webCtxReason := c.initialWebContextReason()
	if c.yt.WebContextConfigured() && !c.webContextCoolingDown() {
		a, err := c.acquireWebContext(ctx, req, id, target, em, 0)
		if err == nil {
			return a, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if req.NoFallback {
			return nil, err
		}
		webCtxReason = "failed: " + err.Error()
	}

	em.stage(StageExtracting)
	ectx, ecancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	defer ecancel()
	ext, err := c.yt.ExtractExcluding(ectx, id, baseSkip(req))
	if err != nil {
		// Don't blame the player-context endpoint when the request was canceled.
		if ctx.Err() == nil {
			c.warnWebContextEndpointFailed(em, webCtxReason)
		}
		return nil, err
	}
	a, err := c.buildTransfer(ctx, req, id, target, ext, em, 0)
	if err != nil {
		if ctx.Err() == nil {
			c.warnWebContextEndpointFailed(em, webCtxReason)
		}
		return nil, err
	}
	c.warnWebContextFallback(em, a, webCtxReason)
	c.warnSessionDowngrade(em, a)
	// Stream and Writer succeed once buildTransfer returns.
	c.warnClientSubstitution(em, a)
	c.applyFullMetadata(ctx, req, a)
	return a, nil
}

// initialWebContextReason reports why a configured player-context was skipped
// before extraction starts.
func (c *Client) initialWebContextReason() string {
	if c.yt.WebContextConfigured() && c.webContextCoolingDown() {
		return "in cooldown after a recent failure"
	}
	return ""
}

// warnWebContextFallback emits one warning when a configured player-context did
// not deliver and another client did. Successful downloads still need to report
// that the configured WEB context was bypassed; callers that require WEB delivery
// can pass --no-fallback.
func (c *Client) warnWebContextFallback(em *emitter, delivered *acquired, reason string) {
	if !c.yt.WebContextConfigured() || delivered.attempt == youtube.AttemptWebContext {
		return
	}
	if reason == "" {
		reason = "unavailable"
	}
	detail := fmt.Sprintf("web player-context did not deliver (%s); served via %s", reason, delivered.client)
	em.warn(WarnWebContextFallback, withAuthHint(detail, reason)+"; pass --no-fallback to require WEB delivery")
}

// warnWebContextEndpointFailed reports a configured WEB player-context failure
// after the fallback chain also fails. That keeps the endpoint failure visible
// when the final error is a generic downstream aggregate, such as an incomplete
// stream after every client is exhausted. It fires only for the "failed: " reason
// form, not for a cooldown skip or a delivered stream that was later capped.
func (c *Client) warnWebContextEndpointFailed(em *emitter, reason string) {
	cause, ok := strings.CutPrefix(reason, "failed: ")
	if !ok {
		return
	}
	detail := fmt.Sprintf("web player-context endpoint returned an unexpected response (%s); the fallback also failed", cause)
	em.warn(WarnWebContextFallback, withAuthHint(detail, reason))
}

// withAuthHint appends the api-key hint when reason carries an HTTP auth
// rejection, so the two web-context warnings stay consistent.
func withAuthHint(detail, reason string) string {
	if authFailureInReason(reason) {
		return detail + "; set or verify --api-key"
	}
	return detail
}

// isWebFamily reports whether a client display name belongs to the WEB family.
func isWebFamily(client string) bool {
	return strings.Contains(strings.ToUpper(client), "WEB")
}

// authFailureInReason reports whether a fallback reason contains an HTTP
// authentication rejection.
func authFailureInReason(reason string) bool {
	return strings.Contains(reason, "HTTP 401") || strings.Contains(reason, "HTTP 403")
}

// warnSessionDowngrade warns when a request configured for WEB audio is delivered
// by a non-WEB client. Player-context fallback is reported separately.
func (c *Client) warnSessionDowngrade(em *emitter, a *acquired) {
	if c.yt.WebContextConfigured() {
		return
	}
	expectsWeb := c.opts.Session != nil || c.opts.SessionProvider != nil || c.forcedSingleWeb()
	if !expectsWeb || isWebFamily(a.client) {
		return
	}
	em.warn(WarnFallbackProfile, fmt.Sprintf("expected full WEB audio but the %s client delivered the stream", a.client))
}

// buildTransfer selects and resolves a format from ext. When pinnedItag is
// non-zero, selection prefers that encoding.
func (c *Client) buildTransfer(ctx context.Context, req Request, id string, target format.Target, ext *youtube.Extraction, em *emitter, pinnedItag int) (*acquired, error) {
	video, selFmt, plan, err := c.selectAndResolve(ctx, req, target, ext, em, pinnedItag)
	if err != nil {
		return nil, err
	}
	a := &acquired{video: video, fmtSel: selFmt, attempt: ext.Attempt(), client: ext.ClientName(), substitutedFrom: ext.SubstitutedFrom()}

	// SABR reloads are pinned to the original attempt by SABRStream.reextract.
	if plan.SABR != nil {
		// Prime before Open so acquisition can fall back when the provider fails.
		pctx, cancel := withTimeout(ctx, c.opts.Timeouts.Resolve)
		err := plan.SABR.PrimeToken(pctx)
		cancel()
		if err != nil {
			return nil, err
		}
		a.transfer = sabrTransfer{dl: c.dl, handle: plan.SABR}
		return a, nil
	}
	a.transfer = urlTransfer{dl: c.dl, src: toSource(*plan.Direct), refresh: c.directRefresh(req, id, target, ext.Attempt(), selFmt.Itag, em)}
	return a, nil
}

// warnClientSubstitution reports a successful WEB watch-page fallback.
func (c *Client) warnClientSubstitution(em *emitter, a *acquired) {
	if a.substitutedFrom != "" {
		em.warn(WarnFallbackProfile, fmt.Sprintf("forced client %s failed; used WEB through the watch-page fallback", a.substitutedFrom))
	}
}

// directRefresh builds a signed-URL refresh callback pinned to the original
// extraction attempt and itag. Pinning prevents a resumed range from mixing bytes
// from different encodings.
func (c *Client) directRefresh(req Request, id string, target format.Target, attempt youtube.AttemptID, pinnedItag int, em *emitter) download.RefreshFunc {
	return func(fctx context.Context, failure *potoken.HTTPFailure) (download.Source, error) {
		rext, rerr := func() (*youtube.Extraction, error) {
			fectx, cancel := withTimeout(fctx, c.opts.Timeouts.Extraction)
			defer cancel()
			return c.yt.ExtractAttempt(fectx, id, attempt)
		}()
		if rerr != nil {
			return download.Source{}, refreshFailure(fctx, "re-extract attempt "+string(attempt), rerr)
		}
		// A refresh resumes an existing byte range, so the original itag is
		// mandatory. A client fallback starts from offset zero and may select a
		// substitute format.
		ridx, rerr := selectIndex(Itag(pinnedItag), req.SourcePolicy, target, rext.Video().Formats)
		if rerr != nil {
			return download.Source{}, fmt.Errorf("%w: pinned itag %d absent after re-extract: %v", ErrURLExpired, pinnedItag, rerr)
		}
		rrctx, cancel := withTimeout(fctx, c.opts.Timeouts.Resolve)
		defer cancel()
		nplan, rerr := c.yt.ResolveWithFailure(rrctx, rext, ridx, failure)
		if rerr != nil {
			return download.Source{}, refreshFailure(fctx, "re-resolve after refresh", rerr)
		}
		if nplan.Direct == nil {
			return download.Source{}, fmt.Errorf("%w: stream refresh resolved itag %d to SABR", ErrURLExpired, pinnedItag)
		}
		em.warn(WarnURLReResolved, "stream URL re-resolved after expiry")
		return toSource(*nplan.Direct), nil
	}
}

// refreshFailure preserves errors that must stop fallback. Other refresh failures
// become ErrURLExpired so file-based downloads can restart with another client.
func refreshFailure(fctx context.Context, what string, err error) error {
	if ctxErr := fctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrRateLimited) || isAvailabilityError(err) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrURLExpired, what, err)
}

// isAvailabilityError reports whether err describes the video's availability.
func isAvailabilityError(err error) bool {
	return errors.Is(err, ErrVideoUnavailable) ||
		errors.Is(err, ErrVideoRestricted) ||
		errors.Is(err, ErrLoginRequired) ||
		errors.Is(err, ErrLiveContent) ||
		errors.Is(err, ErrLiveNotStarted) ||
		errors.Is(err, ErrAgeRestricted) ||
		errors.Is(err, ErrMembersOnly) ||
		errors.Is(err, ErrGeoBlocked) ||
		errors.Is(err, ErrNoAudioFormats)
}

// isUpstreamDiagnostic reports whether err describes extraction, authentication,
// or availability rather than a local I/O failure.
func isUpstreamDiagnostic(err error) bool {
	return errors.Is(err, ErrNeedsPOToken) ||
		errors.Is(err, ErrExtractionFailed) ||
		errors.Is(err, ErrCipherSolve) ||
		isAvailabilityError(err)
}

// acquireWebContext builds a SABR transfer from an attested WEB player context.
// Only provider failures start the provider cooldown.
func (c *Client) acquireWebContext(ctx context.Context, req Request, id string, target format.Target, em *emitter, pinnedItag int) (*acquired, error) {
	em.stage(StageExtracting)
	ext, err := c.yt.ExtractWebContext(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			c.noteWebContextFailure()
		}
		return nil, err
	}
	c.noteWebContextSuccess()

	// Only player-context failures affect its cooldown. GVS token failures come
	// from a separate provider.
	a, err := c.buildTransfer(ctx, req, id, target, ext, em, pinnedItag)
	if err != nil {
		return nil, err
	}
	if _, ok := a.transfer.(sabrTransfer); !ok {
		return nil, fmt.Errorf("WEB player-context did not resolve to a SABR stream")
	}
	return a, nil
}

// selectAndResolve selects a format and resolves its delivery plan. A non-zero
// pinnedItag preserves the preferred encoding across client fallback.
func (c *Client) selectAndResolve(ctx context.Context, req Request, target format.Target, ext *youtube.Extraction, em *emitter, pinnedItag int) (*youtube.Video, Format, youtube.MediaPlan, error) {
	video := ext.Video()
	idx, err := c.selectSourceIndex(req, target, video.Formats, pinnedItag)
	if err != nil {
		return nil, Format{}, youtube.MediaPlan{}, err
	}

	em.stage(StageResolving)
	rctx, rcancel := withTimeout(ctx, c.opts.Timeouts.Resolve)
	defer rcancel()
	plan, err := c.yt.Resolve(rctx, ext, idx)
	if err != nil {
		return nil, Format{}, youtube.MediaPlan{}, err
	}
	c.log.DebugContext(ctx, "stream resolved",
		"itag", video.Formats[idx].Itag, "codec", video.Formats[idx].Codec, "contentLength", video.Formats[idx].ContentLength)
	return video, video.Formats[idx], plan, nil
}

// selectSourceIndex chooses a source format, preferring pinnedItag when available.
// If a fallback client lacks that itag, normal selection chooses a replacement.
func (c *Client) selectSourceIndex(req Request, target format.Target, formats []Format, pinnedItag int) (int, error) {
	if pinnedItag != 0 {
		if idx, err := selectIndex(Itag(pinnedItag), req.SourcePolicy, target, formats); err == nil {
			return idx, nil
		}
	}
	// The facade defaults audio selection to stereo so a bare Request does not hand
	// back a surround track; a caller opts into any-fidelity with
	// Audio: BestAudio().WithChannels(LayoutAny). The pinned-itag re-selection above
	// is an itag selector and ignores layout, so this only shapes the first pick.
	idx, err := selectIndex(req.Audio.WithDefaultChannels(defaultFacadeLayout), req.SourcePolicy, target, formats)
	if err != nil {
		return -1, err
	}
	if pinnedItag != 0 && formats[idx].Itag != pinnedItag {
		c.log.Info("pinned itag absent on fallback client; selecting a different format",
			"pinnedItag", pinnedItag, "itag", formats[idx].Itag, "codec", formats[idx].Codec, "ext", sourceExt(formats[idx]))
	}
	return idx, nil
}

// acquireNext resolves the next non-skipped extraction attempt. It returns the
// attempt ID when one attempt can be skipped after a selection or resolution
// failure. An empty ID means that no individual attempt can be blamed.
func (c *Client) acquireNext(ctx context.Context, req Request, id string, target format.Target, em *emitter, skip map[youtube.AttemptID]bool, pinnedItag int) (*acquired, youtube.AttemptID, error) {
	if c.yt.WebContextConfigured() && !c.webContextCoolingDown() && !skip[youtube.AttemptWebContext] {
		a, err := c.acquireWebContext(ctx, req, id, target, em, pinnedItag)
		if err == nil {
			return a, youtube.AttemptWebContext, nil
		}
		if ctx.Err() != nil {
			return nil, youtube.AttemptWebContext, ctx.Err()
		}
		// The caller records this reason and warns after another client delivers.
		return nil, youtube.AttemptWebContext, err
	}

	em.stage(StageExtracting)
	ectx, ecancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	ext, err := c.yt.ExtractExcluding(ectx, id, skip)
	ecancel()
	if err != nil {
		return nil, "", err
	}
	a, err := c.buildTransfer(ctx, req, id, target, ext, em, pinnedItag)
	if err != nil {
		return nil, ext.Attempt(), err
	}
	return a, ext.Attempt(), nil
}

// acquireAndDownload downloads to a file, retrying incomplete deliveries with
// other extraction attempts. dest returns the path for each selected format.
//
// Cancellation, rate limiting, and local download failures stop the loop.
func (c *Client) acquireAndDownload(ctx context.Context, req Request, id string, em *emitter, dest func(*acquired) string) (*acquired, download.Result, string, error) {
	target := transcodeTarget(req.Transcode)
	skip := baseSkip(req)
	var causes attemptErrors
	pinnedItag := 0
	firstClient := ""
	firstFromWebContext := false
	webContextRetried := false
	webCtxReason := c.initialWebContextReason()
	progress := func(p download.Progress) { em.progress(p.BytesWritten, p.Total) }

	for {
		a, attempt, err := c.acquireNext(ctx, req, id, target, em, skip, pinnedItag)
		if err != nil {
			if ctx.Err() != nil {
				return nil, download.Result{}, "", ctx.Err()
			}
			if errors.Is(err, ErrRateLimited) {
				return nil, download.Result{}, "", err
			}
			if attempt == youtube.AttemptWebContext {
				webCtxReason = "failed: " + err.Error()
			}
			if attempt == "" {
				// ErrChainExhausted only marks the end of the chain. The recorded
				// per-attempt errors contain the useful causes.
				if !errors.Is(err, waxerr.ErrChainExhausted) {
					causes.add(attempt, err)
				}
				break
			}
			causes.add(attempt, err)
			if req.NoFallback {
				break // do not try another download attempt
			}
			skip[attempt] = true
			continue
		}

		// Prefer the first selected encoding on later attempts.
		if pinnedItag == 0 {
			pinnedItag = a.fmtSel.Itag
		}
		if firstClient == "" {
			firstClient = a.client
			firstFromWebContext = a.attempt == youtube.AttemptWebContext
		}
		path := dest(a)
		em.stage(StageDownloading)
		res, derr := a.transfer.toFile(ctx, path, progress)
		if derr == nil {
			// Use the more specific web-context fallback warning below.
			if a.client != firstClient && !firstFromWebContext {
				em.warn(WarnFallbackProfile, fmt.Sprintf("client %q did not complete the stream; used %q", firstClient, a.client))
			}
			c.warnWebContextFallback(em, a, webCtxReason)
			c.warnSessionDowngrade(em, a)
			// Report substitution only after the bytes arrive.
			c.warnClientSubstitution(em, a)
			c.applyFullMetadata(ctx, req, a)
			c.log.DebugContext(ctx, "download complete",
				"client", a.client, "itag", a.fmtSel.Itag, "bytes", res.BytesWritten)
			return a, res, path, nil
		}
		if ctx.Err() != nil {
			return nil, download.Result{}, "", ctx.Err()
		}
		if errors.Is(derr, ErrRateLimited) {
			return nil, download.Result{}, "", derr
		}
		if !isIncompleteDelivery(derr) {
			// Preserve an earlier incomplete-delivery error when a later attempt
			// fails during extraction or availability checks. Local I/O errors
			// remain terminal.
			if isUpstreamDiagnostic(derr) && causes.hasIncomplete() {
				causes.add(a.attempt, derr)
				break
			}
			return nil, download.Result{}, "", derr
		}
		// Record the cap (a non-retrying first/only cap, or any non-web-context cap)
		// so a later retry that fails to re-extract still surfaces ErrIncompleteStream
		// in the aggregate. The retry's own second cap re-enters here with
		// webContextRetried set and is skipped to avoid a duplicate "tried" entry.
		if a.attempt != youtube.AttemptWebContext || !webContextRetried {
			causes.add(a.attempt, derr)
		}
		// Retry the same web-context attempt once with a fresh context before falling
		// back. ExtractWebContext re-fetches /player-context each call, so re-entering
		// acquireWebContext (no skip set) yields a new, likely status-1 context. Confirm
		// it is still selectable because a concurrent sibling may have armed the
		// cooldown. This is the same attempt, not a client switch, so it is allowed
		// under --no-fallback.
		if a.attempt == youtube.AttemptWebContext && !webContextRetried && !c.webContextCoolingDown() {
			webContextRetried = true
			webCtxReason = "stream capped before completion"
			em.warn(WarnWebContextRetry, "attested WEB context was capped (attestation status 2, usually transient); retrying once with a fresh context")
			continue
		}
		if req.NoFallback {
			break // do not switch clients after an incomplete delivery
		}
		skip[a.attempt] = true
		// The watch-page fallback also uses WEB, so it is not a distinct retry for a
		// forced WEB client.
		if c.forcedSingleWeb() && a.attempt != youtube.AttemptWebContext {
			skip[youtube.AttemptWatchPage] = true
			continue
		}
		// A later web-context fallback warning covers this transition.
		if a.attempt != youtube.AttemptWebContext {
			em.warn(WarnIncompleteFallback, fmt.Sprintf("client %q returned an incomplete stream; checking remaining clients", a.client))
		}
	}
	// The download chain is exhausted. If a configured player-context failed and a
	// fallback was attempted but never delivered, surface that endpoint failure next
	// to the aggregate. Under --no-fallback no fallback was attempted, so the
	// endpoint failure is already the returned error.
	if !req.NoFallback {
		c.warnWebContextEndpointFailed(em, webCtxReason)
	}
	return nil, download.Result{}, "", causes.aggregate()
}

// attemptErrors collects failures from a cross-client download.
type attemptErrors struct {
	causes []attemptCause
}

type attemptCause struct {
	id  youtube.AttemptID
	err error
}

func (a *attemptErrors) add(id youtube.AttemptID, err error) {
	a.causes = append(a.causes, attemptCause{id: id, err: err})
}

// hasIncomplete reports whether any recorded cause is an incomplete delivery.
func (a *attemptErrors) hasIncomplete() bool {
	for _, c := range a.causes {
		if isIncompleteDelivery(c.err) {
			return true
		}
	}
	return false
}

func (a *attemptErrors) aggregate() error {
	if len(a.causes) == 0 {
		return ErrIncompleteStream
	}
	best := a.causes[0].err
	for _, cause := range a.causes[1:] {
		best = waxerr.PreferErr(best, cause.err)
	}
	var tried []string
	for _, cause := range a.causes {
		if cause.id != "" {
			tried = append(tried, string(cause.id))
		}
	}
	summary := "no attempted client delivered a complete stream"
	if len(tried) > 0 {
		summary += " (tried " + strings.Join(tried, ", ") + ")"
	}
	switch {
	case errors.Is(best, ErrIncompleteStream):
		// Preserve the most useful truncation detail.
		return fmt.Errorf("%s: %w", summary, best)
	case isIncompleteDelivery(best):
		// A refresh failure is an incomplete file delivery once all attempts fail.
		return fmt.Errorf("%w: %s: %w", ErrIncompleteStream, summary, best)
	case len(tried) == 0:
		return best
	default:
		return fmt.Errorf("%w (tried %s)", best, strings.Join(tried, ", "))
	}
}

// webContextCoolingDown reports whether the WEB player-context attempt is
// skipped because the provider recently failed.
func (c *Client) webContextCoolingDown() bool {
	c.webCtxMu.Lock()
	defer c.webCtxMu.Unlock()
	return time.Now().Before(c.webCtxDownUntil)
}

// noteWebContextFailure starts the provider cooldown window.
func (c *Client) noteWebContextFailure() {
	c.webCtxMu.Lock()
	c.webCtxDownUntil = time.Now().Add(webContextCooldown)
	c.webCtxMu.Unlock()
}

// noteWebContextSuccess clears any cooldown.
func (c *Client) noteWebContextSuccess() {
	c.webCtxMu.Lock()
	c.webCtxDownUntil = time.Time{}
	c.webCtxMu.Unlock()
}

// mediaTransfer delivers media from either a direct URL or a SABR stream.
type mediaTransfer interface {
	toFile(ctx context.Context, path string, progress download.ProgressFunc) (download.Result, error)
	toWriter(ctx context.Context, w io.Writer, progress download.ProgressFunc) (download.Result, error)
	stream(ctx context.Context, progress download.ProgressFunc) (io.ReadCloser, download.StreamInfo, error)
}

// urlTransfer delivers a signed URL through the chunked downloader.
type urlTransfer struct {
	dl      *download.Downloader
	src     download.Source
	refresh download.RefreshFunc
}

func (t urlTransfer) toFile(ctx context.Context, path string, progress download.ProgressFunc) (download.Result, error) {
	return t.dl.ToFile(ctx, t.src, path, t.refresh, progress)
}

func (t urlTransfer) toWriter(ctx context.Context, w io.Writer, progress download.ProgressFunc) (download.Result, error) {
	return t.dl.ToWriter(ctx, t.src, w, t.refresh, progress)
}

func (t urlTransfer) stream(ctx context.Context, progress download.ProgressFunc) (io.ReadCloser, download.StreamInfo, error) {
	return t.dl.Stream(ctx, t.src, t.refresh, progress)
}

// sabrTransfer delivers a sequential SABR stream. The SABR layer reports its own
// progress.
type sabrTransfer struct {
	dl     *download.Downloader
	handle *youtube.SABRStream
}

func (t sabrTransfer) toFile(ctx context.Context, path string, progress download.ProgressFunc) (download.Result, error) {
	rc, _, err := t.handle.Open(ctx, sabrProgress(progress))
	if err != nil {
		return download.Result{}, err
	}
	defer rc.Close()
	return t.dl.ReaderToFile(rc, path)
}

func (t sabrTransfer) toWriter(ctx context.Context, w io.Writer, progress download.ProgressFunc) (download.Result, error) {
	rc, _, err := t.handle.Open(ctx, sabrProgress(progress))
	if err != nil {
		return download.Result{}, err
	}
	defer rc.Close()
	n, err := io.Copy(w, rc)
	if err != nil {
		return download.Result{}, err
	}
	return download.Result{BytesWritten: n}, nil
}

func (t sabrTransfer) stream(ctx context.Context, progress download.ProgressFunc) (io.ReadCloser, download.StreamInfo, error) {
	rc, info, err := t.handle.Open(ctx, sabrProgress(progress))
	if err != nil {
		return nil, download.StreamInfo{}, err
	}
	return rc, download.StreamInfo{ContentLength: info.ContentLength, ContentType: info.ContentType}, nil
}

// sabrProgress adapts a download progress callback to SABR's byte counts.
func sabrProgress(p download.ProgressFunc) func(bytesWritten, total int64) {
	if p == nil {
		return nil
	}
	return func(bw, total int64) { p(download.Progress{BytesWritten: bw, Total: total}) }
}

// Download acquires and processes a single YouTube video to the configured sink.
// It is strictly single-video: a playlist URL returns ErrIsPlaylist (use
// Enumerate and loop).
//
// Audio selection defaults to stereo, so a bare Request (zero Audio) yields the
// best stereo track rather than a surround one. Set
// Audio: BestAudio().WithChannels(LayoutSurround) for surround, or
// WithChannels(LayoutAny) to rank purely by fidelity.
//
// When no processing is requested (a nil ProcessSpec) it downloads the selected
// source stream straight to the sink with no processing and no temp file: the bytes
// are byte-identical to what YouTube served, so Result.SourceBytes ==
// Result.OutputBytes, Result.OutputFormat == Result.SourceFormat, and
// Result.Transcoded is false. A TranscodeSpec with FormatCopy is different: it
// remuxes into the target container, so no re-encode happens but the bytes and
// container may change. When a cut, transcode, or
// loudness stage is requested it stages the source to a temp file, runs the fused
// pipeline, and finalizes to the sink.
func (c *Client) Download(ctx context.Context, req Request) (res *Result, err error) {
	em := newEmitter(req.Events, "")
	defer func() { em.finish(res, err) }()

	id, err := youtube.ExtractVideoID(req.URL)
	if err != nil {
		return nil, err
	}
	em.videoID = id
	if err = validateProcessSpec(req.ProcessSpec); err != nil {
		return nil, err
	}
	// Report HTTP throttling as job warnings.
	ctx = httpx.WithThrottleHook(ctx, func(e httpx.ThrottleEvent) { emitThrottle(em, e) })

	if req.Output.kind == outputNone {
		return nil, fmt.Errorf("waxtap.Download: an Output is required (use Stream for reader delivery)")
	}
	if req.Output.kind == outputFile {
		if req.SkipIfExists && fileExists(req.Output.path) {
			em.stage(StageSkipped)
			return &Result{SourceKind: SourceYouTube, VideoID: id, OutputPath: req.Output.path}, nil
		}
		// Create the output directory before downloading so staging failures are
		// reported early.
		if err := ensureParentDir(req.Output.path); err != nil {
			return nil, err
		}
	}

	if !needsProcessing(req.ProcessSpec) {
		return c.deliverSource(ctx, req, id, em)
	}
	return c.downloadAndProcess(ctx, req, id, em)
}

// deliverSource downloads without processing. File outputs can retry incomplete
// attempts because staging is atomic; Writer outputs cannot retract written bytes.
func (c *Client) deliverSource(ctx context.Context, req Request, id string, em *emitter) (*Result, error) {
	switch req.Output.kind {
	case outputFile:
		a, r, _, err := c.acquireAndDownload(ctx, req, id, em, func(*acquired) string { return req.Output.path })
		if err != nil {
			return nil, err
		}
		em.stage(StageFinalizing)
		return &Result{
			SourceKind:   SourceYouTube,
			VideoID:      id,
			Title:        a.video.Title,
			Client:       a.client,
			SourceFormat: a.fmtSel,
			OutputFormat: a.fmtSel,
			OutputPath:   req.Output.path,
			SourceBytes:  r.BytesWritten,
			OutputBytes:  r.BytesWritten,
			Metadata:     videoMetadataFor(req, a.video),
		}, nil
	case outputWriter:
		a, err := c.acquire(ctx, req, id, em)
		if err != nil {
			return nil, err
		}
		em.stage(StageDownloading)
		r, derr := a.transfer.toWriter(ctx, req.Output.writer, func(p download.Progress) { em.progress(p.BytesWritten, p.Total) })
		if derr != nil {
			if isIncompleteDelivery(derr) {
				// Written bytes cannot be retracted, so report the partial delivery.
				return nil, fmt.Errorf("%w: %v", ErrIncompleteStream, derr)
			}
			return nil, derr
		}
		em.stage(StageFinalizing)
		return &Result{
			SourceKind:   SourceYouTube,
			VideoID:      id,
			Title:        a.video.Title,
			Client:       a.client,
			SourceFormat: a.fmtSel,
			OutputFormat: a.fmtSel,
			SourceBytes:  r.BytesWritten,
			OutputBytes:  r.BytesWritten,
			Metadata:     videoMetadataFor(req, a.video),
		}, nil
	}
	return nil, fmt.Errorf("waxtap: unsupported output kind for keep-source delivery")
}

// downloadAndProcess stages the source to a temp file and runs the fused
// pipeline, then finalizes to the sink. For a file sink the pipeline writes the
// destination path directly (atomic), so only a measure-only pass needs a move.
func (c *Client) downloadAndProcess(ctx context.Context, req Request, id string, em *emitter) (*Result, error) {
	jobDir, err := c.makeJobDir()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(jobDir)

	pipeOut := ""
	if req.Output.kind == outputFile {
		pipeOut = req.Output.path
	}

	deliver, res, err := c.produce(ctx, req, id, jobDir, pipeOut, em)
	if err != nil {
		return nil, err
	}

	em.stage(StageFinalizing)
	switch req.Output.kind {
	case outputFile:
		if deliver != req.Output.path {
			// Measure-only/no-op: the pipeline wrote nothing, so move the staged
			// source into place.
			if err := moveFile(deliver, req.Output.path); err != nil {
				return nil, err
			}
		}
		res.OutputPath = req.Output.path
		res.OutputBytes = fileSize(req.Output.path)
	case outputWriter:
		n, err := streamFileTo(req.Output.writer, deliver)
		if err != nil {
			return nil, err
		}
		res.OutputBytes = n
	}
	return res, nil
}

// produce downloads the source into jobDir, collects cut ranges, and runs the
// pipeline writing to pipeOut (or a temp inside jobDir when pipeOut is ""). It
// returns the deliverable file path and a Result with metadata and flags filled,
// leaving sink-specific fields to the caller.
func (c *Client) produce(ctx context.Context, req Request, id, jobDir, pipeOut string, em *emitter) (string, *Result, error) {
	// Under the fail policy, probe SponsorBlock before the download so an outage
	// stops the request before media transfer starts. This only inspects the error
	// and discards the segments; collectRanges below remains the single emitter of
	// empty/proceed-uncut warnings. The same id keys the cache, so the later fetch
	// is a hit unless the cache is disabled.
	if cs := req.Cut; cs != nil && cs.SponsorBlock != nil && cs.OnError == FailDownload {
		sbCtx, cancel := withTimeout(ctx, c.sponsorBlockTimeout(cs))
		_, ferr := c.sb.FetchSegments(sbCtx, id, cs.SponsorBlock)
		cancel()
		if ferr != nil {
			return "", nil, fmt.Errorf("waxtap: SponsorBlock fetch failed: %w", ferr)
		}
	}

	// The selected format determines the staged source filename.
	dest := func(a *acquired) string { return filepath.Join(jobDir, "source"+sourceExt(a.fmtSel)) }
	a, dlRes, srcPath, err := c.acquireAndDownload(ctx, req, id, em, dest)
	if err != nil {
		return "", nil, err
	}
	srcExt := sourceExt(a.fmtSel)

	em.stage(StageStaging)
	ranges, sbRanges, err := c.collectRanges(ctx, req.Cut, a.video.ID, em)
	if err != nil {
		return "", nil, err
	}

	eo := embedOptions{thumbnail: req.EmbedThumbnail, metadata: req.EmbedMetadata}
	// The delivered file's extension: the post-pass must not remux into a container
	// the extension would then misname. A Writer sink has no path, hence no
	// extension constraint.
	embedExt := ""
	if req.Output.kind == outputFile {
		embedExt = strings.ToLower(strings.TrimPrefix(filepath.Ext(req.Output.path), "."))
	}

	// A SponsorBlock-only request can resolve to no ranges. In that case, deliver
	// the staged source with no re-encode. Explicit FormatCopy still remuxes, and a
	// downmix needs the probe to decide whether to fold. An embed post-pass may
	// still tag the staged source in place.
	if len(ranges) == 0 && req.Transcode == nil && req.Loudness == nil && !req.Downmix {
		c.embedMetadata(ctx, srcPath, embedExt, a.video, eo, em)
		res := &Result{
			SourceKind:   SourceYouTube,
			VideoID:      a.video.ID,
			Title:        a.video.Title,
			Client:       a.client,
			SourceFormat: a.fmtSel,
			OutputFormat: a.fmtSel,
			SourceBytes:  dlRes.BytesWritten,
			Metadata:     videoMetadataFor(req, a.video),
		}
		return srcPath, res, nil
	}

	runner := c.engine()

	out := pipeOut
	if out == "" {
		out = filepath.Join(jobDir, "output"+outputExt(req.Transcode, srcExt))
	}

	pres, err := pipeline.Run(ctx, runner, srcPath, out, pipelineSpec(req.ProcessSpec, ranges), em.pipelineStage)
	if err != nil {
		return "", nil, err
	}
	warnEmptyCut(em, req.Cut, pres, len(sbRanges) > 0)

	deliver := pres.OutputPath
	if deliver == "" {
		deliver = srcPath // measure-only/no-op: deliver the original source
	}
	c.embedMetadata(ctx, deliver, embedExt, a.video, eo, em)

	var explicit []cutrange.Range
	if req.Cut != nil {
		explicit = cutRanges(req.Cut.Ranges)
	}
	res := newProcessResult(SourceYouTube, pres, a.fmtSel, loudnessTarget(req.Loudness))
	res.VideoID = a.video.ID
	res.Title = a.video.Title
	res.Client = a.client
	res.SourceBytes = dlRes.BytesWritten
	res.SponsorBlockApplied = sponsorBlockContributed(explicit, sbRanges, pres)
	res.Metadata = videoMetadataFor(req, a.video)
	return deliver, res, nil
}

// collectRanges merges explicit removal ranges with any SponsorBlock segments,
// honoring the fetch timeout and OnError policy. It returns the combined ranges
// and, separately, the SponsorBlock-derived ranges (so the caller can set
// SponsorBlockApplied). A fetch failure is fatal only under FailDownload;
// otherwise it logs a ProceedUncut warning and continues.
func (c *Client) collectRanges(ctx context.Context, cs *CutSpec, videoID string, em *emitter) (all, sbRanges []cutrange.Range, err error) {
	if cs == nil {
		return nil, nil, nil
	}
	explicit := cutRanges(cs.Ranges)
	if cs.SponsorBlock == nil {
		return explicit, nil, nil
	}

	sbCtx, cancel := withTimeout(ctx, c.sponsorBlockTimeout(cs))
	defer cancel()
	segs, ferr := c.sb.FetchSegments(sbCtx, videoID, cs.SponsorBlock)
	if ferr != nil {
		if cs.OnError == FailDownload {
			return nil, nil, fmt.Errorf("waxtap: SponsorBlock fetch failed: %w", ferr)
		}
		em.warn(WarnProceedUncut, "SponsorBlock fetch failed; delivering uncut: "+ferr.Error())
		return explicit, nil, nil
	}
	if len(segs) == 0 {
		em.warn(WarnSponsorBlockEmpty, "SponsorBlock returned no segments")
		return explicit, nil, nil
	}

	sbRanges = cutrange.RangesFromSegments(segs)
	all = append(explicit, sbRanges...)
	return all, sbRanges, nil
}

// sponsorBlockTimeout resolves the SponsorBlock fetch timeout: the per-request
// CutSpec.Timeout takes precedence, then the SponsorBlock option, then the
// per-operation timeout. Zero means no extra deadline.
func (c *Client) sponsorBlockTimeout(cs *CutSpec) (d time.Duration) {
	switch {
	case cs.Timeout > 0:
		return cs.Timeout
	case c.opts.SponsorBlock.Timeout > 0:
		return c.opts.SponsorBlock.Timeout
	default:
		return c.opts.Timeouts.SponsorBlock
	}
}

// Stream acquires a single YouTube video and returns a reader for source-style
// delivery (pipe to disk or object storage). When processing is requested it
// stages and processes to a temp file first, then streams the result. Final byte
// counts are known only after the reader is drained and closed.
func (c *Client) Stream(ctx context.Context, req Request) (rc io.ReadCloser, info StreamInfo, err error) {
	em := newEmitter(req.Events, "")
	defer func() {
		if err != nil {
			em.failed(err)
		}
	}()

	id, err := youtube.ExtractVideoID(req.URL)
	if err != nil {
		return nil, StreamInfo{}, err
	}
	em.videoID = id
	if err = validateProcessSpec(req.ProcessSpec); err != nil {
		return nil, StreamInfo{}, err
	}
	// Report HTTP throttling as job warnings.
	ctx = httpx.WithThrottleHook(ctx, func(e httpx.ThrottleEvent) { emitThrottle(em, e) })

	// Processing stages the source and can retry. Keep-source streaming returns
	// bytes immediately and therefore uses one attempt.
	if needsProcessing(req.ProcessSpec) {
		return c.streamProcessed(ctx, req, id, em)
	}

	a, err := c.acquire(ctx, req, id, em)
	if err != nil {
		return nil, StreamInfo{}, err
	}
	em.stage(StageDownloading)
	body, sinfo, derr := a.transfer.stream(ctx, func(p download.Progress) {
		em.progress(p.BytesWritten, p.Total)
	})
	if derr != nil {
		return nil, StreamInfo{}, derr
	}
	info = StreamInfo{VideoID: id, Title: a.video.Title, Format: a.fmtSel, ContentLength: sinfo.ContentLength, Client: a.client}
	return &doneReader{ReadCloser: body, em: em}, info, nil
}

// streamProcessed stages and processes to a temp file, then returns a reader over
// the result that cleans up the temp directory and fires the terminal event on
// Close.
func (c *Client) streamProcessed(ctx context.Context, req Request, id string, em *emitter) (io.ReadCloser, StreamInfo, error) {
	jobDir, err := c.makeJobDir()
	if err != nil {
		return nil, StreamInfo{}, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(jobDir)
		}
	}()

	deliver, res, err := c.produce(ctx, req, id, jobDir, "", em)
	if err != nil {
		return nil, StreamInfo{}, err
	}

	f, err := os.Open(deliver)
	if err != nil {
		return nil, StreamInfo{}, err
	}
	info := StreamInfo{VideoID: id, Title: res.Title, Format: res.OutputFormat, ContentLength: fileSize(deliver), Client: res.Client}
	ok = true
	return &dirCleanupReader{File: f, dir: jobDir, em: em}, info, nil
}

// videoMetadataFor returns the requested result metadata, or nil when metadata
// was not requested. Chapters are populated only when Request.FullMetadata ran
// the watch-page pass that fills them.
func videoMetadataFor(req Request, v *youtube.Video) *VideoMetadata {
	if !req.IncludeMetadata || v == nil {
		return nil
	}
	return &VideoMetadata{
		Author:       v.Author,
		ChannelID:    v.ChannelID,
		Duration:     v.Duration,
		PublishDate:  v.PublishDate,
		Description:  v.Description,
		Availability: v.Availability,
		Chapters:     v.Chapters,
		Formats:      v.Formats,
	}
}

// watchPageMeta runs the watch-page metadata fetch under the extraction timeout.
// It is the shared fetch behind applyFullMetadata (download) and fullMetadataPass
// (Info), each of which keeps its own error policy.
func (c *Client) watchPageMeta(ctx context.Context, id string) (youtube.WatchPageMeta, error) {
	mctx, cancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	defer cancel()
	return c.yt.WatchPageMetadata(mctx, id)
}

// applyFullMetadata backfills the acquired video with watch-page chapters,
// publish date, and availability when Request.FullMetadata is set. It runs after
// a successful acquisition so an ingest gets full metadata in one call. It is
// best-effort: a failure leaves the base metadata (a completed download is never
// failed for an enrichment error). It is skipped without IncludeMetadata (which
// discards the result anyway), under NoFallback (which forbids the watch page),
// and when extraction already scraped the watch page.
func (c *Client) applyFullMetadata(ctx context.Context, req Request, a *acquired) {
	if !req.FullMetadata || !req.IncludeMetadata || req.NoFallback || a == nil || a.video == nil {
		return
	}
	if a.attempt == youtube.AttemptWatchPage {
		return // chapters and availability were already filled during extraction
	}
	meta, err := c.watchPageMeta(ctx, a.video.ID)
	if err != nil {
		c.log.DebugContext(ctx, "full-metadata watch-page pass failed; keeping base metadata", "err", err)
		return
	}
	mergeWatchPageMeta(a.video, meta)
}

// mergeWatchPageMeta merges a watch-page metadata pass into v: PublishDate only
// when v left it zero (the primary client may already carry it), plus Chapters
// and Availability. It is shared by the download and Info enrichment paths.
func mergeWatchPageMeta(v *youtube.Video, meta youtube.WatchPageMeta) {
	if v.PublishDate.IsZero() {
		v.PublishDate = meta.PublishDate
	}
	v.Chapters = meta.Chapters
	v.Availability = youtube.AvailabilityFromUnlisted(meta.Unlisted)
}

// loudnessTarget returns the target LUFS, or 0 when no loudness work is requested.
func loudnessTarget(l *LoudnessSpec) float64 {
	if l == nil {
		return 0
	}
	return l.Target
}

// streamErr records the first non-EOF read error returned after Stream hands a
// reader to the caller. Close uses it to emit the terminal event, since transfer
// failures usually surface in caller-owned Read calls.
type streamErr struct {
	mu  sync.Mutex
	err error
}

func (s *streamErr) record(err error) {
	if err == nil || errors.Is(err, io.EOF) {
		return
	}
	// Bytes already returned to the caller cannot be retried through another
	// client.
	if isIncompleteDelivery(err) {
		err = fmt.Errorf("%w: %v", ErrIncompleteStream, err)
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// terminal emits Done when the stream closed cleanly, or Failed with the first
// read error.
func (s *streamErr) terminal(em *emitter) {
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		em.failed(err)
		return
	}
	em.done()
}

// doneReader fires the terminal event once when closed, for the zero-disk
// streaming path: Done on a clean read-to-EOF, Failed if a read error occurred.
type doneReader struct {
	io.ReadCloser
	em   *emitter
	errs streamErr
	once sync.Once
}

func (r *doneReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.errs.record(err)
	return n, err
}

func (r *doneReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() { r.errs.terminal(r.em) })
	return err
}

// dirCleanupReader streams a processed temp file, removes its job directory, and
// fires the terminal event when closed (Failed if a read error occurred).
type dirCleanupReader struct {
	*os.File
	dir  string
	em   *emitter
	errs streamErr
	once sync.Once
}

func (r *dirCleanupReader) Read(p []byte) (int, error) {
	n, err := r.File.Read(p)
	r.errs.record(err)
	return n, err
}

func (r *dirCleanupReader) Close() error {
	err := r.File.Close()
	r.once.Do(func() {
		os.RemoveAll(r.dir)
		r.errs.terminal(r.em)
	})
	return err
}
