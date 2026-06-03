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
	// Reserved for cookies and per-scope PO tokens keyed by potoken.Scope.
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

// learnVisitorData records the visitorData YouTube returned, so continuation and
// follow-up requests in the same attempt present a consistent identity.
func (s *session) learnVisitorData(visitorData string) {
	if visitorData != "" {
		s.visitorData = visitorData
	}
}
