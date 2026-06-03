// Package resolver isolates YouTube's most volatile surface: discovering the
// player JS (base.js), solving the signature and n-parameter transforms, and
// building a playable, signed stream URL. Everything that breaks when YouTube
// changes its cipher lives behind the Resolver interface here.
//
// It is an internal package: by Go's internal rule it is importable only under
// youtube/. It owns its own input/output value types and never imports the
// youtube package, so the dependency runs strictly youtube -> resolver with no
// cycle. The youtube package maps these types into its public
// youtube.ResolvedStream.
package resolver

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/colespringer/waxtap/potoken"
)

// Context carries everything the resolver needs to turn a candidate into a
// playable URL. It holds only primitive/stable data, not youtube's internal
// ClientProfile or session, so the resolver stays decoupled from youtube.
type Context struct {
	VideoID   string
	PlayerURL string      // base.js URL, if already discovered
	Headers   http.Header // request headers derived from the winning client profile
	Token     *Token      // supplied PO token, if any (v1: externally provided)
}

// Token is a resolved PO token plus any header/query additions it requires on
// the stream request. It is the resolver-internal projection of a
// potoken.Response.
type Token struct {
	Scope   potoken.Scope
	Value   string
	Headers http.Header
	Query   url.Values
	Expires time.Time
}

// Candidate identifies the stream to resolve. A player response encodes each
// format with either a direct URL or a signatureCipher bundle that must be
// deciphered; exactly one is typically set.
type Candidate struct {
	URL             string // direct URL, if present
	SignatureCipher string // raw signatureCipher bundle, if the URL must be built
}

// Stream is a resolved, playable stream URL with the metadata needed to fetch
// it. The youtube package maps this into youtube.ResolvedStream.
type Stream struct {
	URL           string
	ExpiresAt     time.Time
	ContentLength int64
	Headers       http.Header
}

// Resolver turns a Candidate into a playable Stream, isolating all cipher and
// base.js volatility. Implementations must locate and cache the player program,
// solve the signature and n-parameter, attach any PO token, and return the
// signed URL.
type Resolver interface {
	Resolve(ctx context.Context, rc Context, candidate Candidate) (Stream, error)
}
