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
	"time"

	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube/internal/resolver"
	"github.com/colespringer/waxtap/youtube/internal/sabr"
)

// Limit retries so a bad endpoint or token provider cannot loop indefinitely.
const (
	maxSABRReloads      = 2
	maxSABRPOTRefreshes = 1
)

// SABRStream represents a SABR-backed audio stream. SABR formats have no direct
// media URL; Open fetches their bytes from serverAbrStreamingUrl.
type SABRStream struct {
	client        *Client
	ext           *Extraction
	formatIdx     int
	pinnedItag    int
	contentLength int64
	expiresAt     time.Time
	// primedToken holds the GVS PO token that PrimeToken minted for the first Open.
	primedToken *potoken.Response
}

// SABRStreamInfo describes an open SABR stream.
type SABRStreamInfo struct {
	ContentLength int64  // bytes, or 0 when unknown
	ContentType   string // MIME type reported by the SABR response
}

func (c *Client) newSABRStream(ext *Extraction, formatIndex int, rf rawFormat) *SABRStream {
	return &SABRStream{
		client:        c,
		ext:           ext,
		formatIdx:     formatIndex,
		pinnedItag:    rf.Itag,
		contentLength: atoi64(rf.ContentLength),
		expiresAt:     ext.expiresAt,
	}
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
			next, rerr := s.reextract(ctx)
			if rerr != nil {
				return nil, SABRStreamInfo{}, rerr
			}
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

// reextract refreshes the player response and finds the originally selected
// itag, whose index may have changed. A WEB-context extraction re-fetches a
// fresh attested context (not the InnerTube chain) so the new URL, session, and
// GVS-token binding stay coherent after a mid-stream reload.
func (s *SABRStream) reextract(ctx context.Context) (*Extraction, error) {
	var ext *Extraction
	var err error
	if s.ext.webContext {
		ext, err = s.client.ExtractWebContext(ctx, s.ext.video.ID)
	} else {
		// Reload through the original attempt so resumed media comes from the same
		// client.
		ext, err = s.client.ExtractAttempt(ctx, s.ext.video.ID, s.ext.Attempt())
	}
	if err != nil {
		return nil, err
	}
	for i, rf := range ext.rawAudio {
		if rf.Itag == s.pinnedItag {
			s.formatIdx = i
			return ext, nil
		}
	}
	return nil, fmt.Errorf("%w: selected itag %d is unavailable after SABR reload", waxerr.ErrExtractionFailed, s.pinnedItag)
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
		DRC:             rf.IsDrc != nil && *rf.IsDrc,
	}
	if rf.AudioTrack != nil {
		cfg.AudioTrackID = rf.AudioTrack.ID
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
