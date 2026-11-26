package waxerr

import (
	"errors"
	"fmt"
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
		// A temporary refusal is a verdict about the video, so it outranks a
		// breakage in our own extraction, but it is explicitly not final, so a
		// real availability verdict outranks it.
		{"temporary beats extraction", ErrExtractionFailed, ErrTemporarilyUnavailable, ErrTemporarilyUnavailable},
		{"unavailable beats temporary", ErrTemporarilyUnavailable, unavailable, unavailable},
		{"temporary beats generic", generic, ErrTemporarilyUnavailable, ErrTemporarilyUnavailable},
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

// The spellings are what --json reports as error.kind.
func TestPlaylistUnavailableKind_String(t *testing.T) {
	cases := []struct {
		kind PlaylistUnavailableKind
		want string
	}{
		{PlaylistUnavailableUnknown, "unknown"},
		{PlaylistUnavailableNotFound, "not-found"},
		{PlaylistUnavailablePrivate, "private"},
		{PlaylistUnavailableUnviewable, "unviewable"},
		{PlaylistUnavailableKind(200), "unknown"},
	}
	for _, tc := range cases {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("PlaylistUnavailableKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// YouTube words a private list as one that does not exist, so that kind says
// so itself; every other kind keeps YouTube's reason.
func TestPlaylistUnavailableError_ErrorPerKind(t *testing.T) {
	const gone = "The playlist does not exist."
	cases := []struct {
		err  *PlaylistUnavailableError
		want string
	}{
		{&PlaylistUnavailableError{}, "waxtap: playlist unavailable"},
		{&PlaylistUnavailableError{Reason: gone}, "waxtap: " + gone},
		{&PlaylistUnavailableError{Kind: PlaylistUnavailableNotFound, Reason: gone}, "waxtap: " + gone},
		// A browse 404 carries no reason, so the kind speaks for itself.
		{&PlaylistUnavailableError{Kind: PlaylistUnavailableNotFound}, "waxtap: YouTube reports no such playlist"},
		{&PlaylistUnavailableError{Kind: PlaylistUnavailableUnviewable}, "waxtap: YouTube serves no listing for this playlist ID"},
		{&PlaylistUnavailableError{Kind: PlaylistUnavailablePrivate, Reason: gone}, "waxtap: playlist is private"},
		{&PlaylistUnavailableError{Kind: PlaylistUnavailablePrivate}, "waxtap: playlist is private"},
		{&PlaylistUnavailableError{Kind: PlaylistUnavailableUnviewable, Reason: "This playlist type is unviewable."}, "waxtap: This playlist type is unviewable."},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%+v.Error() = %q, want %q", *tc.err, got, tc.want)
		}
		wrapped := fmt.Errorf("enumerate: %w", tc.err)
		if !errors.Is(wrapped, ErrPlaylistUnavailable) {
			t.Errorf("%+v does not unwrap to ErrPlaylistUnavailable", *tc.err)
		}
		if pu, ok := errors.AsType[*PlaylistUnavailableError](wrapped); !ok || pu.Kind != tc.err.Kind {
			t.Errorf("AsType through a wrap = %v, %v; want kind %v", pu, ok, tc.err.Kind)
		}
	}
}
