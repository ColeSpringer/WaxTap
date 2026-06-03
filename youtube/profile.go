package youtube

import (
	"maps"
	"strconv"

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
		RequiresPOToken:   potoken.ScopeNone,
		SupportsPlaylists: false,
	}
	profileIOS = ClientProfile{
		Name:              "IOS",
		InnerTubeName:     "IOS",
		InnerTubeID:       5,
		Version:           "19.45.4",
		UserAgent:         "com.google.ios.youtube/19.45.4 (iPhone16,2; U; CPU iOS 18_1_0 like Mac OS X;)",
		DeviceModel:       "iPhone16,2",
		RequiresPOToken:   potoken.ScopeNone,
		SupportsPlaylists: false,
	}
	profileWebEmbedded = ClientProfile{
		Name:              "WEB_EMBEDDED_PLAYER",
		InnerTubeName:     "WEB_EMBEDDED_PLAYER",
		InnerTubeID:       56,
		Version:           "1.20250310.01.00",
		UserAgent:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		RequiresPOToken:   potoken.ScopeNone,
		SupportsPlaylists: false,
	}
	profileWeb = ClientProfile{
		Name:              "WEB",
		InnerTubeName:     "WEB",
		InnerTubeID:       1,
		Version:           "2.20250310.01.00",
		UserAgent:         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		RequiresPOToken:   potoken.ScopeGVS, // web stream URLs increasingly need a GVS token
		SupportsPlaylists: true,
	}
)

// makeProfile returns base with its static request headers derived from its
// scalar fields and deep-copied in (so the profile owns an isolated map).
func makeProfile(base ClientProfile) ClientProfile {
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

// DefaultProfiles returns the ordered client strategy chain. ANDROID_VR leads
// because it usually returns direct URLs without a PO token; the embedded and WEB
// clients cover fallback cases.
func DefaultProfiles() []ClientProfile {
	return []ClientProfile{
		makeProfile(profileAndroidVR),
		makeProfile(profileIOS),
		makeProfile(profileWebEmbedded),
		makeProfile(profileWeb),
	}
}

// webProfile returns the built-in WEB profile for watch-page fallback and as a
// last-resort playlist profile.
func webProfile() ClientProfile { return makeProfile(profileWeb) }
