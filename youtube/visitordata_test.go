package youtube

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func TestGenerateVisitorData(t *testing.T) {
	vd := generateVisitorData("US")
	if vd == "" {
		t.Fatal("empty visitorData")
	}
	unesc, err := url.QueryUnescape(vd)
	if err != nil {
		t.Fatalf("visitorData is not URL-escaped: %v", err)
	}
	raw, err := base64.URLEncoding.DecodeString(unesc)
	if err != nil {
		t.Fatalf("visitorData is not base64url: %v", err)
	}
	if len(raw) < 16 {
		t.Errorf("visitorData payload too small: %d bytes", len(raw))
	}
	// First field is the page-load nonce (proto field 1, length-delimited), so the
	// leading wire tag must be 0x0a = (1<<3)|2.
	if raw[0] != 0x0a {
		t.Errorf("first wire tag = %#x, want 0x0a", raw[0])
	}
}

func TestGenerateVisitorData_Randomized(t *testing.T) {
	// Two calls should differ (random nonce + timestamp). Separate vars avoid a
	// false self-comparison warning.
	first := generateVisitorData("US")
	second := generateVisitorData("US")
	if first == second {
		t.Error("expected randomized visitorData across calls")
	}
}

func TestNewSession_SeedsVisitorData(t *testing.T) {
	if newSession("US").visitorData == "" {
		t.Error("newSession should pre-seed a synthetic visitorData")
	}
}

func TestGenerateVisitorData_DefaultRegion(t *testing.T) {
	if generateVisitorData("") == "" {
		t.Error("empty region should default, not produce empty output")
	}
}

// TestVisitorDataEncodingCoherence locks the wire form the synthetic and
// bootstrapped paths share, so an adopted value re-sent verbatim stays
// byte-coherent with what the minter attests under (the /session contract). Both
// paths emit URL-escaped base64url (padding as %3D), never a bare "=".
func TestVisitorDataEncodingCoherence(t *testing.T) {
	// Synthetic: padding, if present, is escaped; there is never a bare '='.
	for range 50 {
		vd := generateVisitorData("US")
		if strings.ContainsRune(vd, '=') {
			t.Fatalf("synthetic visitorData has a bare '=' (not URL-escaped): %q", vd)
		}
		if _, err := url.QueryUnescape(vd); err != nil {
			t.Fatalf("synthetic visitorData is not URL-escaped: %v", err)
		}
	}

	// Bootstrapped: YouTube's page literal is already URL-escaped ("...%3D%3D"), and
	// jsonUnescape decodes only JSON backslash escapes, so %3D survives. The
	// bootstrapped path therefore sends the same form synthetic does, not a raw
	// "==" that would diverge from the value used as content_binding.
	const pageLiteral = "CgtuTFRvMEd4TG5PVQ%3D%3D"
	m := visitorDataRe.FindStringSubmatch(`{"VISITOR_DATA":"` + pageLiteral + `"}`)
	if m == nil {
		t.Fatal("regex did not match the page literal")
	}
	if got := jsonUnescape(m[1]); got != pageLiteral {
		t.Errorf("bootstrapped visitorData = %q, want the page literal verbatim %q", got, pageLiteral)
	}
}
