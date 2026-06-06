package waxtap

import (
	"strings"
	"testing"
)

// TestNew_ChromeMajorValidation covers range validation and mutual exclusion with
// ProfileOverridePath.
func TestNew_ChromeMajorValidation(t *testing.T) {
	if _, err := New(Options{ChromeMajor: -1}); err == nil {
		t.Error("ChromeMajor -1 should be rejected")
	}
	if _, err := New(Options{ChromeMajor: 1000}); err == nil {
		t.Error("ChromeMajor 1000 should be rejected")
	}

	// The conflict is checked before the override file is read.
	_, err := New(Options{ChromeMajor: 151, ProfileOverridePath: writeOverride(t, validOverride)})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("ChromeMajor + ProfileOverridePath err = %v, want a mutual-exclusion error", err)
	}

	// Valid overrides, including zero, are accepted without making network requests.
	if _, err := New(Options{ChromeMajor: 151}); err != nil {
		t.Errorf("valid ChromeMajor 151: %v", err)
	}
	if _, err := New(Options{ChromeMajor: 0}); err != nil {
		t.Errorf("ChromeMajor 0 (default): %v", err)
	}
}
