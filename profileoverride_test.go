package waxtap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/potoken"
)

// writeOverride writes content to a temp file and returns its path.
func writeOverride(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validOverride = `{
  "profiles": [
    {
      "name": "ANDROID_VR",
      "innerTubeName": "ANDROID_VR",
      "innerTubeId": 28,
      "version": "1.99.0",
      "userAgent": "com.google.android.apps.youtube.vr.oculus/1.99.0",
      "deviceMake": "Oculus",
      "deviceModel": "Quest 3",
      "requiresPoTokens": [],
      "supportsPlaylists": false
    },
    {
      "name": "WEB",
      "innerTubeName": "WEB",
      "innerTubeId": 1,
      "version": "2.99.0",
      "userAgent": "Mozilla/5.0 web",
      "requiresPoTokens": ["player", "gvs"],
      "supportsPlaylists": true,
      "needsSignatureTimestamp": true
    }
  ]
}`

func TestLoadProfileOverrides_Valid(t *testing.T) {
	profiles, err := loadProfileOverrides(writeOverride(t, validOverride))
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("got %d profiles, want 2", len(profiles))
	}
	// Order is preserved (it is the strategy chain).
	if profiles[0].Name != "ANDROID_VR" || profiles[1].Name != "WEB" {
		t.Fatalf("profile order = %q, %q", profiles[0].Name, profiles[1].Name)
	}
	// Scope names are decoded into the set the client needs.
	if got := profiles[1].RequiresPOTokens; len(got) != 2 || got[0] != potoken.ScopePlayer || got[1] != potoken.ScopeGVS {
		t.Errorf("WEB RequiresPOTokens = %v, want [player gvs]", got)
	}
	// WEB-family clients carry the signature-timestamp flag; a profile that
	// omits it (ANDROID_VR) defaults to false.
	if !profiles[1].NeedsSignatureTimestamp {
		t.Error("WEB NeedsSignatureTimestamp = false, want true")
	}
	if profiles[0].NeedsSignatureTimestamp {
		t.Error("ANDROID_VR NeedsSignatureTimestamp = true, want false (omitted)")
	}
	// Headers are derived (not left to the caller to assemble by hand).
	if got := profiles[0].Header("X-Youtube-Client-Name"); got != "28" {
		t.Errorf("derived X-Youtube-Client-Name = %q, want 28", got)
	}
	if got := profiles[0].Header("User-Agent"); !strings.Contains(got, "1.99.0") {
		t.Errorf("derived User-Agent = %q, want the override UA", got)
	}
}

// TestLoadProfileOverrides_EmbedURL checks that embedUrl reaches the built
// profile so an overridden embedded client can still send thirdParty.embedUrl.
func TestLoadProfileOverrides_EmbedURL(t *testing.T) {
	const override = `{
	  "profiles": [
	    {
	      "name": "WEB_EMBEDDED_PLAYER",
	      "innerTubeName": "WEB_EMBEDDED_PLAYER",
	      "innerTubeId": 56,
	      "version": "1.99.0",
	      "userAgent": "Mozilla/5.0 web",
	      "embedUrl": "https://example.com/",
	      "needsSignatureTimestamp": true
	    }
	  ]
	}`
	profiles, err := loadProfileOverrides(writeOverride(t, override))
	if err != nil {
		t.Fatal(err)
	}
	if got := profiles[0].EmbedURL; got != "https://example.com/" {
		t.Errorf("EmbedURL = %q, want https://example.com/", got)
	}
}

func TestNew_WithProfileOverride(t *testing.T) {
	c, err := New(Options{ProfileOverridePath: writeOverride(t, validOverride)})
	if err != nil {
		t.Fatalf("New with a valid override should succeed: %v", err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
}

func TestLoadProfileOverrides_Errors(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"malformed json", `{not json`},
		{"unknown field", `{"profiles":[{"name":"X","innerTubeName":"X","version":"1","innerTubeId":1,"bogus":true}]}`},
		{"empty list", `{"profiles":[]}`},
		{"missing version", `{"profiles":[{"name":"X","innerTubeName":"X","innerTubeId":1}]}`},
		{"missing innerTubeId", `{"profiles":[{"name":"X","innerTubeName":"X","version":"1"}]}`},
		{"zero innerTubeId", `{"profiles":[{"name":"X","innerTubeName":"X","version":"1","innerTubeId":0}]}`},
		{"bad scope", `{"profiles":[{"name":"X","innerTubeName":"X","version":"1","innerTubeId":1,"requiresPoTokens":["wat"]}]}`},
		{"singular key rejected", `{"profiles":[{"name":"X","innerTubeName":"X","version":"1","innerTubeId":1,"requiresPoToken":"gvs"}]}`},
		{"none combined with real scope", `{"profiles":[{"name":"X","innerTubeName":"X","version":"1","innerTubeId":1,"requiresPoTokens":["none","gvs"]}]}`},
		{"unconsumed scope", `{"profiles":[{"name":"X","innerTubeName":"X","version":"1","innerTubeId":1,"requiresPoTokens":["subtitles"]}]}`},
		{"trailing object", validOverride + " {\"profiles\":[]}"},
		{"trailing garbage", validOverride + " garbage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadProfileOverrides(writeOverride(t, tc.content)); err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
		})
	}
}

// TestLoadProfileOverrides_TypeMismatchMessage checks that top-level shape errors
// and field type errors are reported in config-file terms, not Go struct terms.
func TestLoadProfileOverrides_TypeMismatchMessage(t *testing.T) {
	t.Run("top-level array", func(t *testing.T) {
		_, err := loadProfileOverrides(writeOverride(t, `[]`))
		if err == nil {
			t.Fatal("expected an error for a top-level array")
		}
		msg := err.Error()
		if !strings.Contains(msg, "expected a JSON object, got array") {
			t.Errorf("error = %q, want the clean type-mismatch message", msg)
		}
		if strings.Contains(msg, "profileOverrideFile") || strings.Contains(msg, "Go value") {
			t.Errorf("error = %q, leaks the internal Go type", msg)
		}
	})
	t.Run("mistyped field is not reported as a wrong top-level shape", func(t *testing.T) {
		_, err := loadProfileOverrides(writeOverride(t, `{"profiles":"x"}`))
		if err == nil {
			t.Fatal("expected an error for a mistyped profiles field")
		}
		msg := err.Error()
		if !strings.Contains(msg, `field "profiles" has the wrong type`) {
			t.Errorf("error = %q, want a field-level type message", msg)
		}
		if strings.Contains(msg, "expected a JSON object") {
			t.Errorf("error = %q, a mistyped field is not a wrong top-level shape", msg)
		}
		if strings.Contains(msg, "profileOverrideFile") || strings.Contains(msg, "profileSpec") {
			t.Errorf("error = %q, leaks the internal Go type", msg)
		}
	})
}

func TestLoadProfileOverrides_MissingFile(t *testing.T) {
	if _, err := loadProfileOverrides(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected an error for a missing override file")
	}
}

func TestNew_BadProfileOverrideFailsConstruction(t *testing.T) {
	if _, err := New(Options{ProfileOverridePath: writeOverride(t, `{"profiles":[]}`)}); err == nil {
		t.Fatal("New must fail when the override file is invalid")
	}
}
