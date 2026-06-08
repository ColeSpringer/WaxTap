// Package sabr streams YouTube SABR audio over UMP. SABR-backed formats omit
// per-format URLs; clients repeatedly POST VideoPlaybackAbrRequest messages to
// serverAbrStreamingUrl and assemble the returned init and media segments.
//
// Each request depends on state returned by the previous response, so SABR
// streams are fetched sequentially rather than through the parallel chunk
// downloader.
package sabr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// Defaults applied by Open when a Config field is left zero.
const (
	defaultRoundTimeout = 30 * time.Second
	defaultMaxBackoff   = 30 * time.Second

	// maxEmptyRounds bounds consecutive rounds without new media before the
	// stream is treated as stalled.
	maxEmptyRounds = 2
	// maxContextRounds bounds consecutive rounds whose only progress is a changed
	// SABR context (no media), so a server that keeps mutating contexts without
	// sending media still trips the stall guard. Media resets the count.
	maxContextRounds = 8
	// maxRedirects bounds SABR_REDIRECT hops in one Open.
	maxRedirects = 5
	// maxRoundBytes caps a single SABR response body. Audio rounds are normally
	// much smaller.
	maxRoundBytes = 64 << 20

	// statusAttestationPending is the lowest StreamProtectionStatus.status that
	// indicates the PO token has not been accepted (2=PENDING, 3=REQUIRED).
	statusAttestationPending = 2

	// maxSabrContexts caps the number of distinct context types stored so a
	// misbehaving server cannot grow the maps without limit. activeContextTypes
	// stays a subset of the stored types (see applyContextPolicy), so one cap
	// bounds both. YouTube sends 1-3.
	maxSabrContexts = 64
)

// ErrReloadPlayer signals a RELOAD_PLAYER_RESPONSE part. Callers should fetch a
// new /player response and rebuild Config before retrying. The youtube package
// handles this error internally.
var ErrReloadPlayer = errors.New("sabr: reload player response")

// HTTPDoer is the HTTP operation SABR requires.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Progress reports the number of bytes emitted by the stream.
type Progress struct {
	BytesWritten int64
	Total        int64
}

// ProgressFunc receives best-effort byte progress. Read calls it synchronously,
// so implementations should return quickly.
type ProgressFunc func(Progress)

// StreamInfo is the response metadata returned alongside the reader.
type StreamInfo struct {
	ContentLength int64
	ContentType   string
}

// Config contains the normalized values needed to open a SABR stream.
// ServerAbrURL must already have its n parameter resolved. UstreamerConfig and
// POToken must be decoded from base64 and base64url, respectively.
type Config struct {
	// HTTP performs the SABR POSTs. Required.
	HTTP HTTPDoer
	// Logger receives debug and warning logs. Nil discards them.
	Logger *slog.Logger
	// ServerAbrURL is the descrambled serverAbrStreamingUrl. Required.
	ServerAbrURL string
	// UstreamerConfig is the decoded videoPlaybackUstreamerConfig sent in every
	// request.
	UstreamerConfig []byte
	// Format selects the audio encoding to stream.
	Format FormatId
	// ClientInfo is the wire identity sent in streamerContext.client_info.
	ClientInfo ClientInfo
	// UserAgent is the HTTP User-Agent for the SABR POST; it must match the
	// client that extracted the formats.
	UserAgent string
	// POToken is the base64url-decoded GVS-scope PO token sent as
	// streamerContext.po_token.
	POToken []byte
	// ContentLength sets StreamInfo.ContentLength and the progress total from the
	// player response. Zero means unknown.
	ContentLength int64
	// RoundTimeout bounds one request/response round. Zero uses the default.
	RoundTimeout time.Duration
	// MaxBackoff clamps server-directed backoff. Zero uses the default.
	MaxBackoff time.Duration
	// DescrambleN resolves the throttling n parameter of a SABR_REDIRECT URL. Nil
	// follows redirects unchanged.
	DescrambleN func(ctx context.Context, rawURL string) (string, error)
}

