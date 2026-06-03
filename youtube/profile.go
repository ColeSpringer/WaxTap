package youtube

import (
	"maps"

	"github.com/colespringer/waxtap/potoken"
)

// ClientProfile identifies an InnerTube client such as ANDROID_VR, iOS, or WEB.
// The header map is unexported and every accessor returns a clone, so callers
// cannot mutate a profile after construction.
type ClientProfile struct {
	Name          string // internal name, e.g. "ANDROID_VR"
	InnerTubeName string // InnerTube context clientName, e.g. "ANDROID_VR"
	InnerTubeID   int    // drives X-Youtube-Client-Name (never hardcoded)
	Version       string
	APIKey        string
	UserAgent     string
	DeviceModel   string

	RequiresPOToken   potoken.Scope
	SupportsCookies   bool
	SupportsPlaylists bool

	headers map[string]string // owned; cloned on construct and on read
}

// NewClientProfile returns base with headers deep-copied in, yielding a profile
// that owns an isolated header map callers cannot mutate.
func NewClientProfile(base ClientProfile, headers map[string]string) ClientProfile {
	base.headers = cloneStringMap(headers)
	return base
}

// Headers returns a copy of the profile's headers; mutating the result does not
// affect the profile.
func (p ClientProfile) Headers() map[string]string { return cloneStringMap(p.headers) }

// Header returns a single header value.
func (p ClientProfile) Header(key string) string { return p.headers[key] }

// WithHeader returns a copy of the profile with key set to value; the receiver
// is unchanged.
func (p ClientProfile) WithHeader(key, value string) ClientProfile {
	next := cloneStringMap(p.headers)
	next[key] = value
	p.headers = next
	return p
}

func cloneStringMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return map[string]string{}
	}
	return maps.Clone(m)
}

// DefaultProfiles returns the built-in ordered client strategy chain. The
// scaffold has no built-in profiles until extraction clients are added.
func DefaultProfiles() []ClientProfile { return nil }
