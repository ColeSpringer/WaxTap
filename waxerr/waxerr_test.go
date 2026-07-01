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

// TestPreferErr_ProviderRanksAsNetwork verifies provider-error precedence.
func TestPreferErr_ProviderRanksAsNetwork(t *testing.T) {
	pe := &ProviderError{Endpoint: "player-context", Cause: errors.New("dial tcp: refused")}
	if got := PreferErr(ErrNeedsPOToken, pe); got != pe {
		t.Errorf("PreferErr(needs-po-token, provider) = %v, want provider (network beats po-token)", got)
	}
	if got := PreferErr(pe, ErrExtractionFailed); got != ErrExtractionFailed {
		t.Errorf("PreferErr(provider, extraction) = %v, want extraction (diagnosis beats network)", got)
	}
	if got := PreferErr(pe, ErrIncompleteStream); got != ErrIncompleteStream {
		t.Errorf("PreferErr(provider, incomplete) = %v, want incomplete (delivery beats network)", got)
	}
}

// TestPreferErr_RequestedFormatNotMasked verifies that an explicit format miss
// outranks an availability error from another client.
func TestPreferErr_RequestedFormatNotMasked(t *testing.T) {
	rfe := &RequestedFormatError{Selector: "itag(18)", Itags: []int{140, 251}}
	unavailable := &PlayabilityError{Status: "ERROR", Sentinel: ErrVideoUnavailable}
	// The first error wins the tie.
	if got := PreferErr(rfe, unavailable); got != rfe {
		t.Errorf("PreferErr(requested-format, unavailable) = %v, want the requested-format error", got)
	}
	if got := PreferErr(ErrIncompleteStream, rfe); got != rfe {
		t.Errorf("PreferErr(incomplete, requested-format) = %v, want requested-format (top tier)", got)
	}
}

func TestPreferErr_AvailabilityFamily(t *testing.T) {
	for _, sentinel := range []error{
		ErrVideoUnavailable, ErrVideoRestricted, ErrLoginRequired, ErrLiveContent, ErrNoAudioFormats,
		ErrLiveNotStarted, ErrAgeRestricted, ErrMembersOnly, ErrGeoBlocked,
	} {
		if got := PreferErr(ErrNeedsPOToken, sentinel); got != sentinel {
			t.Errorf("PreferErr(needs-po-token, %v) = %v, want %v", sentinel, got, sentinel)
		}
		// The new verdicts must outrank a generic/network error, like their siblings.
		if got := PreferErr(errors.New("generic"), sentinel); got != sentinel {
			t.Errorf("PreferErr(generic, %v) = %v, want %v (rank 5)", sentinel, got, sentinel)
		}
	}
}
