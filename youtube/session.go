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

// newSession starts a per-attempt session. The consent ID is randomized so
// repeated calls do not reuse the exact same CONSENT cookie value; visitorData
// is filled after the first response that provides it.
func newSession() *session {
	return &session{consentID: strconv.Itoa(rand.IntN(900) + 100)}
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
