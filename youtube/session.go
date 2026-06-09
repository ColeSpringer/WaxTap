package youtube

import (
	rand "math/rand/v2"
	"strconv"
)

// visitorSource records where a session's visitorData came from. It is separate
// from the mint lock (potBound): the mint lock pins a value for one attempt, while
// an adopted source pins an externally supplied value for the whole Client.
type visitorSource uint8

const (
	visitorSynthetic    visitorSource = iota // locally generated placeholder
	visitorBootstrapped                      // learned from a YouTube page / response
	visitorAdopted                           // supplied by the caller; never overwritten
)

func (v visitorSource) String() string {
	switch v {
	case visitorBootstrapped:
		return "bootstrapped"
	case visitorAdopted:
		return "adopted"
	default:
		return "synthetic"
	}
}

// session is mutable per-attempt extraction state. It stays unexported so
// cookies, visitor data, and PO-token state can evolve without changing the
// public API.
type session struct {
	visitorData string
	consentID   string
	// potBound prevents visitorData from changing after a PO token is minted.
	potBound bool
	// source records the provenance of visitorData; an adopted value is never
	// overwritten or cleared (see learnVisitorData / resetPOBinding).
	source visitorSource
}

// newSession starts a per-attempt session for the given content region. It
// pre-seeds synthetic visitorData so the first player request carries a visitor
// identity; learnVisitorData replaces it when YouTube returns a server-issued
// value.
func newSession(countryCode string) *session {
	return &session{
		visitorData: generateVisitorData(countryCode),
		consentID:   strconv.Itoa(rand.IntN(900) + 100),
		source:      visitorSynthetic,
	}
}

// consentCookieValue is the CONSENT cookie value sent on YouTube requests.
func (s *session) consentCookieValue() string {
	return "YES+cb.20210328-17-p0.en+FX+" + s.consentID
}

// learnVisitorData adopts a server-issued visitorData (from a homepage bootstrap
// or a player/browse response). It does nothing when a PO token has bound the
// session to its current value, or when the value was externally adopted: an
// adopted visitorData must reach the token minter byte-for-byte, so a later
// response must not replace it.
func (s *session) learnVisitorData(visitorData string) {
	if visitorData == "" || s.potBound || s.source == visitorAdopted {
		return
	}
	s.visitorData = visitorData
	s.source = visitorBootstrapped
}

// adoptVisitorData pins an externally supplied visitorData. It is used verbatim
// everywhere (visitor-id header, InnerTube context, and the GVS token's
// content_binding) and is protected from later overwrites by the adopted source.
func (s *session) adoptVisitorData(visitorData string) {
	s.visitorData = visitorData
	s.source = visitorAdopted
}

// bindPOToken prevents later responses from replacing visitorData.
func (s *session) bindPOToken() {
	s.potBound = true
}

// resetPOBinding allows the next extraction attempt to adopt a new visitorData.
// It clears only the per-attempt mint lock; an adopted source is left intact.
func (s *session) resetPOBinding() {
	s.potBound = false
}
