package youtube

import (
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/potoken"
)

// profileByName returns the named profile from the default strategy chain.
func profileByName(t *testing.T, name string) ClientProfile {
	t.Helper()
	for _, p := range DefaultProfiles() {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("%s profile not present in the default chain", name)
	return ClientProfile{}
}

// TestAndroidVRFingerprint checks the android_vr request shape. The device and
// OS fields must stay populated and must reach the InnerTube body context.
func TestAndroidVRFingerprint(t *testing.T) {
	p := DefaultProfiles()[0]
	if p.Name != "ANDROID_VR" {
		t.Fatalf("first profile = %q, want ANDROID_VR (the no-token lead client)", p.Name)
	}
	if p.AndroidSDKVersion == 0 || p.DeviceMake == "" || p.DeviceModel == "" || p.OSName == "" || p.OSVersion == "" {
		t.Fatalf("ANDROID_VR profile missing device fingerprint: %+v", p)
	}

	// The fingerprint must actually reach the request body context.
	ictx := New(Config{}).newInnertubeContext(makeProfile(profileAndroidVR), newSession("US"))
	if ictx.Client.AndroidSDKVersion != 32 ||
		ictx.Client.DeviceMake != "Oculus" ||
		ictx.Client.DeviceModel != "Quest 3" ||
		ictx.Client.OSName != "Android" ||
		ictx.Client.OSVersion != "12L" {
		t.Errorf("android_vr InnerTube context missing fingerprint: %+v", ictx.Client)
	}
}

// TestIOSFingerprint checks the iOS request shape. A stale version or a sparse
// device context draws a 400 from /player, so the identity fields must stay
// populated and reach the InnerTube body context.
func TestIOSFingerprint(t *testing.T) {
	ios := profileByName(t, "IOS")
	if ios.DeviceMake == "" || ios.DeviceModel == "" || ios.OSName == "" || ios.OSVersion == "" {
		t.Fatalf("IOS profile missing device fingerprint: %+v", ios)
	}
	// The version must stay embedded in the user agent; they are bumped together.
	if !strings.Contains(ios.UserAgent, ios.Version) {
		t.Errorf("IOS UserAgent %q does not embed version %q", ios.UserAgent, ios.Version)
	}

	// The stable identity must reach the request body context, and the volatile
	// version/osVersion must carry through unchanged.
	ictx := New(Config{}).newInnertubeContext(makeProfile(profileIOS), newSession("US"))
	if ictx.Client.DeviceMake != "Apple" ||
		ictx.Client.DeviceModel != "iPhone16,2" ||
		ictx.Client.OSName != "iPhone" {
		t.Errorf("iOS InnerTube context missing fingerprint: %+v", ictx.Client)
	}
	if ictx.Client.ClientVersion != profileIOS.Version || ictx.Client.OSVersion != profileIOS.OSVersion {
		t.Errorf("iOS context version/osVersion did not carry through: %+v", ictx.Client)
	}
}

// TestIOSRequiresGVSOnly checks that iOS requires a GVS token for its media fetch
// but not a player token. A player-scope requirement would gate extraction itself
// (Extract fetches player tokens), re-breaking iOS when no provider is configured.
func TestIOSRequiresGVSOnly(t *testing.T) {
	ios := profileByName(t, "IOS")
	if !ios.requiresPOToken(potoken.ScopeGVS) {
		t.Error("IOS must require a GVS PO token for its media fetch")
	}
	if ios.requiresPOToken(potoken.ScopePlayer) {
		t.Error("IOS must not require a player PO token (that would break extraction)")
	}
}

// TestWebEmbeddedRequiresNoPOTokens locks the embedded client to needing no PO
// tokens (matching yt-dlp). If YouTube starts requiring one, this fails and forces
// an explicit decision instead of a silent breakage.
func TestWebEmbeddedRequiresNoPOTokens(t *testing.T) {
	emb := profileByName(t, "WEB_EMBEDDED_PLAYER")
	if emb.requiresPOToken(potoken.ScopePlayer) || emb.requiresPOToken(potoken.ScopeGVS) {
		t.Errorf("WEB_EMBEDDED_PLAYER should require no PO tokens, got %v", emb.RequiresPOTokens)
	}
	// The embed origin must be present, or /player returns a playability ERROR.
	if emb.EmbedURL == "" {
		t.Error("WEB_EMBEDDED_PLAYER must carry an EmbedURL")
	}
}

// TestWebRequiresBothPOTokenGates checks that WEB declares both PO-token scopes:
// one for the /player body and one for the stream URL.
func TestWebRequiresBothPOTokenGates(t *testing.T) {
	var web ClientProfile
	for _, p := range DefaultProfiles() {
		if p.Name == "WEB" {
			web = p
		}
	}
	if web.Name == "" {
		t.Fatal("WEB profile not present in the default chain")
	}
	if !web.requiresPOToken(potoken.ScopePlayer) || !web.requiresPOToken(potoken.ScopeGVS) {
		t.Errorf("WEB RequiresPOTokens = %v, want both player and gvs", web.RequiresPOTokens)
	}
}

func TestCanonicalizeScopes(t *testing.T) {
	cases := []struct {
		name string
		in   []potoken.Scope
		want []potoken.Scope
	}{
		{"nil", nil, nil},
		{"empty", []potoken.Scope{}, nil},
		{"only none", []potoken.Scope{potoken.ScopeNone}, nil},
		{"drops none", []potoken.Scope{potoken.ScopeNone, potoken.ScopePlayer}, []potoken.Scope{potoken.ScopePlayer}},
		{"dedupes", []potoken.Scope{potoken.ScopeGVS, potoken.ScopeGVS}, []potoken.Scope{potoken.ScopeGVS}},
		{"preserves order", []potoken.Scope{potoken.ScopeGVS, potoken.ScopePlayer}, []potoken.Scope{potoken.ScopeGVS, potoken.ScopePlayer}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalizeScopes(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("canonicalizeScopes(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestCanonicalizeScopes_DoesNotAliasInput checks that the constructor owns its slice:
// mutating the caller's input must not reach a built profile.
func TestCanonicalizeScopes_DoesNotAliasInput(t *testing.T) {
	in := []potoken.Scope{potoken.ScopePlayer, potoken.ScopeGVS}
	got := canonicalizeScopes(in)
	in[0] = potoken.ScopeNone
	if got[0] != potoken.ScopePlayer {
		t.Errorf("result aliases input: got[0] = %v after mutating input", got[0])
	}
}
