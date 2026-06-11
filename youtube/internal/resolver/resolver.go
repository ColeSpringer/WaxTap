// Package resolver isolates YouTube's volatile player JavaScript. It discovers
// base.js, reads player metadata, solves the signature and n-parameter
// transforms, and builds playable stream URLs.
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
	VideoID   string      // video whose stream is being resolved
	PlayerURL string      // base.js URL, if already discovered
	Headers   http.Header // request headers derived from the winning client profile
	Token     *Token      // supplied PO token, if any
}

// Token is a resolved PO token plus any header/query additions it requires on
// the stream request. It is the resolver-internal projection of a
// potoken.Response.
type Token struct {
	Scope   potoken.Scope // token scope
	Value   string        // token value
	Headers http.Header   // headers to add to the stream request
	Query   url.Values    // query parameters to add to the stream URL
	Expires time.Time     // token expiry, or zero when unknown
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
	URL           string      // signed playable URL
	ExpiresAt     time.Time   // URL expiry, or zero when unknown
	ContentLength int64       // bytes, or 0 when unknown
	Headers       http.Header // headers required when fetching URL
}

// Resolver turns a Candidate into a playable Stream. Implementations must locate
// and cache the player program, solve the signature and n-parameter, attach any
// PO token, and return the signed URL.
type Resolver interface {
	// Resolve builds a playable stream from candidate.
	Resolve(ctx context.Context, rc Context, candidate Candidate) (Stream, error)
}

// PlayerInspector exposes base.js metadata needed before stream resolution. It
// is separate from Resolver so injected resolvers do not need to support player
// inspection.
type PlayerInspector interface {
	// SignatureTimestamp returns the signature timestamp embedded in base.js.
	// rc.PlayerURL selects the player directly; otherwise discovery starts from
	// rc.VideoID. A compiled player without a recognized timestamp returns zero.
	SignatureTimestamp(ctx context.Context, rc Context) (int, error)
	// DescrambleN rewrites a stream URL's throttling n parameter using base.js.
	// rc identifies the player to use. URLs without an n parameter are returned
	// unchanged.
	DescrambleN(ctx context.Context, rc Context, rawURL string) (string, error)
}

// SourceCache stores base.js source between process runs. Player uses it behind
// the in-memory compiled-program cache; misses and write failures fall back to
// network fetches. Implementations must be safe for concurrent use. A nil
// SourceCache disables persistence.
type SourceCache interface {
	// Get returns cached source for key.
	Get(key string) ([]byte, bool)
	// Put stores source under key.
	Put(key string, data []byte)
}
