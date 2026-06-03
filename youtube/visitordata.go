package youtube

import (
	"encoding/base64"
	"encoding/binary"
	rand "math/rand/v2"
	"net/url"
	"time"
)

// YouTube's visitorData is a base64url-encoded Protocol Buffers payload that
// identifies a logged-out visitor. Mobile and VR InnerTube clients increasingly
// reject player requests without visitorData, returning a generic ERROR instead
// of formats. WaxTap sends a synthetic visitorData on the first request, then
// replaces it with the server-issued value when YouTube returns one.
//
// The synthetic value is not an account identifier; it follows the logged-out
// visitor shape produced by a fresh browser session.

// visitorNonceAlphabet is the character set for the 11-character page-load nonce
// embedded in visitorData.
const visitorNonceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// protoBuf accumulates a Protocol Buffers message in wire format. Only the two
// wire types visitorData needs are implemented: varint (0) and length-delimited
// (2).
type protoBuf struct {
	b []byte
}

// putTag writes a field's wire tag (field number + wire type).
func (p *protoBuf) putTag(field int, wireType byte) {
	p.b = binary.AppendUvarint(p.b, uint64(field)<<3|uint64(wireType))
}

// putVarint writes a varint-typed field. Values are non-negative here.
func (p *protoBuf) putVarint(field int, v int64) {
	p.putTag(field, 0)
	p.b = binary.AppendUvarint(p.b, uint64(v))
}

// putBytes writes a length-delimited field.
func (p *protoBuf) putBytes(field int, v []byte) {
	p.putTag(field, 2)
	p.b = binary.AppendUvarint(p.b, uint64(len(v)))
	p.b = append(p.b, v...)
}

// putString writes a length-delimited field from a string.
func (p *protoBuf) putString(field int, v string) {
	p.putBytes(field, []byte(v))
}

// generateVisitorData builds synthetic visitorData for the given content region,
// defaulting to US. The payload mirrors a logged-out browser token: page-load
// nonce, recent timestamp, and geo block.
func generateVisitorData(countryCode string) string {
	if countryCode == "" {
		countryCode = "US"
	}

	var geoDetail protoBuf
	geoDetail.putString(2, "")
	geoDetail.putVarint(4, int64(rand.IntN(255)+1))

	var geo protoBuf
	geo.putString(1, countryCode)
	geo.putBytes(2, geoDetail.b)

	var msg protoBuf
	msg.putString(1, randomNonce(11))
	// Keep the timestamp recent, like a first page load from this week.
	msg.putVarint(5, time.Now().Unix()-int64(rand.IntN(600000)))
	msg.putBytes(6, geo.b)

	return url.QueryEscape(base64.URLEncoding.EncodeToString(msg.b))
}

func randomNonce(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = visitorNonceAlphabet[rand.IntN(len(visitorNonceAlphabet))]
	}
	return string(b)
}
