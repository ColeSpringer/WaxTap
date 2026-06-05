// Package potoken defines the PO-token provider contract.
//
// PO ("proof of origin") tokens are used by some YouTube clients at two points. A
// player-scope token goes in the /player request body; without it, WEB-family
// clients can return formats without URLs. A GVS-scope token goes on the
// googlevideo stream URL; without it, downloads usually return 403. Tokens are
// bound to a video and expire, and are not interchangeable across scopes.
//
// WaxTap accepts caller-supplied providers and invokes them when a token is needed;
// it does not include a built-in token generator. A single video can drive one
// call per scope, so a caching provider needs to key by scope and token binding.
//
// This is a leaf package (standard library only) so both the top-level facade
// (which holds the Provider in Options) and the internal resolver (which needs
// the scope/token contract) can depend on it without an import cycle.
package potoken

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Scope identifies where a PO token is applied. Tokens are not interchangeable
// across scopes.
type Scope uint8

const (
	ScopeNone      Scope = iota
	ScopePlayer          // /player request body
	ScopeGVS             // googlevideo stream (download) URLs
	ScopeSubtitles       // subtitle/timedtext URLs
)

func (s Scope) String() string {
	switch s {
	case ScopePlayer:
		return "player"
	case ScopeGVS:
		return "gvs"
	case ScopeSubtitles:
		return "subtitles"
	default:
		return "none"
	}
}

// ParseScope is the inverse of Scope.String. It accepts the canonical names
// ("none", "player", "gvs", "subtitles"), case-insensitively, plus the empty
// string as "none". It is used to decode scopes from configuration such as the
// client-profile override file.
func ParseScope(s string) (Scope, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return ScopeNone, nil
	case "player":
		return ScopePlayer, nil
	case "gvs":
		return ScopeGVS, nil
	case "subtitles":
		return ScopeSubtitles, nil
	default:
		return ScopeNone, fmt.Errorf("unknown PO-token scope %q", s)
	}
}

// Request is passed to a Provider when WaxTap needs a token. It carries only
// stable public data, not WaxTap's internal client profile or session.
type Request struct {
	VideoID       string
	ClientName    string // InnerTube client name in play, e.g. "WEB"
	ClientVersion string
	VisitorData   string
	// UserAgent is the exact User-Agent WaxTap will send for this scope.
	// Providers that bind tokens to request headers should use it when minting.
	// Empty means the provider can use its own default.
	UserAgent string
	Scope     Scope
	Failure   *HTTPFailure // the 403 that triggered this refresh, if any
}

// Response is what a Provider returns. Response.Token is the token string; stream
// tokens may also require additional request headers or query parameters.
type Response struct {
	Token     string
	Headers   http.Header // additional headers to send (may be nil)
	Query     url.Values  // additional query parameters (may be nil)
	ExpiresAt time.Time   // zero == unknown
}

// Provider supplies PO tokens on demand. Implementations must honor ctx
// cancellation and should be safe for concurrent use.
type Provider interface {
	ProvidePOToken(ctx context.Context, req Request) (Response, error)
}

// HTTPFailure is a compact snapshot of the HTTP failure that triggered a token
// refresh, for diagnosis by the provider.
type HTTPFailure struct {
	StatusCode int
	Status     string
	URL        string
	Body       string // truncated response body
}
