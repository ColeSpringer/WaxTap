// Package potoken defines the PO-token provider contract.
//
// PO ("proof of origin") tokens are sometimes required on YouTube stream URLs.
// They are bound to a specific video and expire; a missing or expired token
// usually surfaces as HTTP 403 on the stream URL. WaxTap accepts caller-supplied
// providers and invokes them when a token refresh is needed. It does not include
// a built-in token generator.
//
// This is a leaf package (standard library only) so both the top-level facade
// (which holds the Provider in Options) and the internal resolver (which needs
// the scope/token contract) can depend on it without an import cycle.
package potoken

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Scope identifies which class of URL a token is needed for. Tokens are not
// interchangeable across scopes.
type Scope uint8

const (
	ScopeNone      Scope = iota
	ScopePlayer          // format URLs inside the player response
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

// Request is passed to a Provider when WaxTap needs a token. It carries only
// stable public data, not WaxTap's internal client profile or session.
type Request struct {
	VideoID       string
	ClientName    string // InnerTube client name in play, e.g. "WEB"
	ClientVersion string
	VisitorData   string
	Scope         Scope
	Failure       *HTTPFailure // the 403 that triggered this refresh, if any
}

// Response is what a Provider returns. A token is rarely just a string: it may
// also require additional headers and/or query parameters on the stream
// request, so the provider can supply all three.
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
