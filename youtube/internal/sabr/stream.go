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
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/internal/dumpfile"
	"github.com/colespringer/waxtap/internal/httpx"
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

	// StreamProtectionStatus.status values. Only REQUIRED (3) is terminal: the PO
	// token is rejected and a better one is needed. PENDING (2) is informational;
	// the server still streams media while attestation settles, so WaxTap consumes
	// it and continues, matching the googlevideo reference, which aborts only on 3.
	statusAttestationPending  = 2
	statusAttestationRequired = 3

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
	// Do executes a SABR HTTP request.
	Do(*http.Request) (*http.Response, error)
}

// Progress reports the number of bytes emitted by the stream.
type Progress struct {
	BytesWritten int64 // bytes emitted so far
	Total        int64 // expected bytes, or 0 when unknown
}

// ProgressFunc receives best-effort byte progress. Read calls it synchronously,
// so implementations should return quickly.
type ProgressFunc func(Progress)

// StreamInfo is the response metadata returned alongside the reader.
type StreamInfo struct {
	ContentLength int64  // bytes, or 0 when unknown
	ContentType   string // response MIME type
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
	// DRC mirrors the selected audio format's isDrc flag.
	DRC bool
	// AudioTrackID is the selected audio track id, for multi-audio videos.
	AudioTrackID string
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
	// DumpDir, when set, receives each round's raw response body as a
	// timestamped file so the exact UMP/protobuf bytes can be re-decoded
	// offline. Best-effort diagnostics: write failures are logged at debug and
	// never affect the stream.
	DumpDir string
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
	rounds         int // total POSTs sent, for log correlation and dump names

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

	// attestationPending records that a STREAM_PROTECTION_STATUS=2 (PENDING) was
	// seen. Status 2 still streams, but a status-2 stream that ends without an
	// end-segment or content length may be a withheld partial, so stallResult must
	// not treat it as complete.
	attestationPending bool

	// formatInitSeen records that FORMAT_INITIALIZATION_METADATA arrived, after
	// which the audio format is also reported in selected_format_ids (matching
	// the reference client's notion of a committed format).
	formatInitSeen bool

	// audioRound holds the selected audio format's media headers received in the
	// current round, drained into a buffered range by the next buildRequest.
	audioRound []*MediaHeader

	done bool
	err  error
}

// mediaSegment holds a segment's bytes and duration. The duration contributes to
// downloadedMs only after the segment is emitted.
type mediaSegment struct {
	data     []byte
	duration int64
}

