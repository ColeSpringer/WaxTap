package youtube

import "testing"

// TestAndroidVRFingerprint guards the android_vr request shape. The device and
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
