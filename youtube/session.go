package youtube

import (
	rand "math/rand/v2"
	"strconv"
)

// session is mutable per-attempt extraction state. It stays unexported so
// cookies, visitor data, and PO-token state can evolve without changing the
// public API.
type session struct {
	visitorData string
	consentID   string
	// potBound prevents visitorData from changing after a PO token is minted.
	potBound bool
}

// newSession starts a per-attempt session for the given content region. It
// pre-seeds synthetic visitorData so the first player request carries a visitor
// identity; learnVisitorData replaces it when YouTube returns a server-issued
// value.
func newSession(countryCode string) *session {
	return &session{
		visitorData: generateVisitorData(countryCode),
		consentID:   strconv.Itoa(rand.IntN(900) + 100),
	}
}

// consentCookieValue is the CONSENT cookie value sent on YouTube requests.
func (s *session) consentCookieValue() string {
	return "YES+cb.20210328-17-p0.en+FX+" + s.consentID
}

// learnVisitorData adopts a server-issued visitorData unless a PO token has
// already bound the session to its current value.
func (s *session) learnVisitorData(visitorData string) {
	if visitorData != "" && !s.potBound {
		s.visitorData = visitorData
	}
}

// bindPOToken prevents later responses from replacing visitorData.
func (s *session) bindPOToken() {
	s.potBound = true
}

// resetPOBinding allows the next extraction attempt to adopt a new visitorData.
func (s *session) resetPOBinding() {
	s.potBound = false
}