// headerItag returns the itag identifying a media header's format, preferring
// the nested format id. Zero means the header did not identify its format.
func headerItag(h *MediaHeader) int32 {
	if h.FormatId.Itag != 0 {
		return h.FormatId.Itag
	}
	return h.Itag
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
	if res.attestationPending {
		s.attestationPending = true
	}
	if res.policy != nil {
		// The server's pacing directives, for diagnosing withheld-media stalls.
		s.log.DebugContext(s.ctx, "sabr: next request policy",
			"round", s.rounds,
			"target_audio_readahead_ms", res.policy.TargetAudioReadaheadMs,
			"max_time_since_last_request_ms", res.policy.MaxTimeSinceLastRequestMs,
			"backoff_ms", res.policy.BackoffTimeMs,
			"playback_cookie_len", len(res.policy.PlaybackCookie))
		if len(res.policy.PlaybackCookie) > 0 {
			s.playbackCookie = res.policy.PlaybackCookie
		}
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
	s.log.DebugContext(s.ctx, "sabr: round complete",
		"round", s.rounds,
		"headers", len(res.headers), "media_parts", len(res.media),
		"emitted_bytes", emitted, "advanced", advanced,
		"next_seq", s.nextSeq, "end_segment", s.endSegment,
		"buffered_segments", len(s.segments), "downloaded_ms", s.downloadedMs,
		"empty_rounds", s.emptyRounds, "context_rounds", s.contextRounds)
	if waiting {
		return httpx.Sleep(s.ctx, clampBackoff(res.policy.BackoffTimeMs, s.maxBackoff()))
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
	// attestationPending is set when this round carried STREAM_PROTECTION_STATUS=2.
	attestationPending bool

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
			// Only the selected audio format's metadata may drive completion;
			// with a declared (discarded) video format the server can describe
			// that format too, and its segment count must not become ours.
			if m.FormatId.Itag != 0 && m.FormatId.Itag != s.cfg.Format.Itag {
				s.log.DebugContext(s.ctx, "sabr: ignoring format metadata for non-selected format", "itag", m.FormatId.Itag)
				continue
			}
			s.formatInitSeen = true
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
			if st.Status >= statusAttestationRequired {
				res.signal = fmt.Errorf("%w: SABR attestation required (status %d)", waxerr.ErrNeedsPOToken, st.Status)
				return res, nil
			}
			if st.Status == statusAttestationPending {
				// Non-terminal: the server still streams while attestation settles.
				// Record it so stallResult won't treat a metadata-less partial as
				// complete.
				res.attestationPending = true
				s.log.DebugContext(s.ctx, "sabr: attestation pending (status 2); consuming media and continuing")
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
	// The per-segment trace boxes a large argument list, so it is gated here
	// rather than left to slog's internal level check.
	debug := s.log.Enabled(s.ctx, slog.LevelDebug)
	for _, hid := range res.mediaOrder {
		h := res.headers[hid]
		data := res.media[hid]
		if h == nil {
			s.log.WarnContext(s.ctx, "sabr: media bytes without a header", "header_id", hid)
			continue
		}
		// Per-segment trace ahead of the skip branches below, so re-sent and
		// below-start segments stay visible when diagnosing pacing stalls. A
		// zero duration_ms with a non-zero effective_duration_ms means the
		// server moved the duration into time_range.
		if debug {
			s.log.DebugContext(s.ctx, "sabr: segment received",
				"round", s.rounds,
				"seq", h.SequenceNumber, "is_init", h.IsInitSeg,
				"duration_ms", h.DurationMs, "effective_duration_ms", h.effectiveDurationMs(),
				"start_ms", h.StartMs, "itag", h.Itag,
				"format_itag", h.FormatId.Itag, "format_lmt", h.FormatId.LastModified, "format_xtags", h.FormatId.XTags,
				"content_length", h.ContentLength, "bytes", len(data))
		}
		// Route by format. Only the selected audio format reaches the output;
		// any other format is dropped outright so it can never corrupt the audio.
		// Itag 0 means the header did not name a format; in an audio-led session
		// it can only be the audio track.
		if itag := headerItag(h); itag != 0 && itag != s.cfg.Format.Itag {
			s.log.DebugContext(s.ctx, "sabr: dropping media for unexpected format", "itag", itag, "seq", h.SequenceNumber, "bytes", len(data))
			continue
		}
		if h.IsInitSeg {
			if s.initWritten {
				// Media was already emitted (it self-initialized, or no separate
				// init preceded it). A late init segment cannot be prepended now;
				// surface it instead of silently dropping it and corrupting output.
				s.log.WarnContext(s.ctx, "sabr: init segment arrived after media was emitted; ignoring", "header_id", hid)
				continue
			}
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
		// effectiveDurationMs, not the flat DurationMs: this duration drives
		// downloadedMs and therefore player_time_ms, which must keep advancing
		// on servers that carry segment timing only in time_range.
		s.segments[seq] = mediaSegment{data: data, duration: h.effectiveDurationMs()}
		s.audioRound = append(s.audioRound, h)
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

// ebmlMagic is the four-byte EBML header that begins every Matroska/WebM file.
var ebmlMagic = []byte{0x1A, 0x45, 0xDF, 0xA3}

// mediaSelfInitializes reports whether the buffered leading media segment carries
// its own container header, so the stream is a valid file without a separate init
// segment. YouTube's WebM/Opus SABR audio is self-initializing: the first media
// segment starts with the EBML header rather than a distinct init segment. A bare
// fragment (a headerless WebM Cluster or an MP4 moof) is not, so it stays held
// until a real init segment arrives.
func (s *stream) mediaSelfInitializes() bool {
	if !s.seqInit {
		return false
	}
	seg, ok := s.segments[s.nextSeq]
	if !ok {
		return false
	}
	return bytes.HasPrefix(seg.data, ebmlMagic)
}

// drain moves the init segment and contiguous media into pending. A separate init
// segment leads when present (e.g. fragmented MP4's moov); otherwise media that
// carries its own container header (WebM/Opus) is emitted as-is. Bare fragments
// stay held so the output is never a headerless container.
func (s *stream) drain() int {
	emitted := 0
	if !s.initWritten {
		switch {
		case s.initBytes != nil:
			s.pending = append(s.pending, s.initBytes...)
			emitted += len(s.initBytes)
		case s.mediaSelfInitializes():
			// The leading media segment is self-initializing; nothing to prepend.
		default:
			return 0 // hold until an init segment or a self-initializing lead arrives
		}
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
//
// A stall under attestation-pending (STREAM_PROTECTION_STATUS=2) is reported
// token-neutrally. A live A/B (2026-06) holding the request constant and varying
// only the GVS PO token showed the server delivers the same ~first minute of
// audio and then withholds the rest whether the token is a real INTEGRITY mint,
// garbage, or absent. The output is byte-identical, so the cap is upstream of
// the token and refreshing it does not help; the cause (a per-session preview
// limit) is still under investigation. The message must not point at the token,
// or it sends operators chasing a lever that isn't there.
func (s *stream) stallResult() error {
	desc := s.stallDescription()
	if desc == "" {
		// No available metadata proves that more data is expected.
		s.done = true
		return nil
	}
	// A stalled SABR delivery may succeed through another client. Keep the
	// attestation-pending message neutral because changing the token does not lift
	// this limit.
	if s.attestationPending {
		return fmt.Errorf("%w: %s under attestation-pending (status 2); cause is upstream of the PO token (refreshing it does not lift the cap)", waxerr.ErrIncompleteStream, desc)
	}
	return fmt.Errorf("%w: %s", waxerr.ErrIncompleteStream, desc)
}

// stallDescription names what is provably missing, or "" when nothing proves
// the stream incomplete.
func (s *stream) stallDescription() string {
	switch {
	case !s.initWritten:
		return "SABR stream stalled before delivering an init segment"
	case s.endSegment > 0 && s.nextSeq <= s.endSegment:
		return fmt.Sprintf("SABR stream stalled at segment %d of %d", s.nextSeq, s.endSegment)
	case len(s.segments) > 0:
		return fmt.Sprintf("SABR stream stalled with %d undelivered segments", len(s.segments))
	case s.contentLen > 0 && s.bytesWritten < s.contentLen:
		return fmt.Sprintf("SABR stream stalled after %d of %d bytes", s.bytesWritten, s.contentLen)
	case s.bytesWritten == 0:
		return "SABR stream stalled before delivering any media"
	case s.attestationPending:
		// Status 2 with no end-segment or content length to prove completeness;
		// it may be a withheld partial, so do not treat it as complete.
		return "SABR stream ended without completion metadata"
	}
	return ""
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
	s.rounds++
	body := s.buildRequest().marshal()
	s.dumpBody("request", body)

	ctx := s.ctx
	if to := s.roundTimeout(); to > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.requestURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "application/vnd.yt-ump")
	req.Header.Set("Accept-Encoding", "identity")
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
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRoundBytes))
	if err != nil {
		return nil, err
	}
	s.dumpBody("round", respBody)
	return respBody, nil
}

// requestURL returns the SABR endpoint with the 0-based request number
// appended, matching the reference client's `rn` parameter. The signed URL is
// extended verbatim, never re-encoded: round-tripping it through url.Values
// would re-order and re-escape signature-covered parameters and silently drop
// any pair the query parser rejects.
func (s *stream) requestURL() string {
	sep := "&"
	if !strings.Contains(s.serverURL, "?") {
		sep = "?"
	}
	return s.serverURL + sep + "rn=" + strconv.Itoa(s.rounds-1)
}

// dumpBody writes a raw request/response body under cfg.DumpDir, mirroring the
// WAXTAP_DUMP_DIR philosophy: gated by configuration, best-effort, and never
// affecting the stream. kind is "round" (response) or "request".
func (s *stream) dumpBody(kind string, body []byte) {
	dir := s.cfg.DumpDir
	if dir == "" || len(body) == 0 {
		return
	}
	path, err := dumpfile.Write(dir, fmt.Sprintf("sabr-%s-%03d.bin", kind, s.rounds), body)
	if err != nil {
		s.log.DebugContext(s.ctx, "sabr: dump failed", "dir", dir, "err", err)
		return
	}
	s.log.DebugContext(s.ctx, "sabr: wrote dump", "path", path)
}

// buildRequest assembles the next SABR request from the current stream state.
// client_abr_state.player_time_ms reports the contiguous downloaded audio
// duration as the playback position.
func (s *stream) buildRequest() videoPlaybackAbrRequest {
	req := videoPlaybackAbrRequest{
		ClientAbrState: clientAbrState{
			PlayerTimeMs:      s.downloadedMs,
			EnabledTrackTypes: enabledTrackTypesAudioOnly,
			DrcEnabled:        s.cfg.DRC,
			AudioTrackID:      s.cfg.AudioTrackID,
		},
		PreferredAudioFormatIds: []FormatId{s.cfg.Format},
		UstreamerConfig:         s.cfg.UstreamerConfig,
		StreamerContext: streamerContext{
			ClientInfo:     s.cfg.ClientInfo,
			POToken:        s.cfg.POToken,
			PlaybackCookie: s.playbackCookie,
		},
	}
	s.populateContexts(&req.StreamerContext)
	// Acknowledge the segments received last round as buffered. The reference
	// client reports these per-round deltas (with real start times and sequence
	// numbers) rather than a cumulative range; the server accumulates them.
	req.BufferedRanges = append(req.BufferedRanges, bufferedRangesFromHeaders(s.cfg.Format, s.audioRound)...)
	s.audioRound = s.audioRound[:0]
	if s.formatInitSeen {
		req.SelectedFormatIds = append(req.SelectedFormatIds, s.cfg.Format)
	}
	// Outgoing pacing state: the server streams ahead of player_time_ms, so a
	// player_time_ms that stops advancing explains withheld media.
	s.log.DebugContext(s.ctx, "sabr: request state",
		"round", s.rounds,
		"player_time_ms", s.downloadedMs,
		"buffered_ranges", len(req.BufferedRanges),
		"first_seq", s.firstSeq, "next_seq", s.nextSeq)
	return req
}

// bufferedRangesFromHeaders builds the buffered ranges covering the segments a
// format received in one round, one range per contiguous sequence run. The
// headers are sorted by sequence number first: acknowledging one span across a
// gap (or an inverted span, on out-of-order arrival) would tell the server a
// never-received segment is buffered and prevent its retransmission. Start and
// duration fall back to time_range when the flat fields are absent; timescale
// 1000 keeps ticks in milliseconds.
func bufferedRangesFromHeaders(f FormatId, hs []*MediaHeader) []BufferedRange {
	if len(hs) == 0 {
		return nil
	}
	slices.SortFunc(hs, func(a, b *MediaHeader) int {
		return cmp.Compare(a.SequenceNumber, b.SequenceNumber)
	})
	var out []BufferedRange
	for i := 0; i < len(hs); {
		j := i + 1
		dur := hs[i].effectiveDurationMs()
		for j < len(hs) && hs[j].SequenceNumber == hs[j-1].SequenceNumber+1 {
			dur += hs[j].effectiveDurationMs()
			j++
		}
		start := hs[i].effectiveStartMs()
		out = append(out, BufferedRange{
			FormatId:          f,
			StartTimeMs:       start,
			DurationMs:        dur,
			StartSegmentIndex: int32(hs[i].SequenceNumber),
			EndSegmentIndex:   int32(hs[j-1].SequenceNumber),
			TimeRange:         &TimeRange{StartTicks: start, DurationTicks: dur, Timescale: 1000},
		})
		i = j
	}
	return out
}

// descramble solves the n parameter of a redirect URL when a DescrambleN hook is
// configured; without one the URL is followed unchanged.
func (s *stream) descramble(rawURL string) (string, error) {
	if s.cfg.DescrambleN == nil {
		return rawURL, nil
	}
	out, err := s.cfg.DescrambleN(s.ctx, rawURL)
	if err != nil {
		// Preserve ErrCipherSolve so callers can distinguish it from extraction
		// failures.
		return "", fmt.Errorf("descramble SABR redirect: %w", err)
	}
	return out, nil
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
