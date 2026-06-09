package waxtap

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/potoken"
)

// singleWEBOverride is a uniform (single-client) override chain, which satisfies
// the adoption uniform-chain requirement.
const singleWEBOverride = `{
  "profiles": [
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

type stubSessionProvider struct{}

func (stubSessionProvider) ProvideSession(context.Context) (potoken.Session, error) {
	return potoken.Session{VisitorData: "Cgt%3D%3D"}, nil
}

func TestNew_AdoptionValidation(t *testing.T) {
	vd := &potoken.Session{VisitorData: "Cgt%3D%3D"}

	// Session and SessionProvider are mutually exclusive.
	if _, err := New(Options{Client: "web", Session: vd, SessionProvider: stubSessionProvider{}}); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("Session+SessionProvider err = %v, want mutual-exclusion", err)
	}

	// Client and ProfileOverridePath are mutually exclusive.
	if _, err := New(Options{Client: "web", ProfileOverridePath: writeOverride(t, singleWEBOverride)}); err == nil ||
		!strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("Client+ProfileOverridePath err = %v, want mutual-exclusion", err)
	}

	// Unknown client name.
	if _, err := New(Options{Client: "nope"}); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Errorf("unknown client err = %v", err)
	}

	// A static Session with an empty VisitorData is rejected (it would break GVS
	// content_binding coherence if silently adopted).
	if _, err := New(Options{Client: "web", Session: &potoken.Session{}}); err == nil ||
		!strings.Contains(err.Error(), "non-empty VisitorData") {
		t.Errorf("empty-VisitorData Session err = %v, want a non-empty-VisitorData rejection", err)
	}

	// Adoption with the default (multi-client) chain is rejected.
	if _, err := New(Options{Session: vd}); err == nil || !strings.Contains(err.Error(), "uniform client chain") {
		t.Errorf("adoption + default chain err = %v, want uniform-chain rejection", err)
	}

	// Adoption with a mixed override chain is rejected.
	if _, err := New(Options{Session: vd, ProfileOverridePath: writeOverride(t, validOverride)}); err == nil ||
		!strings.Contains(err.Error(), "uniform client chain") {
		t.Errorf("adoption + mixed override err = %v, want uniform-chain rejection", err)
	}

	// Adoption with a single forced client is accepted.
	if _, err := New(Options{Session: vd, Client: "web"}); err != nil {
		t.Errorf("adoption + Client=web: %v", err)
	}
	// Adoption with a uniform single-client override is accepted.
	if _, err := New(Options{Session: vd, ProfileOverridePath: writeOverride(t, singleWEBOverride)}); err != nil {
		t.Errorf("adoption + single-client override: %v", err)
	}
	// A SessionProvider with a uniform chain is accepted.
	if _, err := New(Options{SessionProvider: stubSessionProvider{}, Client: "ios"}); err != nil {
		t.Errorf("adoption (provider) + Client=ios: %v", err)
	}
}

// TestNew_AdoptedCookiesRequireJar rejects adopted cookies when the supplied
// HTTPClient has no cookie jar (no silent drop); visitorData-only adoption is fine.
func TestNew_AdoptedCookiesRequireJar(t *testing.T) {
	cookies := []*http.Cookie{{Name: "PREF", Value: "x", Domain: ".youtube.com", Path: "/"}}

	// No jar + cookies -> error at New.
	if _, err := New(Options{
		Client:     "web",
		Session:    &potoken.Session{VisitorData: "Cgt%3D%3D", Cookies: cookies},
		HTTPClient: &http.Client{}, // no jar
	}); err == nil || !strings.Contains(err.Error(), "cookie jar") {
		t.Errorf("cookies + no-jar err = %v, want a cookie-jar error", err)
	}

	// A jar present -> accepted.
	jar, _ := cookiejar.New(nil)
	if _, err := New(Options{
		Client:     "web",
		Session:    &potoken.Session{VisitorData: "Cgt%3D%3D", Cookies: cookies},
		HTTPClient: &http.Client{Jar: jar},
	}); err != nil {
		t.Errorf("cookies + jar: %v", err)
	}

	// visitorData-only adoption works without any jar.
	if _, err := New(Options{
		Client:     "web",
		Session:    &potoken.Session{VisitorData: "Cgt%3D%3D"},
		HTTPClient: &http.Client{}, // no jar, but no cookies either
	}); err != nil {
		t.Errorf("visitorData-only adoption, no jar: %v", err)
	}
}
