package youtube

import (
	"slices"
	"testing"

	"github.com/colespringer/waxtap/potoken"
)

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
