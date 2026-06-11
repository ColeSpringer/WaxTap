// Package potoken defines the PO-token provider contract and the related
// browser-attested handoff contracts ([Session] and [PlayerContext]) that let
// WaxTap adopt an external attesting browser's identity and streaming context.
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

// ProviderFunc adapts an ordinary function to Provider, so a caller can supply PO
// tokens from a closure (for example, one that fetches them from a URL) without
// defining a named type. Like a single token, the closure is invoked once per
// video per scope, so a closure that mints fresh each call should cache by
// (scope, binding) if it is expensive.
type ProviderFunc func(ctx context.Context, req Request) (Response, error)

// ProvidePOToken calls f.
func (f ProviderFunc) ProvidePOToken(ctx context.Context, req Request) (Response, error) {
	return f(ctx, req)
}

// Session is an externally supplied, pre-resolved guest identity that WaxTap can
// adopt instead of bootstrapping its own. It exists so the session that attested
// a GVS PO token (e.g. a real browser driving a token minter) and the session
// WaxTap streams under can be one byte-identical identity.
//
// VisitorData must be the exact X-Goog-Visitor-Id literal the supplying browser
// uses: the value youtube.com puts in ytcfg.VISITOR_DATA, URL-escaped (the
// "...%3D%3D" form). WaxTap re-sends it verbatim as the visitor-id header, the
// InnerTube context visitorData, and the GVS token's content_binding, with no
// escape or unescape, so coherence holds to the byte. Cookies are the Set-Cookie
// values issued with that visitorData; they must be a logged-out guest session
// (a logged-in visitorData is account-bound).
type Session struct {
	VisitorData string
	Cookies     []*http.Cookie
}

// SessionProvider supplies a guest Session on demand. WaxTap resolves it once per
// Client and reuses the result for that Client's lifetime, so the adopted
// visitorData never changes mid-extraction; long-running services should recreate
// the Client per task to pick up a fresh session. Implementations must honor ctx
// cancellation and should be safe for concurrent use.
type SessionProvider interface {
	ProvideSession(ctx context.Context) (Session, error)
}

// HTTPFailure is a compact snapshot of the HTTP failure that triggered a token
// refresh, for diagnosis by the provider.
type HTTPFailure struct {
	StatusCode int
	Status     string
	URL        string
	Body       string // truncated response body
}