// Open starts a SABR stream and returns a reader over the reassembled audio.
// It performs the first request before returning, so initial protocol and
// authentication failures are returned from Open. Later failures are returned
// from Read. Closing the reader cancels all requests and backoff waits.
func Open(ctx context.Context, cfg Config, progress ProgressFunc) (io.ReadCloser, StreamInfo, error) {
	if cfg.HTTP == nil {
		return nil, StreamInfo{}, fmt.Errorf("%w: sabr: nil HTTP client", waxerr.ErrExtractionFailed)
	}
	if cfg.ServerAbrURL == "" {
		return nil, StreamInfo{}, fmt.Errorf("%w: sabr: empty serverAbrStreamingUrl", waxerr.ErrExtractionFailed)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &stream{
		cfg:                cfg,
		log:                log,
		progress:           progress,
		ctx:                sctx,
		cancel:             cancel,
		serverURL:          cfg.ServerAbrURL,
		segments:           make(map[uint64]mediaSegment),
		contentLen:         cfg.ContentLength,
		sabrContexts:       make(map[int32][]byte),
		activeContextTypes: make(map[int32]bool),
	}
	if err := s.prime(); err != nil {
		cancel()
		return nil, StreamInfo{}, err
	}
	return s, StreamInfo{ContentLength: s.contentLen, ContentType: s.contentType}, nil
}

// stream is the SABR reader. Only one goroutine may call Read; Close may be
// called from another goroutine to cancel the stream.
type stream struct {
	cfg      Config
	log      *slog.Logger
	progress ProgressFunc

	ctx    context.Context
	cancel context.CancelFunc

	serverURL      string
	playbackCookie []byte
	redirects      int
	emptyRounds    int
	contextRounds  int

	// SABR context state. sabrContexts holds the value last received for each
	// context type; activeContextTypes is the subset currently echoed back to the
	// server. The server seeds and updates both via parts 57/59; see
	// applyContextUpdates.
	sabrContexts       map[int32][]byte
	activeContextTypes map[int32]bool

	// Reassembly state. The init segment is emitted first, followed by contiguous
	// media segments. segments buffers media received ahead of a gap.
	initBytes    []byte
	initWritten  bool
	segments     map[uint64]mediaSegment
	firstSeq     uint64
	nextSeq      uint64
	seqInit      bool
	endSegment   uint64
	contentType  string
	downloadedMs int64

	pending      []byte // emitted, not yet read
	bytesWritten int64
	contentLen   int64

	done bool
	err  error
}

// mediaSegment holds a segment's bytes and duration. The duration contributes to
// downloadedMs only after the segment is emitted.
type mediaSegment struct {
	data     []byte
	duration int64
}

func (s *stream) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		s.fill()
	}
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

// Close cancels the stream context, aborting any in-flight round or backoff.
func (s *stream) Close() error {
	s.cancel()
	return nil
}

// prime runs rounds until the first bytes are ready, the stream completes, or an
// error occurs.
func (s *stream) prime() error {
	s.fill()
	return s.err
}

// fill runs SABR rounds until there is something to read, the stream is done, or
// it fails.
func (s *stream) fill() {
	for len(s.pending) == 0 && !s.done && s.err == nil {
		if err := s.ctx.Err(); err != nil {
			s.err = err
			return
		}
		if err := s.round(); err != nil {
			s.err = err
			return
		}
	}
}

// round performs one request/response cycle: POST, consume the UMP body, and
// integrate any new segments. It returns errors for terminal protocol signals,
// HTTP failures, and stalls.
func (s *stream) round() error {
	body, err := s.post()
	if err != nil {
		return err
	}
	res, err := s.consume(body)
	if err != nil {
		return err
	}
	if res.signal != nil {
		return res.signal
	}
	if res.policy != nil && len(res.policy.PlaybackCookie) > 0 {
		s.playbackCookie = res.policy.PlaybackCookie
	}
	// Apply SABR context updates before the redirect/progress checks so an update
	// is never dropped. changed reports whether the next request will differ.
	changed := s.applyContextUpdates(res.contextUpdates, res.contextPolicy)
	if res.redirect != "" {
		s.redirects++
		if s.redirects > maxRedirects {
			return fmt.Errorf("%w: too many SABR redirects", waxerr.ErrExtractionFailed)
		}
		s.serverURL = res.redirect
		s.emptyRounds = 0
		return nil // re-POST to the new endpoint without advancing state
	}

	emitted, advanced := s.integrate(res)
	if s.complete() {
		s.done = true
		return nil
	}
	// Backoff means the server is pacing the stream, not that it has stalled.
	// Buffering a segment ahead of a gap also counts as media progress.
	waiting := res.policy != nil && res.policy.BackoffTimeMs > 0
	switch {
	case emitted > 0 || advanced:
		// Media progress clears both stall guards.
		s.emptyRounds = 0
		s.contextRounds = 0
	case changed:
		// A changed context makes the next request differ (it now echoes the
		// context), so the round isn't empty and we re-POST, but cap these so
		// context churn without media still stalls instead of looping forever.
		s.emptyRounds = 0
		s.contextRounds++
		if s.contextRounds >= maxContextRounds {
			return s.stallResult()
		}
	case waiting:
		// Honor the backoff without counting an empty round.
	default:
		s.emptyRounds++
		if s.emptyRounds >= maxEmptyRounds {
			return s.stallResult()
		}
	}
	if waiting {
		return s.sleep(clampBackoff(res.policy.BackoffTimeMs, s.maxBackoff()))
	}
	return nil
}

