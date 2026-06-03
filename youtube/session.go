package youtube

// session is mutable per-attempt extraction state. It stays unexported so
// cookies, visitor data, and PO-token state can evolve without changing the
// public API.
type session struct {
	visitorData string
	consentID   string
	// Reserved for cookies and per-scope PO tokens keyed by potoken.Scope.
}
