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
	// maxRedirects bounds SABR_REDIRECT hops in one Open.
	maxRedirects = 5
	// maxRoundBytes caps a single SABR response body. Audio rounds are normally
	// much smaller.
	maxRoundBytes = 64 << 20

	// statusAttestationPending is the lowest StreamProtectionStatus.status that
	// indicates the PO token has not been accepted (2=PENDING, 3=REQUIRED).
	statusAttestationPending = 2
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
		cfg:        cfg,
		log:        log,
		progress:   progress,
		ctx:        sctx,
		cancel:     cancel,
		serverURL:  cfg.ServerAbrURL,
		segments:   make(map[uint64][]byte),
		contentLen: cfg.ContentLength,
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

	// Reassembly state. The init segment is emitted once, then media segments in
	// contiguous sequence order; segments holds received-but-not-yet-contiguous
	// media keyed by sequence number.
	initBytes    []byte
	initWritten  bool
	segments     map[uint64][]byte
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
	if res.redirect != "" {
		s.redirects++
		if s.redirects > maxRedirects {
			return fmt.Errorf("%w: too many SABR redirects", waxerr.ErrExtractionFailed)
		}
		s.serverURL = res.redirect
		s.emptyRounds = 0
		return nil // re-POST to the new endpoint without advancing state
	}

	emitted := s.integrate(res)
	if s.complete() {
		s.done = true
		return nil
	}
	if emitted == 0 {
		s.emptyRounds++
		if s.emptyRounds >= maxEmptyRounds {
			return s.stallResult()
		}
	} else {
		s.emptyRounds = 0
	}
	if res.policy != nil && res.policy.BackoffTimeMs > 0 {
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
		case partSabrContextUpdate, partSabrContextSendPol:
			// Context updates are not needed for the short audio streams
			// currently supported.
		default:
			// Unknown part; the UMP reader already skipped its payload by size.
		}
	}
}

// integrate adds one response's segments to the reassembly buffer and moves any
// newly contiguous bytes into pending. It returns the number of bytes emitted.
func (s *stream) integrate(res roundResult) int {
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
			}
			continue
		}
		seq := h.SequenceNumber
		if s.seqInit && seq < s.nextSeq {
			continue // already emitted (e.g. a redirect/reload re-sent it)
		}
		if _, exists := s.segments[seq]; exists {
			continue // duplicate within the buffer; do not double-count duration
		}
		s.segments[seq] = data
		s.downloadedMs += h.DurationMs
	}
	if !s.seqInit && len(s.segments) > 0 {
		// The first request carries no buffered range, so the server streams from
		// the start: the lowest sequence number in the first delivered batch is the
		// stream's first segment.
		s.nextSeq = minKey(s.segments)
		s.firstSeq = s.nextSeq
		s.seqInit = true
	}
	return s.drain()
}

// drain appends the init segment (once) and all contiguous media segments to
// pending, reporting progress for the bytes emitted.
func (s *stream) drain() int {
	emitted := 0
	if !s.initWritten && s.initBytes != nil {
		s.pending = append(s.pending, s.initBytes...)
		emitted += len(s.initBytes)
		s.initWritten = true
	}
	for {
		data, ok := s.segments[s.nextSeq]
		if !ok {
			break
		}
		s.pending = append(s.pending, data...)
		emitted += len(data)
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

// complete reports whether every media segment up to the known end has been
// emitted contiguously.
func (s *stream) complete() bool {
	return s.endSegment > 0 && s.seqInit && s.nextSeq > s.endSegment
}

// stallResult returns an error when the stream is known to be incomplete.
// Without a known end segment, prior media followed by no progress is treated as
// EOF.
func (s *stream) stallResult() error {
	if s.endSegment > 0 && s.nextSeq <= s.endSegment {
		return fmt.Errorf("%w: SABR stream stalled at segment %d of %d", waxerr.ErrExtractionFailed, s.nextSeq, s.endSegment)
	}
	if len(s.segments) > 0 {
		return fmt.Errorf("%w: SABR stream stalled with %d undelivered segments", waxerr.ErrExtractionFailed, len(s.segments))
	}
	if s.bytesWritten == 0 {
		return fmt.Errorf("%w: SABR stream stalled before delivering any media", waxerr.ErrExtractionFailed)
	}
	// No end segment is known, and all received media has been emitted.
	s.done = true
	return nil
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

func minKey(m map[uint64][]byte) uint64 {
	var min uint64
	first := true
	for k := range m {
		if first || k < min {
			min, first = k, false
		}
	}
	return min
}

// wrapExtraction marks a SABR decode failure as an extraction error and includes
// the protocol revision used by the parser.
func wrapExtraction(err error) error {
	return fmt.Errorf("%w: SABR decode (proto pinned to %s): %v", waxerr.ErrExtractionFailed, upstreamCommit, err)
}
