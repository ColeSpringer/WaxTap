package youtube

import "testing"

func TestSession_VisitorDataPinnedAfterPOToken(t *testing.T) {
	s := newSession("US")
	s.visitorData = "ORIGINAL"

	s.learnVisitorData("LEARNED1")
	if s.visitorData != "LEARNED1" {
		t.Fatalf("learnVisitorData before binding = %q, want LEARNED1", s.visitorData)
	}

	s.bindPOToken()
	s.learnVisitorData("LEARNED2")
	if s.visitorData != "LEARNED1" {
		t.Errorf("learnVisitorData after binding = %q, want it pinned to LEARNED1", s.visitorData)
	}

	s2 := newSession("US")
	s2.visitorData = "KEEP"
	s2.learnVisitorData("")
	if s2.visitorData != "KEEP" {
		t.Errorf("learnVisitorData(\"\") = %q, want KEEP", s2.visitorData)
	}
}

func TestSession_POBindingResetAllowsRelearn(t *testing.T) {
	s := newSession("US")
	s.visitorData = "SYNTHETIC"

	// Simulate a failed attempt that minted a token.
	s.bindPOToken()
	s.learnVisitorData("SERVER1")
	if s.visitorData != "SYNTHETIC" {
		t.Fatalf("visitorData = %q, want it pinned to SYNTHETIC during attempt 1", s.visitorData)
	}

	s.resetPOBinding()
	s.learnVisitorData("SERVER2")
	if s.visitorData != "SERVER2" {
		t.Errorf("visitorData = %q, want SERVER2 after resetPOBinding (no longer pinned)", s.visitorData)
	}
}