// roundResult is the decoded content of one SABR response body. UMP header ids
// are scoped to a single response, so headers and media are matched per round: a
// MEDIA part always shares its response with the MediaHeader it references.
type roundResult struct {
	headers     map[uint32]*MediaHeader
	media       map[uint32][]byte
	mediaOrder  []uint32 // header ids in arrival order, for stable processing
	policy      *NextRequestPolicy
	redirect    string
	signal      error
	endSegment  uint64
	contentType string

	// SABR context signals, applied once per round in round(). contextUpdates
	// are part-57 blobs to store/echo; contextPolicy is the part-59 start/stop/
	// discard directive.
	contextUpdates []SabrContextUpdate
	contextPolicy  *SabrContextSendingPolicy
}

// consume parses one UMP response body into a roundResult. A terminal signal
// (attestation/reload/SABR error) short-circuits the rest of the body.
func (s *stream) consume(body []byte) (roundResult, error) {
	res := roundResult{
		headers: make(map[uint32]*MediaHeader),
		media:   make(map[uint32][]byte),
	}
	r := newUMPReader(body)
	for {
		part, ok, err := r.next()
		if err != nil {
			return res, wrapExtraction(err)
		}
		if !ok {
			return res, nil
		}
		switch part.Type {
		case partMediaHeader:
			h, err := unmarshalMediaHeader(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			hh := h
			res.headers[h.HeaderID] = &hh
		case partMedia:
			id, media, err := leadingVarint(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			hid := uint32(id)
			if _, seen := res.media[hid]; !seen {
				res.mediaOrder = append(res.mediaOrder, hid)
			}
			res.media[hid] = append(res.media[hid], media...)
		case partMediaEnd:
			// Advisory: segments are finalized at the end of the round.
		case partFormatInitMetadata:
			m, err := unmarshalFormatInitMetadata(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			if m.EndSegmentNumber > 0 {
				res.endSegment = uint64(m.EndSegmentNumber)
			}
			if m.MimeType != "" {
				res.contentType = m.MimeType
			}
		case partNextRequestPolicy:
			p, err := unmarshalNextRequestPolicy(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			res.policy = &p
		case partSabrRedirect:
			rd, err := unmarshalSabrRedirect(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			url, err := s.descramble(rd.URL)
			if err != nil {
				return res, err
			}
			res.redirect = url
		case partStreamProtection:
			st, err := unmarshalStreamProtectionStatus(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			if st.Status >= statusAttestationPending {
				res.signal = fmt.Errorf("%w: SABR attestation required (status %d)", waxerr.ErrNeedsPOToken, st.Status)
				return res, nil
			}
		case partSabrError:
			se, err := unmarshalSabrError(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			res.signal = fmt.Errorf("%w: SABR error type=%q code=%d", waxerr.ErrExtractionFailed, se.Type, se.Code)
			return res, nil
		case partReloadPlayerResp:
			res.signal = ErrReloadPlayer
			return res, nil
		case partSabrContextUpdate:
			u, err := unmarshalSabrContextUpdate(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			res.contextUpdates = append(res.contextUpdates, u)
		case partSabrContextSendPol:
			p, err := unmarshalSabrContextSendingPolicy(part.Payload)
			if err != nil {
				return res, wrapExtraction(err)
			}
			res.contextPolicy = &p
		default:
			// Unknown part; the UMP reader already skipped its payload by size.
		}
	}
}

// integrate buffers one response's segments and moves contiguous bytes into
// pending. advanced reports whether the response supplied any new segment.
func (s *stream) integrate(res roundResult) (emitted int, advanced bool) {
	if res.endSegment > 0 {
		s.endSegment = res.endSegment
	}
	if res.contentType != "" {
		s.contentType = res.contentType
	}
	for _, hid := range res.mediaOrder {
		h := res.headers[hid]
		data := res.media[hid]
		if h == nil {
			s.log.WarnContext(s.ctx, "sabr: media bytes without a header", "header_id", hid)
			continue
		}
		if h.IsInitSeg {
			if s.initBytes == nil {
				s.initBytes = data
				advanced = true
			}
			continue
		}
		seq := h.SequenceNumber
		if s.seqInit && seq < s.firstSeq {
			// The server sent a segment before the sequence where this stream began.
			s.log.WarnContext(s.ctx, "sabr: segment below the stream start; dropping", "seq", seq, "first_seq", s.firstSeq)
			continue
		}
		if s.seqInit && seq < s.nextSeq {
			continue // already emitted (e.g. a redirect/reload re-sent it)
		}
		if _, exists := s.segments[seq]; exists {
			continue // duplicate within the buffer; do not double-count duration
		}
		s.segments[seq] = mediaSegment{data: data, duration: h.DurationMs}
		advanced = true
	}
	if !s.seqInit && len(s.segments) > 0 {
		// The first request has no buffered range, so its lowest sequence number
		// anchors the stream.
		s.nextSeq = minKey(s.segments)
		s.firstSeq = s.nextSeq
		s.seqInit = true
	}
	return s.drain(), advanced
}

// drain moves the init segment and contiguous media into pending. It holds media
// until the init segment arrives so the output remains a valid container.
func (s *stream) drain() int {
	emitted := 0
	if !s.initWritten {
		if s.initBytes == nil {
			return 0 // the init segment must lead; hold media until it arrives
		}
		s.pending = append(s.pending, s.initBytes...)
		emitted += len(s.initBytes)
		s.initWritten = true
	}
	for {
		seg, ok := s.segments[s.nextSeq]
		if !ok {
			break
		}
		s.pending = append(s.pending, seg.data...)
		emitted += len(seg.data)
		s.downloadedMs += seg.duration
		delete(s.segments, s.nextSeq)
		s.nextSeq++
	}
	if emitted > 0 {
		s.bytesWritten += int64(emitted)
		if s.progress != nil {
			s.progress(Progress{BytesWritten: s.bytesWritten, Total: s.contentLen})
		}
	}
	return emitted
}

// complete reports whether the init segment and every segment through the
// declared end have been emitted. Content length is not a completion signal
// because the player may under-report it.
func (s *stream) complete() bool {
	return s.initWritten && s.endSegment > 0 && s.seqInit && s.nextSeq > s.endSegment
}

// stallResult returns an error when the stream is detectably incomplete. Without
// an end segment or content length, an exhausted stream is treated as complete.
func (s *stream) stallResult() error {
	switch {
	case !s.initWritten:
		return fmt.Errorf("%w: SABR stream stalled before delivering an init segment", waxerr.ErrExtractionFailed)
	case s.endSegment > 0 && s.nextSeq <= s.endSegment:
		return fmt.Errorf("%w: SABR stream stalled at segment %d of %d", waxerr.ErrExtractionFailed, s.nextSeq, s.endSegment)
	case len(s.segments) > 0:
		return fmt.Errorf("%w: SABR stream stalled with %d undelivered segments", waxerr.ErrExtractionFailed, len(s.segments))
	case s.contentLen > 0 && s.bytesWritten < s.contentLen:
		return fmt.Errorf("%w: SABR stream stalled after %d of %d bytes", waxerr.ErrExtractionFailed, s.bytesWritten, s.contentLen)
	case s.bytesWritten == 0:
		return fmt.Errorf("%w: SABR stream stalled before delivering any media", waxerr.ErrExtractionFailed)
	}
	// No available metadata proves that more data is expected.
	s.done = true
	return nil
}

// applyContextUpdates folds part-57 updates and a part-59 policy into the stored
// and active context maps. It returns whether the outgoing request state changed:
// a changed state means the next request carries a new/updated context, so the
// caller treats it as progress and re-POSTs. An identical re-send returns false,
// so the empty-round guard still bounds a stuck stream.
func (s *stream) applyContextUpdates(updates []SabrContextUpdate, policy *SabrContextSendingPolicy) bool {
	changed := false
	for _, u := range updates {
		if !u.HasType || len(u.Value) == 0 {
			s.log.DebugContext(s.ctx, "sabr: ignoring context update without type/value",
				"has_type", u.HasType, "value_len", len(u.Value))
			continue
		}
		existing, stored := s.sabrContexts[u.Type]
		if !stored && len(s.sabrContexts) >= maxSabrContexts {
			s.log.WarnContext(s.ctx, "sabr: context type cap reached; dropping update",
				"type", u.Type, "cap", maxSabrContexts)
			continue
		}
		// KEEP_EXISTING governs only the value write; the send_by_default
		// activation below still applies, so a server can start sending a context
		// it asked us not to overwrite.
		keepValue := u.WritePolicy == writePolicyKeepExisting && stored
		if keepValue {
			s.log.DebugContext(s.ctx, "sabr: keeping existing context value", "type", u.Type, "scope", u.Scope)
		} else {
			s.log.DebugContext(s.ctx, "sabr: context received",
				"type", u.Type, "scope", u.Scope, "value_len", len(u.Value), "write_policy", u.WritePolicy)
			if !stored || !bytes.Equal(existing, u.Value) {
				s.sabrContexts[u.Type] = u.Value
				changed = true
			}
		}
		if u.SendByDefault && !s.activeContextTypes[u.Type] {
			s.activeContextTypes[u.Type] = true
			changed = true
		}
	}
	if policy != nil {
		changed = s.applyContextPolicy(policy) || changed
	}
	return changed
}

// applyContextPolicy applies a SABR_CONTEXT_SENDING_POLICY: start activates a
// type so it is echoed, stop deactivates it, and discard drops the stored value
// entirely. It returns whether any state changed.
func (s *stream) applyContextPolicy(policy *SabrContextSendingPolicy) bool {
	changed := false
	for _, t := range policy.StartPolicy {
		if _, held := s.sabrContexts[t]; !held {
			// Nothing to echo for a type we hold no value for. Skipping it also
			// keeps activeContextTypes a subset of the (capped) stored types, so a
			// server can't grow it without bound.
			s.log.DebugContext(s.ctx, "sabr: ignoring start for an unknown context type", "type", t)
			continue
		}
		if !s.activeContextTypes[t] {
			s.activeContextTypes[t] = true
			changed = true
			s.log.DebugContext(s.ctx, "sabr: context activated", "type", t)
		}
	}
	for _, t := range policy.StopPolicy {
		if s.activeContextTypes[t] {
			delete(s.activeContextTypes, t)
			changed = true
			s.log.DebugContext(s.ctx, "sabr: context deactivated", "type", t)
		}
	}
	for _, t := range policy.DiscardPolicy {
		discarded := false
		if _, ok := s.sabrContexts[t]; ok {
			delete(s.sabrContexts, t)
			discarded = true
		}
		if s.activeContextTypes[t] {
			delete(s.activeContextTypes, t)
			discarded = true
		}
		if discarded {
			changed = true
			s.log.DebugContext(s.ctx, "sabr: context discarded", "type", t)
		}
	}
	return changed
}

// populateContexts fills streamerContext fields 5/6 from the stored context maps:
// active types are echoed with their value in sabr_contexts; stored-but-inactive
// types are listed in unsent_sabr_contexts. Types are sorted for deterministic
// output. Active types are always a subset of the stored types (see
// applyContextPolicy), so every active type has a value to echo.
func (s *stream) populateContexts(sc *streamerContext) {
	if len(s.sabrContexts) == 0 {
		return
	}
	types := make([]int32, 0, len(s.sabrContexts))
	for t := range s.sabrContexts {
		types = append(types, t)
	}
	slices.Sort(types)
	for _, t := range types {
		if s.activeContextTypes[t] {
			sc.SabrContexts = append(sc.SabrContexts, SabrContext{Type: t, Value: s.sabrContexts[t]})
		} else {
			sc.UnsentSabrContexts = append(sc.UnsentSabrContexts, t)
		}
	}
	s.log.DebugContext(s.ctx, "sabr: outgoing context set",
		"active", len(sc.SabrContexts), "unsent", len(sc.UnsentSabrContexts))
}

// post builds and sends one VideoPlaybackAbrRequest, returning the response body.
func (s *stream) post() ([]byte, error) {
	body := s.buildRequest().marshal()

	ctx := s.ctx
	if to := s.roundTimeout(); to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.serverURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "*/*")
	if s.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", s.cfg.UserAgent)
	}
	if s.cfg.ClientInfo.AcceptLanguage != "" {
		req.Header.Set("Accept-Language", s.cfg.ClientInfo.AcceptLanguage)
	}

	resp, err := s.cfg.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &waxerr.HTTPStatusError{StatusCode: resp.StatusCode, Status: resp.Status, URL: s.serverURL}
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxRoundBytes))
}

// buildRequest assembles the next SABR request from the current stream state.
func (s *stream) buildRequest() videoPlaybackAbrRequest {
	req := videoPlaybackAbrRequest{
		ClientAbrState: clientAbrState{
			PlayerTimeMs:      s.downloadedMs,
			EnabledTrackTypes: enabledTrackTypesAudioOnly,
		},
		SelectedFormatIds: []FormatId{s.cfg.Format},
		PlayerTimeMs:      s.downloadedMs,
		UstreamerConfig:   s.cfg.UstreamerConfig,
		StreamerContext: streamerContext{
			ClientInfo:     s.cfg.ClientInfo,
			POToken:        s.cfg.POToken,
			PlaybackCookie: s.playbackCookie,
		},
	}
	s.populateContexts(&req.StreamerContext)
	// Report only the contiguous run already emitted ([firstSeq, nextSeq-1]).
	// Reporting the highest received segment would hide any gap and prevent
	// retransmission.
	if s.seqInit && s.nextSeq > s.firstSeq {
		req.BufferedRanges = []BufferedRange{{
			FormatId:          s.cfg.Format,
			StartTimeMs:       0,
			DurationMs:        s.downloadedMs,
			StartSegmentIndex: int32(s.firstSeq),
			EndSegmentIndex:   int32(s.nextSeq - 1),
		}}
	}
	return req
}

// descramble solves the n parameter of a redirect URL when a DescrambleN hook is
// configured; without one the URL is followed unchanged.
func (s *stream) descramble(rawURL string) (string, error) {
	if s.cfg.DescrambleN == nil {
		return rawURL, nil
	}
	out, err := s.cfg.DescrambleN(s.ctx, rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: descramble SABR redirect: %v", waxerr.ErrExtractionFailed, err)
	}
	return out, nil
}

func (s *stream) sleep(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *stream) roundTimeout() time.Duration {
	if s.cfg.RoundTimeout > 0 {
		return s.cfg.RoundTimeout
	}
	return defaultRoundTimeout
}

func (s *stream) maxBackoff() time.Duration {
	if s.cfg.MaxBackoff > 0 {
		return s.cfg.MaxBackoff
	}
	return defaultMaxBackoff
}

// clampBackoff converts a server-supplied backoff in milliseconds to a duration
// clamped to [0, max]. It compares before converting to time.Duration to avoid
// overflow.
func clampBackoff(ms int64, max time.Duration) time.Duration {
	if ms <= 0 {
		return 0
	}
	if max > 0 && ms >= max.Milliseconds() {
		return max
	}
	return time.Duration(ms) * time.Millisecond
}

// minKey returns the smallest key in m, which must be non-empty.
func minKey(m map[uint64]mediaSegment) uint64 {
	var lowest uint64
	first := true
	for k := range m {
		if first {
			lowest, first = k, false
			continue
		}
		lowest = min(lowest, k)
	}
	return lowest
}

// wrapExtraction marks a SABR decode failure as an extraction error and includes
// the protocol revision used by the parser.
func wrapExtraction(err error) error {
	return fmt.Errorf("%w: SABR decode (proto pinned to %s): %v", waxerr.ErrExtractionFailed, upstreamCommit, err)
}
