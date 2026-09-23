package youtube

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
	"github.com/colespringer/waxtap/v3/youtube/internal/resolver"
	"github.com/colespringer/waxtap/v3/youtube/internal/sabr"
)

// Limit retries so a bad endpoint or token provider cannot loop indefinitely.
const (
	maxSABRReloads      = 2
	maxSABRPOTRefreshes = 1
)

// SABRStream represents a SABR-backed audio stream. SABR formats have no direct
// media URL; Open fetches their bytes from serverAbrStreamingUrl.
type SABRStream struct {
	client    *Client
	ext       *Extraction
	formatIdx int
	// primedToken holds the GVS PO token that PrimeToken minted for the first Open.
	primedToken *potoken.Response
}

// SABRStreamInfo describes an open SABR stream.
type SABRStreamInfo struct {
	ContentLength int64  // bytes, or 0 when unknown
	ContentType   string // MIME type reported by the SABR response
}

func (c *Client) newSABRStream(ext *Extraction, formatIndex int) *SABRStream {
	return &SABRStream{client: c, ext: ext, formatIdx: formatIndex}
}

// Open starts the SABR stream and returns a reader over the reassembled audio.
// It sends the first request before returning, so initial protocol and
// authentication failures are returned by Open. Later failures are returned by
// Read.
//
// Open retries a bounded number of player reloads and GVS PO-token rejections.
// progress receives byte counts and may be nil.
func (s *SABRStream) Open(ctx context.Context, progress func(bytesWritten, total int64)) (io.ReadCloser, SABRStreamInfo, error) {
	var pf sabr.ProgressFunc
	if progress != nil {
		pf = func(p sabr.Progress) { progress(p.BytesWritten, p.Total) }
	}

	ext := s.ext
	primed := s.primedToken
	s.primedToken = nil // single-use: a second Open re-mints
	var failure *potoken.HTTPFailure
	reloads, potRefreshes := 0, 0
	for {
		cfg, err := s.client.buildSABRConfig(ctx, ext, s.formatIdx, failure, primed)
		primed = nil // the primed token applies only to the first build
		if err != nil {
			return nil, SABRStreamInfo{}, err
		}
		rc, info, err := sabr.Open(ctx, cfg, pf)
		if err == nil {
			return &sabrReader{ReadCloser: rc}, SABRStreamInfo{ContentLength: info.ContentLength, ContentType: info.ContentType}, nil
		}
		switch {
		case errors.Is(err, sabr.ErrReloadPlayer):
			if reloads >= maxSABRReloads {
				return nil, SABRStreamInfo{}, fmt.Errorf("%w: SABR reload limit (%d) reached", waxerr.ErrExtractionFailed, maxSABRReloads)
			}
			reloads++
			next, idx, rerr := s.reextract(ctx)
			if rerr != nil {
				return nil, SABRStreamInfo{}, rerr
			}
			// Keep the handle current so ContextGeneration names the context
			// that actually streamed and a second reload compares against this
			// pick. Open runs on one goroutine and readers exist only after it
			// returns, so plain assignment is safe.
			s.ext, s.formatIdx = next, idx
			ext, failure = next, nil
		case errors.Is(err, waxerr.ErrNeedsPOToken) || isSABRAuthFailure(err):
			// Both statuses indicate that the GVS token was rejected.
			if potRefreshes >= maxSABRPOTRefreshes {
				return nil, SABRStreamInfo{}, sabrClientTokenError(ext.profile.Name, err)
			}
			potRefreshes++
			failure = sabrRefreshFailure(cfg.ServerAbrURL, err)
		default:
			return nil, SABRStreamInfo{}, err
		}
	}
}

// PrimeToken mints the GVS PO token before Open so provider failures surface
// while the caller can still use a fallback. The first Open consumes the token;
// later opens, reloads, and refreshes mint a new one. It is a no-op when the
// selected profile does not require a GVS token.
func (s *SABRStream) PrimeToken(ctx context.Context) error {
	token, err := s.client.fetchPOToken(ctx, s.ext.profile, s.ext.session, s.ext.video.ID, potoken.ScopeGVS, nil)
	if err != nil {
		return err
	}
	s.primedToken = token
	return nil
}

// ContextGeneration reports the provider session generation of the attested
// context the stream last used; a reload keeps it current. Pass it to
// [Client.ReportPlayerContext] when the stream capped. Zero for streams off the
// web-context path or from a provider that does not version its sessions.
func (s *SABRStream) ContextGeneration() uint64 {
	return s.ext.contextGen
}

// Format reports the format the stream delivers. A reload during Open can move
// the stream to the selected rendition re-encoded at a new lastModified, whose
// size, bitrate, and duration may differ from the format first selected.
func (s *SABRStream) Format() format.Format {
	rf, ok := s.ext.rawFormatByIndex(s.formatIdx)
	if !ok {
		return format.Format{}
	}
	return rf.toFormat()
}

