package youtube

import (
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/colespringer/waxtap/v3/internal/clientident"
	"github.com/colespringer/waxtap/v3/potoken"
)

// ClientProfile identifies an InnerTube client such as ANDROID_VR, iOS, or WEB.
// The header map is unexported and every accessor returns a clone, so callers
// cannot mutate a profile after construction.
type ClientProfile struct {
	Name          string // internal name, e.g. "ANDROID_VR"
	InnerTubeName string // InnerTube context clientName, e.g. "ANDROID_VR"
	InnerTubeID   int    // drives X-Youtube-Client-Name (never hardcoded)
	Version       string // InnerTube client version
	APIKey        string // optional InnerTube API key
	UserAgent     string // request User-Agent

	// Device and OS fields sent in the InnerTube client context. Mobile and VR
	// clients need these populated; sparse identities are more likely to hit the
	// bot-check path. Zero values are omitted from the request.
	DeviceMake        string
	DeviceModel       string // client device model
	OSName            string // client operating-system name
	OSVersion         string // client operating-system version
	AndroidSDKVersion int    // Android SDK level, or 0 when not applicable

	// RequiresPOTokens lists the PO-token scopes this client must supply: a
	// player-scope token in the /player body, a GVS token on the stream URL,
	// or both. Empty means none. NewClientProfile clones and canonicalizes
	// the slice.
	RequiresPOTokens  []potoken.Scope
	SupportsCookies   bool // whether cookie-backed sessions are supported
	SupportsPlaylists bool // whether the profile can browse playlists

	// NeedsSignatureTimestamp indicates that /player requests for this profile
	// must include the signature timestamp from base.js. Profiles that return
	// direct URLs should leave it false to avoid loading base.js during extraction.
	NeedsSignatureTimestamp bool

	// EmbedURL is sent as context.thirdParty.embedUrl on /player requests, where
	// WEB_EMBEDDED_PLAYER needs a third-party embed origin (not youtube.com) or it
	// returns a playability ERROR.
	EmbedURL string

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
	profileVisionOS = ClientProfile{
		Name:          "VISIONOS",
		InnerTubeName: "VISIONOS",
		InnerTubeID:   101,
		Version:       "1.02",
		UserAgent:     "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15",
		// Apple Vision Pro fingerprint used by the visionos InnerTube client.
		DeviceMake:        "Apple",
		DeviceModel:       "RealityDevice17,1",
		OSName:            "visionOS",
		OSVersion:         "26.5.23O471",
		SupportsPlaylists: false,
	}
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
	// IOS identity verified against yt-dlp INNERTUBE_CLIENTS on 2026-06-07. Bump
	// Version and UserAgent together: a stale iOS app version draws a 400 from
	// /player, and a sparse device context hits the bot-check path.
	profileIOS = ClientProfile{
		Name:          "IOS",
		InnerTubeName: "IOS",
		InnerTubeID:   5,
		Version:       "21.02.3",
		UserAgent:     "com.google.ios.youtube/21.02.3 (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X;)",
		DeviceMake:    "Apple",
		DeviceModel:   "iPhone16,2",
		OSName:        "iPhone",
		OSVersion:     "18.3.2.22D82",
		// No PO-token scopes. The GVS token iOS wants is iOSGuard-attested, but the
		// bgutil/WaxSeal-style providers WaxTap integrates mint a BotGuard/web token
		// iOS GVS cannot use, so requiring GVS would only block iOS behind a token no
		// web minter can supply. A live `--client ios` check (2026-06-09) confirmed
		// iOS streams unrestricted public videos with no token; a restricted video
		// returns a clear download-time 403. Not ScopePlayer either: Extract fetches
		// player tokens up front, so that would gate extraction. (yt-dlp marks the iOS
		// player token recommended-not-required, GVS not_required_with_player_token.)
		SupportsPlaylists: false,
		// Native client: no base.js signature timestamp, so NeedsSignatureTimestamp
		// stays unset (yt-dlp REQUIRE_JS_PLAYER false).
	}
	profileWebEmbedded = ClientProfile{
		Name:          "WEB_EMBEDDED_PLAYER",
		InnerTubeName: "WEB_EMBEDDED_PLAYER",
		InnerTubeID:   56,
		Version:       clientident.WebEmbeddedVersion,
		UserAgent:     clientident.UserAgent(0),
		// Embed origin matches yt-dlp's choice (reddit.com).
		EmbedURL: "https://www.reddit.com/",
		// web_embedded is WEB-family and uses both PO-token scopes. Player tokens make
		// a forced no-token client fail at token acquisition instead of sending a
		// tokenless /player request. GVS tokens cover the later stream request when
		// /player does return playable formats.
		RequiresPOTokens:        []potoken.Scope{potoken.ScopePlayer, potoken.ScopeGVS},
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
// built-in Chrome identity. Token-free clients and clients capable of full-track
// delivery are tried before clients with observed range or token restrictions.
func DefaultProfiles() []ClientProfile {
	return buildDefaultProfiles(clientident.UserAgent(0))
}

// BuildClientChain returns a one-element strategy chain for a single built-in
// client, forcing it as the whole chain instead of the default multi-client
// fallback. WEB-family clients receive the same User-Agent / Chrome-major
// treatment buildDefaultProfiles applies; native clients keep their own
// User-Agent (chromeMajor does not apply to them). An unknown name is an error.
//
// A single forced client is trivially a uniform chain, which is what external
// session adoption requires.
func BuildClientChain(name string, chromeMajor int) ([]ClientProfile, error) {
	webUA := clientident.UserAgent(chromeMajor)
	base := profileWeb
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web":
		base = profileWeb
		base.UserAgent = webUA
	case "web_embedded", "web_embedded_player":
		base = profileWebEmbedded
		base.UserAgent = webUA
	case "ios":
		base = profileIOS
	case "android_vr":
		base = profileAndroidVR
	case "visionos":
		base = profileVisionOS
	default:
		return nil, fmt.Errorf("unknown client %q (want one of: web, ios, android_vr, visionos, web_embedded)", name)
	}
	return []ClientProfile{makeProfile(base)}, nil
}

// buildDefaultProfiles returns the default strategy chain after applying webUA
// to the WEB-family profiles.
func buildDefaultProfiles(webUA string) []ClientProfile {
	web := profileWeb
	web.UserAgent = webUA
	embedded := profileWebEmbedded
	embedded.UserAgent = webUA
	return []ClientProfile{
		makeProfile(profileVisionOS),
		makeProfile(profileAndroidVR),
		makeProfile(web),
		makeProfile(profileIOS),
		makeProfile(embedded),
	}
}

// adoptedProfile applies an adopted session's browser identity to a browser
// profile, the way webContextProfile applies a context's version: the profile is
// rebuilt through makeProfile so the User-Agent and X-Youtube-Client-Version
// headers follow the scalars (see doc.go: profiles are immutable, derive, never
// mutate).
//
// The user agent applies to WEB and WEB_EMBEDDED_PLAYER; the client version to
// WEB alone, since it is the WEB player's. Native clients and sessions without an
// identity return profile unchanged. An adopted identity outranks
// Options.ChromeMajor and a WEB profile from a ProfileOverridePath: the cookies,
// the visitor id, and the token all belong to that browser, so the requests
// carrying them have to present it too.
func adoptedProfile(profile ClientProfile, sess *session) ClientProfile {
	if sess == nil || sess.source != visitorAdopted || (sess.userAgent == "" && sess.clientVersion == "") {
		return profile
	}
	isWeb := profile.InnerTubeName == profileWeb.InnerTubeName
	if !isWeb && !isWebEmbedded(profile) {
		return profile
	}
	base := profile
	changed := false
	if sess.userAgent != "" {
		base.UserAgent = sess.userAgent
		changed = true
	}
	if isWeb && sess.clientVersion != "" {
		base.Version = sess.clientVersion
		changed = true
	}
	if !changed {
		return profile
	}
	return makeProfile(base)
}
