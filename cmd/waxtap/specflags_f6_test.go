package main

import (
	"strings"
	"testing"
)

// TestParseRangesMultiSeparator covers a multi-dash range like 1-2-3: it reports a
// range-shape error instead of letting parseTimestamp reject the "2-3" fragment.
func TestParseRangesMultiSeparator(t *testing.T) {
	_, err := parseRanges([]string{"1-2-3"})
	if err == nil || !strings.Contains(err.Error(), "one start-end") {
		t.Fatalf("parseRanges(1-2-3) err = %v, want the range-shape message", err)
	}
	for _, ok := range []string{"1:00-2:30", "1-2"} {
		if _, err := parseRanges([]string{ok}); err != nil {
			t.Errorf("parseRanges(%q) err = %v, want nil", ok, err)
		}
	}
}
