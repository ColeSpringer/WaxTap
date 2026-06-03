package youtube

import (
	"encoding/base64"
	"net/url"
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
	if generateVisitorData("US") == generateVisitorData("US") {
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
