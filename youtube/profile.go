package youtube

import (
	"maps"
	"strconv"

	"github.com/colespringer/waxtap/internal/clientident"
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

	// Device and OS fields sent in the InnerTube client context. Mobile and VR
	// clients need these populated; sparse identities are more likely to hit the
	// bot-check path. Zero values are omitted from the request.
	DeviceMake        string
	DeviceModel       string
	OSName            string
	OSVersion         string
	AndroidSDKVersion int

	// RequiresPOTokens lists the PO-token scopes this client must supply: a
	// player-scope token in the /player body, a GVS token on the stream URL,
	// or both. Empty means none. NewClientProfile clones and canonicalizes
	// the slice.
	RequiresPOTokens  []potoken.Scope
	SupportsCookies   bool
	SupportsPlaylists bool

	// NeedsSignatureTimestamp indicates that /player requests for this profile
	// must include the signature timestamp from base.js. Profiles that return
	// direct URLs should leave it false to avoid loading base.js during extraction.
	NeedsSignatureTimestamp bool

	headers map[string]string // owned; cloned on construct and on read
}

// NewClientProfile returns base with headers deep-copied in, yielding a profile
// that owns an isolated header map callers cannot mutate.
func NewClientProfile(base ClientProfile, headers map[string]string) ClientProfile {
	base.headers = cloneStringMap(headers)
	base.RequiresPOTokens = canonicalizeScopes(base.RequiresPOTokens)
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

// requiresPOToken reports whether the profile needs a PO token for scope s.
// ScopeNone is a zero-value sentinel, not a required scope, so it returns false
// even if malformed input reached the profile.
func (p ClientProfile) requiresPOToken(s potoken.Scope) bool {
	if s == potoken.ScopeNone {
		return false
	}
	for _, sc := range p.RequiresPOTokens {
		if sc == s {
			return true
		}
	}
	return false
}

// canonicalizeScopes returns an owned, order-preserving copy with ScopeNone and
// duplicates removed. The profile constructor cannot return an error, so it
// normalizes programmatic input: {ScopeNone, ScopePlayer} becomes {ScopePlayer},
// and an empty or all-ScopeNone input yields nil.
func canonicalizeScopes(scopes []potoken.Scope) []potoken.Scope {
	if len(scopes) == 0 {
		return nil
	}
	out := make([]potoken.Scope, 0, len(scopes))
	seen := make(map[potoken.Scope]bool, len(scopes))
	for _, s := range scopes {
		if s == potoken.ScopeNone || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// innerTubeOrigin is the Origin header value used for InnerTube requests.
const innerTubeOrigin = "https://www.youtube.com"

// Base profiles omit headers. makeProfile derives request headers from these
// fields so the JSON client context and HTTP headers stay in sync. In
// particular, X-Youtube-Client-Name comes from InnerTubeID.
//
// These values change when YouTube rotates clients. Callers can supply
// Config.Profiles to override the built-ins.
var (
	profileAndroidVR = ClientProfile{
		Name:          "ANDROID_VR",
		InnerTubeName: "ANDROID_VR",
		InnerTubeID:   28,
		Version:       "1.65.10",
		// The built-ins use keyless InnerTube POSTs. APIKey remains configurable
		// for caller-supplied profiles.
		APIKey:    "",
		UserAgent: "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip",
		// Oculus Quest fingerprint used by the android_vr InnerTube client.
		DeviceMake:        "Oculus",
		DeviceModel:       "Quest 3",
		OSName:            "Android",
		OSVersion:         "12L",
		AndroidSDKVersion: 32,
		SupportsPlaylists: false,
	}
	profileIOS = ClientProfile{
		Name:              "IOS",
		InnerTubeName:     "IOS",
		InnerTubeID:       5,
		Version:           "19.45.4",
		UserAgent:         "com.google.ios.youtube/19.45.4 (iPhone16,2; U; CPU iOS 18_1_0 like Mac OS X;)",
		DeviceModel:       "iPhone16,2",
		SupportsPlaylists: false,
	}
	profileWebEmbedded = ClientProfile{
		Name:                    "WEB_EMBEDDED_PLAYER",
		InnerTubeName:           "WEB_EMBEDDED_PLAYER",
		InnerTubeID:             56,
		Version:                 clientident.WebEmbeddedVersion,
		UserAgent:               clientident.UserAgent(0),
		SupportsPlaylists:       false,
		NeedsSignatureTimestamp: true,
	}
	profileWeb = ClientProfile{
		Name:          "WEB",
		InnerTubeName: "WEB",
		InnerTubeID:   1,
		Version:       clientident.WebVersion,
		UserAgent:     clientident.UserAgent(0),
		// WEB requires both PO-token scopes: player for the /player body and GVS
		// for the stream URL.
		RequiresPOTokens:        []potoken.Scope{potoken.ScopePlayer, potoken.ScopeGVS},
		SupportsPlaylists:       true,
		NeedsSignatureTimestamp: true,
	}
)

// BuildProfile derives the static InnerTube request headers from base and
// returns an isolated profile. Use it for configured profiles so headers such as
// X-Youtube-Client-Name stay tied to the scalar client identity.
func BuildProfile(base ClientProfile) ClientProfile {
	// Accept-Language is intentionally not set here; it is applied per request
	// from the configured locale (see Client.hl / acceptLanguage).
	headers := map[string]string{
		"User-Agent":               base.UserAgent,
		"X-Youtube-Client-Name":    strconv.Itoa(base.InnerTubeID),
		"X-Youtube-Client-Version": base.Version,
		"Origin":                   innerTubeOrigin,
		"Content-Type":             "application/json",
		"Accept":                   "*/*",
	}
	return NewClientProfile(base, headers)
}

func makeProfile(base ClientProfile) ClientProfile { return BuildProfile(base) }

// DefaultProfiles returns the ordered client strategy chain using the default
// built-in Chrome identity. ANDROID_VR leads because it usually returns direct
// URLs without a PO token; the embedded and WEB clients cover fallback cases.
func DefaultProfiles() []ClientProfile {
	return buildDefaultProfiles(clientident.UserAgent(0))
}

// buildDefaultProfiles returns the default strategy chain after applying webUA
// to the WEB-family profiles.
func buildDefaultProfiles(webUA string) []ClientProfile {
	web := profileWeb
	web.UserAgent = webUA
	embedded := profileWebEmbedded
	embedded.UserAgent = webUA
	return []ClientProfile{
		makeProfile(profileAndroidVR),
		makeProfile(profileIOS),
		makeProfile(embedded),
		makeProfile(web),
	}
}
