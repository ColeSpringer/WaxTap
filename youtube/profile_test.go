package youtube

import (
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/potoken"
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

func TestDefaultChainOrder(t *testing.T) {
	// VISIONOS leads: since 2026-08-17 the server refuses ANDROID_VR delivery of
	// videos longer than about a minute from the first byte (yt-dlp dropped the
	// client from its defaults for the same reason), while VISIONOS is token-free
	// and delivers in full. ANDROID_VR stays as a fallback; it still serves
	// short videos and the gate is a server experiment that may move again.
	want := []string{"VISIONOS", "ANDROID_VR", "WEB", "IOS", "WEB_EMBEDDED_PLAYER"}
	got := make([]string, 0, len(DefaultProfiles()))
	for _, p := range DefaultProfiles() {
		got = append(got, p.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("default chain order = %v, want %v", got, want)
	}
}

// TestVisionOSFingerprint checks the visionos request shape: the lead
// token-free client, identity verified against yt-dlp INNERTUBE_CLIENTS on
// 2026-08-31. Bump Version, UserAgent, and OSVersion together when refreshing.
func TestVisionOSFingerprint(t *testing.T) {
	p := DefaultProfiles()[0]
	if p.Name != "VISIONOS" {
		t.Fatalf("first profile = %q, want VISIONOS (the no-token lead client)", p.Name)
	}
	if p.InnerTubeID != 101 {
		t.Errorf("VISIONOS InnerTubeID = %d, want 101", p.InnerTubeID)
	}
	if len(p.RequiresPOTokens) != 0 {
		t.Errorf("VISIONOS requires PO tokens %v, want none (the whole point of the client)", p.RequiresPOTokens)
	}
	if p.NeedsSignatureTimestamp {
		t.Error("VISIONOS needs a signature timestamp; it is a native client and must not")
	}

	ictx := New(Config{}).newInnertubeContext(makeProfile(profileVisionOS), newSession("US"))
	if ictx.Client.DeviceMake != "Apple" ||
		ictx.Client.DeviceModel != "RealityDevice17,1" ||
		ictx.Client.OSName != "visionOS" ||
		ictx.Client.OSVersion == "" {
		t.Errorf("visionos InnerTube context missing fingerprint: %+v", ictx.Client)
	}
	if ictx.Client.AndroidSDKVersion != 0 {
		t.Errorf("visionos context carries androidSdkVersion %d; an Apple device must not", ictx.Client.AndroidSDKVersion)
	}
}

// TestAndroidVRFingerprint checks the android_vr request shape. The device and
// OS fields must stay populated and must reach the InnerTube body context.
func TestAndroidVRFingerprint(t *testing.T) {
	p := profileByName(t, "ANDROID_VR")
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

// TestIOSRequiresNoPOTokens locks iOS to needing no PO tokens. A live --client ios
// check confirmed unrestricted public videos stream tokenless, and the GVS token
// iOS wants is iOSGuard-attested, not mintable by the BotGuard/web providers WaxTap
// integrates, so requiring it would only block the token-free path behind a token
// no web minter can supply. iOS must not require a player token either: Extract
// fetches player tokens up front, so that would gate extraction itself.
func TestIOSRequiresNoPOTokens(t *testing.T) {
	ios := profileByName(t, "IOS")
	if ios.requiresPOToken(potoken.ScopeGVS) {
		t.Error("IOS must not require a GVS PO token: it streams public videos tokenless, and iOSGuard tokens are not mintable by WaxTap's web providers")
	}
	if ios.requiresPOToken(potoken.ScopePlayer) {
		t.Error("IOS must not require a player PO token (that would break extraction)")
	}
	if len(ios.RequiresPOTokens) != 0 {
		t.Errorf("IOS RequiresPOTokens = %v, want none", ios.RequiresPOTokens)
	}
}

// TestWebEmbeddedRequiresBothPOTokens keeps the embedded client aligned with the
// WEB-family token model: player tokens gate /player, and GVS tokens gate the
// media request when /player returns playable formats.
func TestWebEmbeddedRequiresBothPOTokens(t *testing.T) {
	emb := profileByName(t, "WEB_EMBEDDED_PLAYER")
	if !emb.requiresPOToken(potoken.ScopePlayer) {
		t.Errorf("WEB_EMBEDDED_PLAYER should require a player PO token, got %v", emb.RequiresPOTokens)
	}
	if !emb.requiresPOToken(potoken.ScopeGVS) {
		t.Errorf("WEB_EMBEDDED_PLAYER should require a GVS PO token (WEB-family), got %v", emb.RequiresPOTokens)
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

// A forced --client visionos builds a one-element chain, like every other
// built-in name.
func TestBuildClientChainVisionOS(t *testing.T) {
	chain, err := BuildClientChain("visionos", 0)
	if err != nil {
		t.Fatalf("BuildClientChain(visionos): %v", err)
	}
	if len(chain) != 1 || chain[0].Name != "VISIONOS" {
		t.Fatalf("chain = %+v, want exactly the VISIONOS profile", chain)
	}
	if _, err := BuildClientChain("bogus", 0); err == nil || !strings.Contains(err.Error(), "visionos") {
		t.Errorf("unknown-client error = %v, want it to list visionos among the choices", err)
	}
}
