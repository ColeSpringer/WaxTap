package resolver

import (
	"net/http"
	"net/url"
)

// This file applies already-resolved PO tokens to stream requests. Deciding when
// a token is needed, and calling the provider, stays in the youtube package
// because it owns the client profile and session context.

// poTokenQueryParam is the query parameter googlevideo expects a GVS proof-of-
// origin token under.
const poTokenQueryParam = "pot"

// applyToken attaches a PO token to a stream URL's query and request headers: the
// token value as the `pot` parameter, plus any provider-supplied query parameters
// and headers (which override on conflict). It is a no-op for a nil token.
func applyToken(q url.Values, headers http.Header, tok *Token) {
	if tok == nil {
		return
	}
	if tok.Value != "" {
		q.Set(poTokenQueryParam, tok.Value)
	}
	for k, vs := range tok.Query {
		q[k] = append([]string(nil), vs...)
	}
	for k, vs := range tok.Headers {
		headers[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
}
