package youtube

import (
	"context"
	"fmt"
	"net/http"

	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
	"github.com/colespringer/waxtap/v3/youtube/internal/resolver"
)

// Resolve builds a direct or SABR MediaPlan for the candidate at formatIndex.
// It reuses the client profile and session that won extraction so the media is
// fetched under the same YouTube identity.
//
// Resolution is index-based rather than itag-based because itags can repeat
// across languages and DRC variants. rawAudio[i] is kept parallel to
// Video.Formats[i] so the selected public format maps to the right raw format.
func (c *Client) Resolve(ctx context.Context, ext *Extraction, formatIndex int) (MediaPlan, error) {
	return c.ResolveWithFailure(ctx, ext, formatIndex, nil)
}

// ResolveWithFailure resolves the selected format like Resolve and passes the
// HTTP failure that caused a refresh to the PO-token provider. Initial resolution
// passes nil. When refreshing an expired signed URL, call this with a fresh
// Extraction; the old player response still contains the old URL.
func (c *Client) ResolveWithFailure(ctx context.Context, ext *Extraction, formatIndex int, failure *potoken.HTTPFailure) (MediaPlan, error) {
	if ext == nil {
		return MediaPlan{}, fmt.Errorf("%w: nil extraction", waxerr.ErrExtractionFailed)
	}
	if c.resolver == nil {
		return MediaPlan{}, fmt.Errorf("%w: no resolver configured", waxerr.ErrExtractionFailed)
	}
	rf, ok := ext.rawFormatByIndex(formatIndex)
	if !ok {
		return MediaPlan{}, fmt.Errorf("%w: format index %d out of range", waxerr.ErrExtractionFailed, formatIndex)
	}

	// A format without a URL or cipher is served through the response's SABR
	// endpoint. The GVS PO token is minted on the delivery path by
	// SABRStream.PrimeToken or, for direct callers, by the first Open. Read-only
	// resolution does not mint a token.
	if rf.URL == "" && rf.SignatureCipher == "" {
		if ext.serverAbrURL == "" {
			return MediaPlan{}, fmt.Errorf("%w: candidate has neither URL nor signatureCipher", waxerr.ErrExtractionFailed)
		}
		return MediaPlan{SABR: c.newSABRStream(ext, formatIndex, rf)}, nil
	}

	token, err := c.resolveToken(ctx, ext, failure)
	if err != nil {
		return MediaPlan{}, err
	}

	stream, err := c.resolver.Resolve(ctx, resolver.Context{
		VideoID: ext.video.ID,
		Headers: streamHeaders(ext.profile),
		Token:   token,
	}, resolver.Candidate{
		URL:             rf.URL,
		SignatureCipher: rf.SignatureCipher,
	})
	if err != nil {
		return MediaPlan{}, err
	}

	out := ResolvedStream{
		URL:           stream.URL,
		ExpiresAt:     stream.ExpiresAt,
		ContentLength: stream.ContentLength,
		Headers:       stream.Headers,
	}
	// Fill resolver gaps from the player response: content length from the raw
	// format, and expiry from streamingData when the signed URL omits it.
	if out.ContentLength == 0 {
		out.ContentLength = atoi64(rf.ContentLength)
	}
	if out.ExpiresAt.IsZero() {
		out.ExpiresAt = ext.expiresAt
	}
	return MediaPlan{Direct: &out}, nil
}

// streamHeaders derives the request headers a media (googlevideo) request should
// carry from the winning client profile. The user agent must match the client
// that extracted the formats.
func streamHeaders(p ClientProfile) http.Header {
	h := make(http.Header)
	if p.UserAgent != "" {
		h.Set("User-Agent", p.UserAgent)
	}
	return h
}
