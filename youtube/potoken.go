package youtube

import (
	"context"
	"fmt"

	"github.com/colespringer/waxtap/v2/potoken"
	"github.com/colespringer/waxtap/v2/waxerr"
	"github.com/colespringer/waxtap/v2/youtube/internal/resolver"
)

// This file holds the PO-token lookup shared by extraction and resolution.
// WEB-family clients can require a player token before the /player request and a
// GVS token before stream resolution. Both requests go through the configured
// potoken.Provider; each call site applies the returned value where YouTube
// expects it.

// fetchPOToken asks the configured provider for a token of the given scope, using
// the active profile and session as identity. It returns (nil, nil) when the
// profile does not require that scope, and an error wrapping waxerr.ErrNeedsPOToken
// when a required token cannot be obtained (no provider, provider error, or an
// unusable response). Context cancellation/timeout is surfaced unwrapped so callers
// can distinguish it from a token lookup failure.
func (c *Client) fetchPOToken(ctx context.Context, profile ClientProfile, sess *session, videoID string, scope potoken.Scope, failure *potoken.HTTPFailure) (*potoken.Response, error) {
	if !profile.requiresPOToken(scope) {
		return nil, nil
	}
	if c.potoken == nil {
		// A required token is unavailable; fail before issuing a /player request
		// likely to omit URLs or a stream request likely to return 403.
		return nil, fmt.Errorf("%w: client %q requires a %s PO token but no provider is configured",
			waxerr.ErrNeedsPOToken, profile.Name, scope)
	}

	resp, err := c.potoken.ProvidePOToken(ctx, potoken.Request{
		VideoID:       videoID,
		ClientName:    profile.InnerTubeName,
		ClientVersion: profile.Version,
		VisitorData:   sess.visitorData,
		// Some token services bind the token to request headers. Use the same
		// User-Agent the request for this scope will send.
		UserAgent: profile.UserAgent,
		Scope:     scope,
		Failure:   failure,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// Preserve the provider error for diagnostics while retaining the
		// ErrNeedsPOToken classification.
		return nil, fmt.Errorf("%w: PO token provider failed: %w", waxerr.ErrNeedsPOToken, err)
	}
	if !usablePOToken(scope, resp) {
		return nil, fmt.Errorf("%w: PO token provider returned nothing usable for client %q (scope %s)",
			waxerr.ErrNeedsPOToken, profile.Name, scope)
	}
	// Keep subsequent player, GVS, and SABR requests on the identity used to mint
	// this token.
	sess.bindPOToken()
	return &resp, nil
}

// usablePOToken reports whether resp carries something the given scope can use.
// A /player request uses only Response.Token; stream resolution can also pass
// provider headers and query parameters to the resolver.
func usablePOToken(scope potoken.Scope, resp potoken.Response) bool {
	if scope == potoken.ScopePlayer {
		return resp.Token != ""
	}
	return resp.Token != "" || len(resp.Headers) > 0 || len(resp.Query) > 0
}

// resolveToken obtains the GVS/stream-scope PO token for the winning profile and
// maps it to a resolver.Token, or nil when no GVS token is needed. It is the
// stream-side counterpart to the player-token fetch in Extract.
//
// failure carries the HTTP 403 that triggered a refresh, if any; initial
// resolution passes nil.
func (c *Client) resolveToken(ctx context.Context, ext *Extraction, failure *potoken.HTTPFailure) (*resolver.Token, error) {
	resp, err := c.fetchPOToken(ctx, ext.profile, ext.session, ext.video.ID, potoken.ScopeGVS, failure)
	if err != nil || resp == nil {
		return nil, err
	}
	return &resolver.Token{
		Scope:   potoken.ScopeGVS,
		Value:   resp.Token,
		Headers: resp.Headers,
		Query:   resp.Query,
		Expires: resp.ExpiresAt,
	}, nil
}
