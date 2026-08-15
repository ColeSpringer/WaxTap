package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	// stats counts what this attempt's refresh callback did. It is nil for a SABR
	// transfer, which has no signed URL to refresh.
	stats *refreshStats
}

// refreshStats counts what one attempt's signed-URL refresh callback did, so a
// failed download can say how close it came and a successful one can say how much
// it needed. The two counts separate the failure shapes a single terminal error
// cannot: a session that ran out of rotations reports several, and a body that
// ended short without ever asking for a new URL reports none.
//
// The download layer serializes refresh callbacks (renew holds the shared-source
// lock) and the facade reads the counts after the transfer returns, so the mutex
// guards an ordering that happens to hold today rather than one the code relies
// on.
type refreshStats struct {
	mu        sync.Mutex
	attempts  int // refresh callbacks entered
	refreshes int // callbacks that returned a replacement Source
	rotations int // guest identities discarded
	// identityGen is the guest-identity generation currently behind this attempt's
	// URLs: the extraction's own to begin with, and the re-extraction's after each
	// refresh. A whole-chain retry has to rotate that one, not the generation the
	// attempt started on, which the attempt's own rotations already discarded.
	identityGen uint64
}

// begin notes that a refresh was asked for. The outcome is counted separately
// because a refresh that rotated the identity and then failed to re-extract is
// exactly the state these counters exist to expose, and counting only successes
// renders it identically to no refresh at all.
func (s *refreshStats) begin() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.attempts++
	s.mu.Unlock()
}

// recordRotation notes a discarded guest identity, when it happens rather than
// once the refresh around it succeeds.
func (s *refreshStats) recordRotation() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.rotations++
	s.mu.Unlock()
}

// recordRefresh notes a completed refresh and the generation its replacement URLs
// were minted under.
func (s *refreshStats) recordRefresh(gen uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.refreshes++
	s.identityGen = gen
	s.mu.Unlock()
}

// generation reports the guest-identity generation currently behind the attempt's
// URLs, and whether one is known (a SABR transfer refreshes no signed URL).
func (s *refreshStats) generation() (uint64, bool) {
	if s == nil {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identityGen, true
}

// counts reports the refreshes and identity rotations recorded so far.
func (s *refreshStats) counts() (refreshes, rotations int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshes, s.rotations
}

// String renders the counts for a diagnostic line, or "" when no refresh was ever
// asked for, which is itself the distinguishing signal. A refresh that was asked
// for and failed shows as a shortfall against the attempt count.
func (s *refreshStats) String() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d refreshes, %d session rotations", s.refreshes, s.attempts, s.rotations)
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
	// Both branches record the identity the attempt runs under, SABR included: it
	// refreshes no signed URL, but a whole-chain retry still has to know which
	// identity to discard, and a zero there rotates nothing while reporting success.
	a.stats = &refreshStats{identityGen: ext.IdentityGeneration()}

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
	a.transfer = urlTransfer{dl: c.dl, src: toSource(*plan.Direct), refresh: c.directRefresh(req, id, target, ext, selFmt.Itag, plan.Direct.ExpiresAt, em, a.stats)}
	return a, nil
}

// warnClientSubstitution reports a successful WEB watch-page fallback.
func (c *Client) warnClientSubstitution(em *emitter, a *acquired) {
	if a.substitutedFrom != "" {
		em.warn(WarnFallbackProfile, fmt.Sprintf("forced client %s failed; used WEB through the watch-page fallback", a.substitutedFrom))
	}
}

// sessionRotateMargin separates a delivery-cap 403 from an expiry 403 in
// directRefresh. googlevideo caps stream delivery per guest identity for a
// small share of sessions (empty-body 403 on any read past roughly 1 MB); those
// rejections arrive seconds after the URL is minted, while expiry rejections
// arrive at or after the signed expire time (typically six hours out). A 403
// with this much lifetime left is treated as the cap. Misreads are cheap in
// both directions: rotation still re-resolves the URL, it just also pays one
// homepage bootstrap.
//
// The comparison holds the local wall clock against the server-signed expire
// time. A clock running behind classifies some true expiries as the cap and
// pays that same bootstrap; only a clock running further ahead than the URL
// lifetime (~6 h) would suppress rotation and give capped runs back their
// pre-fix failure. That pathology is accepted rather than engineered around.
const sessionRotateMargin = 5 * time.Minute