// reextract refreshes the player response and re-finds the selected rendition,
// returning the new extraction and the rendition's index in it. It matches the
// (itag, lastModified, xtags) triple the last request named: multi-audio and DRC
// videos repeat an itag, so an itag match could move the stream onto a dub or
// the DRC variant. The same rendition at a new lastModified was re-encoded
// between the two responses and is accepted, since nothing has been delivered
// yet. A WEB-context extraction re-fetches a fresh attested context (not the
// InnerTube chain) so the new URL, session, and GVS-token binding stay coherent
// after the reload.
func (s *SABRStream) reextract(ctx context.Context) (*Extraction, int, error) {
	// buildSABRConfig read this index for the request that drew the reload.
	want, _ := s.ext.rawFormatByIndex(s.formatIdx)
	var ext *Extraction
	var err error
	if s.ext.webContext {
		ext, err = s.client.ExtractWebContext(ctx, s.ext.video.ID)
	} else {
		// Reload through the original attempt so the stream stays on the same
		// client.
		ext, err = s.client.ExtractAttempt(ctx, s.ext.video.ID, s.ext.Attempt())
	}
	if err != nil {
		return nil, 0, err
	}
	idx, exact := findFormat(ext.rawAudio, want)
	if idx < 0 {
		return nil, 0, fmt.Errorf("%w: selected itag %d (lmt %q, xtags %q) is unavailable after SABR reload",
			waxerr.ErrExtractionFailed, want.Itag, want.LastModified, want.XTags)
	}
	if !exact {
		s.client.log.InfoContext(ctx, "SABR reload re-encoded the selected format; continuing at its new lastModified",
			"itag", want.Itag, "xtags", want.XTags, "old_lmt", want.LastModified, "new_lmt", ext.rawAudio[idx].LastModified)
	}
	return ext, idx, nil
}

// findFormat locates want in raw: its encoding anywhere in the list, else the
// one entry that is the same rendition at another lastModified. idx is -1 when
// raw holds neither, or more than one such entry: a DRC variant no field marks
// looks like a second re-encode, and a guess could stream it.
func findFormat(raw []rawFormat, want rawFormat) (idx int, exact bool) {
	if i := findEncoding(raw, want); i >= 0 {
		return i, true
	}
	idx = -1
	for i, rf := range raw {
		if !sameRendition(rf, want) {
			continue
		}
		if idx >= 0 {
			return -1, false
		}
		idx = i
	}
	return idx, false
}

// findEncoding returns the index of want's encoding in raw, or -1: the same
// (itag, lastModified, xtags) triple, with lastModified compared as the response
// spells it, since two unparseable values both reach the wire as 0. Only the
// triple counts; a DRC flag or track id a response adds or drops does not make
// another encoding.
func findEncoding(raw []rawFormat, want rawFormat) int {
	for i, rf := range raw {
		if rf.Itag == want.Itag && rf.LastModified == want.LastModified && rf.XTags == want.XTags {
			return i
		}
	}
	return -1
}

// sameRendition reports whether a and b are one rendition, possibly at two
// lastModified values: the same itag, xtags, DRC flag and audio track.
func sameRendition(a, b rawFormat) bool {
	return a.Itag == b.Itag && a.XTags == b.XTags &&
		sabrDRC(a) == sabrDRC(b) && sabrAudioTrackID(a) == sabrAudioTrackID(b)
}

// buildSABRConfig assembles a sabr.Config for ext's format at formatIndex.
// failure describes the token rejection that triggered a refresh, if any.
// primed is an optional GVS token to reuse for the first build.
func (c *Client) buildSABRConfig(ctx context.Context, ext *Extraction, formatIndex int, failure *potoken.HTTPFailure, primed *potoken.Response) (sabr.Config, error) {
	rf, ok := ext.rawFormatByIndex(formatIndex)
	if !ok {
		return sabr.Config{}, fmt.Errorf("%w: format index %d out of range", waxerr.ErrExtractionFailed, formatIndex)
	}
	if ext.serverAbrURL == "" {
		return sabr.Config{}, fmt.Errorf("%w: SABR format has no serverAbrStreamingUrl", waxerr.ErrExtractionFailed)
	}

	// The SABR streamerContext carries the raw GVS PO token bytes. A refresh
	// always mints a new token.
	token := primed
	if failure != nil || token == nil {
		fresh, err := c.fetchPOToken(ctx, ext.profile, ext.session, ext.video.ID, potoken.ScopeGVS, failure)
		if err != nil {
			return sabr.Config{}, err
		}
		token = fresh
	}
	var potBytes []byte
	if token != nil {
		b, err := decodeBase64Tolerant(token.Token)
		if err != nil {
			return sabr.Config{}, fmt.Errorf("%w: decode GVS PO token: %v", waxerr.ErrExtractionFailed, err)
		}
		potBytes = b
	}

	ustreamer, err := decodeBase64Tolerant(ext.ustreamerConfig)
	if err != nil {
		return sabr.Config{}, fmt.Errorf("%w: decode ustreamer config: %v", waxerr.ErrExtractionFailed, err)
	}

	// Failure to solve n may throttle the stream but does not make the URL
	// unusable. Cancellation still stops the request.
	descramble := c.sabrDescrambleHook(ext.video.ID, ext.playerURL)
	serverURL := ext.serverAbrURL
	if descramble != nil {
		if descrambled, derr := descramble(ctx, serverURL); derr != nil {
			if ctx.Err() != nil {
				return sabr.Config{}, ctx.Err()
			}
			c.log.WarnContext(ctx, "could not descramble the n parameter in SABR serverAbrStreamingUrl; stream may be throttled", "err", derr)
		} else {
			serverURL = descrambled
		}
	}

	cfg := sabr.Config{
		HTTP:            c.http,
		Logger:          c.log,
		ServerAbrURL:    serverURL,
		UstreamerConfig: ustreamer,
		Format:          sabrFormatID(rf),
		ClientInfo:      sabrClientInfo(ext.profile, c.hl),
		UserAgent:       ext.profile.UserAgent,
		POToken:         potBytes,
		ContentLength:   atoi64(rf.ContentLength),
		DescrambleN:     descramble,
		DumpDir:         os.Getenv(sabrDumpEnvVar),
		DRC:             sabrDRC(rf),
		AudioTrackID:    sabrAudioTrackID(rf),
	}
	return cfg, nil
}

