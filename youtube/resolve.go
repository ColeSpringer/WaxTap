package youtube

import (
	"context"
	"fmt"
	"net/http"

	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube/internal/resolver"
)

// Resolve turns the candidate at formatIndex, an index into
// Extraction.Video().Formats, into a playable signed stream. It reuses the client
// profile and session that won extraction so the media URL is resolved under the
// same YouTube identity.
//
// Resolution is index-based rather than itag-based because itags can repeat
// across languages and DRC variants. rawAudio[i] is kept parallel to
// Video.Formats[i] so the selected public format maps to the right raw format.
func (c *Client) Resolve(ctx context.Context, ext *Extraction, formatIndex int) (ResolvedStream, error) {
	return c.ResolveWithFailure(ctx, ext, formatIndex, nil)
}

// ResolveWithFailure resolves the selected format like Resolve and passes the
// HTTP failure that caused a refresh to the PO-token provider. Initial resolution
// passes nil. When refreshing an expired signed URL, call this with a fresh
// Extraction; the old player response still contains the old URL.
func (c *Client) ResolveWithFailure(ctx context.Context, ext *Extraction, formatIndex int, failure *potoken.HTTPFailure) (ResolvedStream, error) {
	if ext == nil {
		return ResolvedStream{}, fmt.Errorf("%w: nil extraction", waxerr.ErrExtractionFailed)
	}
	if c.resolver == nil {
		return ResolvedStream{}, fmt.Errorf("%w: no resolver configured", waxerr.ErrExtractionFailed)
	}
	rf, ok := ext.rawFormatByIndex(formatIndex)
	if !ok {
		return ResolvedStream{}, fmt.Errorf("%w: format index %d out of range", waxerr.ErrExtractionFailed, formatIndex)
	}

	token, err := c.resolveToken(ctx, ext, failure)
	if err != nil {
		return ResolvedStream{}, err
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
		return ResolvedStream{}, err
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
	return out, nil
}

// resolveToken obtains a PO token for the winning profile's required scope, or
// nil when no token is needed. The youtube package calls the provider because it
// owns the profile and session values needed in the request.
//
// failure carries the HTTP 403 that triggered a refresh, if any. Initial
// resolution passes nil; retry paths can pass the triggering failure so providers
// can use it for diagnostics.
func (c *Client) resolveToken(ctx context.Context, ext *Extraction, failure *potoken.HTTPFailure) (*resolver.Token, error) {
	scope := ext.profile.RequiresPOToken
	if scope == potoken.ScopeNone {
		return nil, nil
	}
	if c.potoken == nil {
		// A required token is unavailable; fail before returning a URL that is
		// expected to 403 when downloaded.
		return nil, fmt.Errorf("%w: client %q requires a %s PO token but no provider is configured",
			waxerr.ErrNeedsPOToken, ext.profile.Name, scope)
	}

	resp, err := c.potoken.ProvidePOToken(ctx, potoken.Request{
		VideoID:       ext.video.ID,
		ClientName:    ext.profile.InnerTubeName,
		ClientVersion: ext.profile.Version,
		VisitorData:   ext.session.visitorData,
		Scope:         scope,
		Failure:       failure,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: PO token provider failed: %v", waxerr.ErrNeedsPOToken, err)
	}
	if resp.Token == "" && len(resp.Headers) == 0 && len(resp.Query) == 0 {
		return nil, fmt.Errorf("%w: PO token provider returned nothing for client %q",
			waxerr.ErrNeedsPOToken, ext.profile.Name)
	}
	return &resolver.Token{
		Scope:   scope,
		Value:   resp.Token,
		Headers: resp.Headers,
		Query:   resp.Query,
		Expires: resp.ExpiresAt,
	}, nil
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