// capSuspected reports whether a stream failure looks like the per-session
// delivery cap rather than URL expiry: an empty-body 403 answered while the
// current URL still had comfortable lifetime left. The empty body is the cap's
// measured signature (every observed cap rejection carried zero bytes); a 403
// with a body is something else explaining itself, typically a proxy or
// middlebox block page, which no amount of identity rotation fixes. A 410 is a
// dead URL, never the cap. An unknown expiry counts as suspected, since a
// genuine expiry cannot be established either.
func capSuspected(failure *potoken.HTTPFailure, expiresAt time.Time) bool {
	if failure == nil || failure.StatusCode != http.StatusForbidden || failure.Body != "" {
		return false
	}
	return expiresAt.IsZero() || time.Until(expiresAt) > sessionRotateMargin
}

// directRefresh builds a signed-URL refresh callback pinned to the original
// extraction's attempt and itag. Pinning prevents a resumed range from mixing
// bytes from different encodings.
//
// expiresAt is the expiry of the currently live URL; the closure keeps it and
// the extraction's identity generation current across refreshes, so a 403 can
// be classified as cap-vs-expiry and a rotation discards only the identity
// that minted the failing URL. The download layer serializes refresh callbacks
// (renew runs under the shared source lock), so plain assignment is safe.
func (c *Client) directRefresh(req Request, id string, target format.Target, ext *youtube.Extraction, pinnedItag int, expiresAt time.Time, em *emitter, stats *refreshStats) download.RefreshFunc {
	attempt := ext.Attempt()
	lastExpiry := expiresAt
	identityGen := ext.IdentityGeneration()
	return func(fctx context.Context, failure *potoken.HTTPFailure) (download.Source, error) {
		// An empty-body 403 on a URL nowhere near expiry is the per-session
		// delivery cap. Re-signing the same identity is measured futile there
		// (every fresh URL inherits the cap), so discard the identity and
		// re-extract fresh. An adopted session is retired through its provider;
		// one that cannot be retired takes the plain re-resolve.
		stats.begin()
		rotated := capSuspected(failure, lastExpiry) && c.yt.RotateIdentity(fctx, identityGen, id)
		if rotated {
			stats.recordRotation()
		}
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
		lastExpiry = nplan.Direct.ExpiresAt
		identityGen = rext.IdentityGeneration()
		stats.recordRefresh(identityGen)
		if rotated {
			em.warn(WarnSessionRotated, "the server rejected a stream URL well before its expiry; continuing with a new session")
		} else {
			em.warn(WarnURLReResolved, "stream URL re-resolved after expiry")
		}
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

// maxChainPasses bounds how many times acquireAndDownload runs the whole client
// chain. The second pass exists because the per-download refresh budget is not
// the only thing standing between a capped session and a complete file: measured
// live, successful runs spend up to the full budget of identity rotations, and
// the failures sit exactly at it on every client. Rotation demonstrably recovers
// runs, so the chain is given one more set of them on a fresh session rather than
// the budget being raised, which would spend the extra re-resolves on a URL that
// is genuinely dead as readily as on one that is capped.
//
// One extra pass, not a loop: a pass that delivers nothing still costs a full
// re-extract and re-resolve per client, and the passes are not independent, since
// a capped window can outlast them both.
const maxChainPasses = 2

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
	// lastIdentityGen is the guest identity behind the most recent delivery
	// attempt, which is the one a whole-chain retry has to discard.
	var lastIdentityGen uint64
	pass := 1
	causes.pass = pass

	// nextPass decides whether the exhausted chain gets one more run on a fresh
	// session, and prepares for it. Rotation is the precondition, not a nicety: if
	// the identity behind the failing URLs cannot be discarded (a jarless client,
	// a fixed Session, a provider that cannot retire what it handed out), a second
	// pass re-resolves under the same capped identity and repeats the first
	// exactly. See maxChainPasses.
	nextPass := func() bool {
		if pass >= maxChainPasses || req.NoFallback || !causes.allDeliveriesIncomplete() {
			return false
		}
		if !c.yt.RotateIdentity(ctx, lastIdentityGen, id) {
			return false
		}
		pass++
		causes.pass = pass
		skip = baseSkip(req)
		// The attested WEB context does not get a second pass. It had its one
		// fresh-context retry and, if that capped too, its session was already
		// reported to the provider; running it again would re-report and re-retry
		// past the "second cap, then stop" the provider path is built on.
		skip[youtube.AttemptWebContext] = true
		em.warn(WarnIncompleteFallback, "every client that reached the stream returned an incomplete delivery; retrying the chain once on a fresh session")
		return true
	}

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
				if nextPass() {
					continue
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
		// Whatever happened, this attempt's URLs were minted under an identity a
		// whole-chain retry would have to discard.
		if gen, ok := a.stats.generation(); ok {
			lastIdentityGen = gen
		}
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
			// The refresh counts ride the happy path too: a run that needed two
			// rotations to finish is one rotation from the failures this breadcrumb
			// exists to explain.
			refreshes, rotations := a.stats.counts()
			c.log.DebugContext(ctx, "download complete",
				"client", a.client, "itag", a.fmtSel.Itag, "bytes", res.BytesWritten,
				"refreshes", refreshes, "rotations", rotations, "chainPasses", pass)
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
				// No whole-chain retry here: this attempt reached the transfer and
				// failed on availability or a missing token, which a fresh session does
				// not change. Asking would be dead code anyway, since the cause just
				// recorded is a delivery that was not incomplete.
				causes.addDelivered(a, derr)
				break
			}
			return nil, download.Result{}, "", derr
		}
		// Record the cap (a non-retrying first/only cap, or any non-web-context cap)
		// so a later retry that fails to re-extract still surfaces ErrIncompleteStream
		// in the aggregate. The retry's own second cap re-enters here with
		// webContextRetried set and is skipped to avoid a duplicate "tried" entry.
		if a.attempt != youtube.AttemptWebContext || !webContextRetried {
			causes.addDelivered(a, derr)
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
		// The fresh context capped too, so the provider's session itself is
		// suspect, not the individual context. Report it so the provider retires
		// the session and later downloads mint contexts from a fresh one; this
		// download proceeds to the fallback chain either way.
		if a.attempt == youtube.AttemptWebContext && webContextRetried {
			if st, ok := a.transfer.(sabrTransfer); ok && c.yt.ReportPlayerContext(ctx, st.handle.ContextGeneration(), id) {
				webCtxReason = "stream capped twice; the session was reported to the provider"
			}
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
	// A cancellation that ended the loop needs no check here: both callers run
	// under Download or Stream, whose defers reclassify the aggregate and keep it
	// as detail.
	return nil, download.Result{}, "", causes.aggregate()
}

// attemptErrors collects failures from a cross-client download.
type attemptErrors struct {
	causes []attemptCause
	// pass is the chain pass causes are being recorded for, so a second pass over
	// the same clients is distinguishable from one longer pass.
	pass int
}

type attemptCause struct {
	id   youtube.AttemptID
	err  error
	pass int
	// delivered marks a cause from an attempt that reached the transfer, as
	// opposed to one that failed to start.
	delivered bool
	// stats is the attempt's refresh accounting, rendered, or "" when the failure
	// happened before delivery or the transfer had no signed URL to refresh.
	stats string
}

func (a *attemptErrors) add(id youtube.AttemptID, err error) {
	a.causes = append(a.causes, attemptCause{id: id, err: err, pass: a.pass})
}

// addDelivered records a failure that reached the transfer, so the cause carries
// how many stream-URL refreshes and session rotations the attempt spent getting
// there.
func (a *attemptErrors) addDelivered(acq *acquired, err error) {
	a.causes = append(a.causes, attemptCause{
		id: acq.attempt, err: err, pass: a.pass, delivered: true, stats: acq.stats.String(),
	})
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

// allDeliveriesIncomplete reports whether every attempt that reached the transfer
// ended in an incomplete delivery, and that at least one did. It is the gate on
// retrying the whole chain: the failure it describes is transport-shaped, so a
// fresh session can plausibly fix it, while any other delivery failure means a
// second pass would only repeat itself.
//
// Attempts that never reached the transfer are skipped rather than counted
// against it. On the default chain they are the norm, not the exception: WEB and
// WEB_EMBEDDED_PLAYER stop at "requires a player PO token" with no provider
// configured, so requiring every recorded cause to be incomplete would mean this
// never fires on the one chain that needs it. Those attempts did not fail to
// deliver, they failed to start, and another pass changes nothing about that.
func (a *attemptErrors) allDeliveriesIncomplete() bool {
	seen := false
	for _, c := range a.causes {
		if !c.delivered {
			continue
		}
		if !isIncompleteDelivery(c.err) {
			return false
		}
		seen = true
	}
	return seen
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
	switch {
	case isIncompleteDelivery(best):
		// A refresh failure is an incomplete file delivery once all attempts fail.
		return &IncompleteDeliveryError{Attempts: a.rendered(), Err: best}
	case len(tried) == 0:
		return best
	default:
		return fmt.Errorf("%w (tried %s)", best, strings.Join(tried, ", "))
	}
}

// rendered describes every recorded cause, one line per attempt, for
// [IncompleteDeliveryError.Attempts].
func (a *attemptErrors) rendered() []string {
	out := make([]string, 0, len(a.causes))
	for _, c := range a.causes {
		label := string(c.id)
		if label == "" {
			label = "chain" // no single attempt could be blamed
		}
		if c.pass > 1 {
			label += fmt.Sprintf(" (pass %d)", c.pass)
		}
		// Redacted here rather than at the CLI: the text ends up in a playlist run's
		// NDJSON error field as well as in the terminal message, and a cause can carry
		// a signed stream URL.
		line := label + ": " + redactURLsIn(c.err.Error())
		if c.stats != "" {
			line += " [" + c.stats + "]"
		}
		out = append(out, line)
	}
	return out
}

// IncompleteDeliveryError reports that no extraction attempt delivered a complete
// stream, and carries what every attempt tried and how it ended.
//
// The chain's single preferred cause is what classification needs, but it is not
// what diagnosis needs. [waxerr.PreferErr] ranks ErrIncompleteStream and
// ErrURLExpired equally and keeps the first, so an ANDROID_VR chunk that ended
// short followed by an IOS attempt that spent its refresh budget keeps only the
// short chunk, dropping the one fact that separates a truncated body from an
// exhausted budget. Attempts keeps all of them.
type IncompleteDeliveryError struct {
	// Attempts is one rendered line per extraction attempt, in the order they were
	// tried, as "<attempt>: <cause>", with the attempt's stream-URL refresh and
	// session-rotation counts in brackets when it reached delivery. An attempt no
	// single extraction can be blamed for is labeled "chain".
	//
	// The text can contain a signed stream URL: the chain wraps arbitrary causes,
	// including net/http's own *url.Error. Redact before display.
	Attempts []string
	// Err is the preferred cause, the one chosen for classification. Unwrap
	// reports it alongside ErrIncompleteStream, so errors.Is classification, and
	// with it the CLI exit code, is what the plainly wrapped error always gave.
	Err error
}

func (e *IncompleteDeliveryError) Error() string {
	const summary = "no attempted client delivered a complete stream"
	if len(e.Attempts) == 0 {
		return summary
	}
	return summary + " (" + strings.Join(e.Attempts, "; ") + ")"
}

func (e *IncompleteDeliveryError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrIncompleteStream}
	}
	return []error{ErrIncompleteStream, e.Err}
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

// cancelCause reclassifies a terminal error as the caller's cancellation, keeping
// the original as message detail. Errors that already report the cancellation are
// left alone. It covers the transfer paths (Download, Stream, and the readers
// Stream hands back); Info and Enumerate report their own errors unchanged.
//
// It wraps ctx.Err() rather than joining, so a caller whose retry logic checks
// ErrIncompleteStream first does not retry a download the user canceled. The
// CLI's finalError deliberately joins instead; see its comment.
func cancelCause(ctx context.Context, err error) error {
	ce := ctx.Err()
	if err == nil || ce == nil || errors.Is(err, ce) {
		return err
	}
	return fmt.Errorf("%w: %v", ce, err)
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
	// Substitute before the terminal event so the event and the returned error
	// agree on a canceled run.
	defer func() {
		err = cancelCause(ctx, err)
		em.finish(res, err)
	}()

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
//
// Both branches set OutputFormat.ContentLength from the bytes delivered rather
// than leaving the player's declaration, matching the processing path: a format
// whose player response omits contentLength delivers fine but would otherwise
// report zero. SourceFormat keeps the declared value, which is what it describes.
func (c *Client) deliverSource(ctx context.Context, req Request, id string, em *emitter) (*Result, error) {
	switch req.Output.kind {
	case outputFile:
		a, r, _, err := c.acquireAndDownload(ctx, req, id, em, func(*acquired) string { return req.Output.path })
		if err != nil {
			return nil, err
		}
		em.stage(StageFinalizing)
		out := a.fmtSel
		out.ContentLength = r.BytesWritten
		return &Result{
			SourceKind:   SourceYouTube,
			VideoID:      id,
			Title:        a.video.Title,
			Client:       a.client,
			SourceFormat: a.fmtSel,
			OutputFormat: out,
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
		out := a.fmtSel
		out.ContentLength = r.BytesWritten
		return &Result{
			SourceKind:   SourceYouTube,
			VideoID:      id,
			Title:        a.video.Title,
			Client:       a.client,
			SourceFormat: a.fmtSel,
			OutputFormat: out,
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
	// contentLength describes the file actually delivered. The pipeline's output
	// probe runs before embedMetadata rewrites the file, and the embed-only branch
	// in produce has no probe at all, so the probe size can undercount cover art.
	// OutputBytes is stat'd above, after every write, so it is the authority.
	// Client.Process overlays the same way after its tag-carry pass.
	// Bitrate is deliberately left alone; applyProbe takes the audio stream's own
	// rate, so it keeps describing the audio rather than growing with the artwork.
	// The consequence is intentional: after an embed pass contentLength and bitrate
	// no longer satisfy size * 8 / duration.
	if res.OutputBytes > 0 {
		res.OutputFormat.ContentLength = res.OutputBytes
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

	eo := embedOptions{thumbnail: req.EmbedThumbnail, metadata: req.EmbedMetadata, coverArt: req.CoverArt}
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
	warnLoudnessTargetMissed(em, req.Loudness, pres)
	warnImplicitDownmix(em, req.ProcessSpec, pres)

	deliver := pres.OutputPath
	if deliver == "" {
		deliver = srcPath // measure-only/no-op: deliver the original source
	}
	// A rendered cut moved every later chapter mark; the embed pass remaps them
	// onto the delivered timeline.
	eo.cut = appliedCutFrom(pres)
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
	// Substitute before the terminal event so the event and the returned error
	// agree on a canceled run.
	defer func() {
		if err != nil {
			err = cancelCause(ctx, err)
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
	return &doneReader{ReadCloser: body, ctx: ctx, em: em}, info, nil
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
	return &dirCleanupReader{File: f, dir: jobDir, ctx: ctx, em: em}, info, nil
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
// failed for an enrichment error). It is skipped without IncludeMetadata or
// EmbedMetadata (nothing consumes the result), under NoFallback (which forbids
// the watch page), and when extraction already scraped the watch page.
func (c *Client) applyFullMetadata(ctx context.Context, req Request, a *acquired) {
	if !req.FullMetadata || (!req.IncludeMetadata && !req.EmbedMetadata) || req.NoFallback || a == nil || a.video == nil {
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

// mergeWatchPageMeta merges a watch-page metadata pass into v. The pass backfills
// what extraction left empty and never replaces it, so PublishDate and Chapters
// are filled only when v carries none. Availability is unconditional because this
// pass is its only source. It is shared by the download and Info enrichment paths.
func mergeWatchPageMeta(v *youtube.Video, meta youtube.WatchPageMeta) {
	if v.PublishDate.IsZero() {
		v.PublishDate = meta.PublishDate
	}
	if len(v.Chapters) == 0 {
		v.Chapters = meta.Chapters
	}
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

// record notes a read error. ctx is the request context the reader was built
// with and is required: Stream has already returned by the time a read fails, so
// the facade defer can never see this error and the terminal event would
// otherwise be the one thing still reporting a transfer failure for a job the
// user canceled.
func (s *streamErr) record(ctx context.Context, err error) {
	if err == nil || errors.Is(err, io.EOF) {
		return
	}
	// Bytes already returned to the caller cannot be retried through another
	// client.
	if isIncompleteDelivery(err) {
		err = fmt.Errorf("%w: %v", ErrIncompleteStream, err)
	}
	// cancelCause wraps with %v, so a canceled run reports the cancellation and
	// the sentinel just applied stops matching, exactly as at the facade.
	err = cancelCause(ctx, err)
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
	// ctx is the request context, held because Read carries none and a read that
	// fails after cancellation must report the cancellation. Required.
	ctx  context.Context
	em   *emitter
	errs streamErr
	once sync.Once
}

func (r *doneReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.errs.record(r.ctx, err)
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
	dir string
	// ctx is the request context; see doneReader.ctx.
	ctx  context.Context
	em   *emitter
	errs streamErr
	once sync.Once
}

func (r *dirCleanupReader) Read(p []byte) (int, error) {
	n, err := r.File.Read(p)
	r.errs.record(r.ctx, err)
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