// sabrDescrambleHook returns an n-parameter solver bound to videoID. When
// playerURL is set (the WEB-context path), the solver descrambles against that
// exact base.js instead of one discovered from the video, because YouTube
// A/B-tests base.js per visitor and the context's n is only coherent with the
// player its /player referenced. It returns nil when the configured resolver
// does not support player inspection.
func (c *Client) sabrDescrambleHook(videoID, playerURL string) func(context.Context, string) (string, error) {
	if c.inspector == nil {
		return nil
	}
	return func(ctx context.Context, rawURL string) (string, error) {
		return c.inspector.DescrambleN(ctx, resolver.Context{VideoID: videoID, PlayerURL: playerURL}, rawURL)
	}
}

// sabrFormatID maps a raw player format to the SABR format selector. LastModified
// and XTags distinguish encodings that share an itag.
func sabrFormatID(rf rawFormat) sabr.FormatId {
	return sabr.FormatId{
		Itag:         int32(rf.Itag),
		LastModified: uint64(atoi64(rf.LastModified)),
		XTags:        rf.XTags,
	}
}

// sabrDRC reports whether SABR must declare rf as a DRC rendition.
func sabrDRC(rf rawFormat) bool { return drcFromPtr(rf.IsDrc) == format.Yes }

// sabrAudioTrackID is the audio track SABR declares for rf, or "" when rf
// carries no audioTrack, which leaves client_abr_state.audio_track_id unset.
func sabrAudioTrackID(rf rawFormat) string {
	if rf.AudioTrack == nil {
		return ""
	}
	return rf.AudioTrack.ID
}

// sabrClientInfo maps the winning profile to the SABR streamerContext identity.
// ClientName is the numeric InnerTube id, not the string name.
func sabrClientInfo(p ClientProfile, hl string) sabr.ClientInfo {
	return sabr.ClientInfo{
		ClientName:     int32(p.InnerTubeID),
		ClientVersion:  p.Version,
		OSName:         p.OSName,
		OSVersion:      p.OSVersion,
		DeviceMake:     p.DeviceMake,
		DeviceModel:    p.DeviceModel,
		AcceptLanguage: acceptLanguage(hl),
	}
}

// sabrClientTokenError adds the rejected client's name while preserving the
// original error for errors.Is and errors.AsType.
func sabrClientTokenError(clientName string, err error) error {
	if clientName == "" {
		return err
	}
	return fmt.Errorf("%w (client %q)", err, clientName)
}

// isSABRAuthFailure reports whether the SABR endpoint returned HTTP 401 or 403.
func isSABRAuthFailure(err error) bool {
	httpErr, ok := errors.AsType[*waxerr.HTTPStatusError](err)
	return ok && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden)
}

// sabrRefreshFailure describes a SABR token rejection for the PO-token provider.
// In-protocol attestation has no HTTP status, so it is represented as 401.
func sabrRefreshFailure(serverURL string, err error) *potoken.HTTPFailure {
	if httpErr, ok := errors.AsType[*waxerr.HTTPStatusError](err); ok {
		return &potoken.HTTPFailure{StatusCode: httpErr.StatusCode, Status: httpErr.Status, URL: serverURL}
	}
	return &potoken.HTTPFailure{
		StatusCode: http.StatusUnauthorized,
		Status:     "SABR stream protection: attestation required",
		URL:        serverURL,
	}
}

// sabrReader converts a mid-stream reload signal into a public extraction error.
// Retrying after bytes have been delivered would corrupt the output.
type sabrReader struct {
	io.ReadCloser
}

func (r *sabrReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, sabr.ErrReloadPlayer) {
		return n, fmt.Errorf("%w: SABR reload signaled mid-stream", waxerr.ErrExtractionFailed)
	}
	return n, err
}

// decodeBase64Tolerant accepts standard or URL base64, with or without padding.
// An empty string decodes to nil.
func decodeBase64Tolerant(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64 (%d chars)", len(s))
}
