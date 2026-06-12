package waxerr

import (
	"errors"
	"testing"
)

func TestPreferErr(t *testing.T) {
	generic := errors.New("network down")
	unavailable := &PlayabilityError{Status: "ERROR", Sentinel: ErrVideoUnavailable}

	tests := []struct {
		name string
		a, b error
		want error
	}{
		{"nil a", nil, generic, generic},
		{"nil b", generic, nil, generic},
		{"unavailable beats needs-po-token", ErrNeedsPOToken, unavailable, unavailable},
		{"unavailable beats needs-po-token (swapped)", unavailable, ErrNeedsPOToken, unavailable},
		{"extraction beats incomplete", ErrIncompleteStream, ErrExtractionFailed, ErrExtractionFailed},
		{"incomplete beats generic", generic, ErrIncompleteStream, ErrIncompleteStream},
		{"generic beats needs-po-token", ErrNeedsPOToken, generic, generic},
		{"unavailable beats extraction", ErrExtractionFailed, unavailable, unavailable},
		{"tie keeps first", ErrNeedsPOToken, ErrNeedsPOToken, ErrNeedsPOToken},
		// Expired URLs have the same precedence as incomplete streams.
		{"url-expired beats needs-po-token", ErrNeedsPOToken, ErrURLExpired, ErrURLExpired},
		{"url-expired beats generic", generic, ErrURLExpired, ErrURLExpired},
		{"extraction beats url-expired", ErrURLExpired, ErrExtractionFailed, ErrExtractionFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreferErr(tc.a, tc.b); got != tc.want {
				t.Errorf("PreferErr(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestPreferErr_AvailabilityFamily(t *testing.T) {
	for _, sentinel := range []error{
		ErrVideoUnavailable, ErrVideoRestricted, ErrLoginRequired, ErrLiveContent, ErrNoAudioFormats,
	} {
		if got := PreferErr(ErrNeedsPOToken, sentinel); got != sentinel {
			t.Errorf("PreferErr(needs-po-token, %v) = %v, want %v", sentinel, got, sentinel)
		}
	}
}
