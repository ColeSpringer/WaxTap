package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/internal/tempfile"
)

func TestJSONFloatNonFinite(t *testing.T) {
	cases := map[float64]string{
		-14.0:        "-14",
		0:            "0",
		math.Inf(1):  "null",
		math.Inf(-1): "null",
		math.NaN():   "null",
	}
	for in, want := range cases {
		b, err := json.Marshal(jsonFloat(in))
		if err != nil {
			t.Fatalf("marshal %v: %v", in, err)
		}
		if string(b) != want {
			t.Errorf("jsonFloat(%v) = %s, want %s", in, b, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:        "0 B",
		512:      "512 B",
		1024:     "1.0 KiB",
		1536:     "1.5 KiB",
		1048576:  "1.0 MiB",
		23592960: "22.5 MiB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                "0:00",
		19 * time.Second: "0:19",
		90 * time.Second: "1:30",
		time.Hour + 2*time.Minute + 3*time.Second: "1:02:03",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanLUFS(t *testing.T) {
	if got := humanLUFS(-14.2); got != "-14.2" {
		t.Errorf("humanLUFS finite = %q", got)
	}
	if got := humanLUFS(math.Inf(-1)); got != "n/a" {
		t.Errorf("humanLUFS(-inf) = %q, want n/a", got)
	}
}

func TestCleanMessage(t *testing.T) {
	if got := cleanMessage("waxtap: boom"); got != "boom" {
		t.Errorf("cleanMessage stripped wrong: %q", got)
	}
	if got := cleanMessage("plain"); got != "plain" {
		t.Errorf("cleanMessage altered plain: %q", got)
	}
}

func TestNoteForcedIOSIncomplete(t *testing.T) {
	cases := []struct {
		name   string
		client string
		err    error
		want   bool
	}{
		{"forced ios incomplete", "ios", waxtap.ErrIncompleteStream, true},
		{"forced ios case-insensitive", "iOS", waxtap.ErrIncompleteStream, true},
		{"forced ios wrapped incomplete", "ios", fmt.Errorf("deliver: %w", waxtap.ErrIncompleteStream), true},
		{"forced ios other error", "ios", waxtap.ErrVideoUnavailable, false},
		{"default chain incomplete", "", waxtap.ErrIncompleteStream, false},
		{"forced web incomplete", "web", waxtap.ErrIncompleteStream, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errBuf strings.Builder
			env := &appEnv{out: &strings.Builder{}, errOut: &errBuf, cfg: &appConfig{client: tc.client}}
			noteForcedIOSIncomplete(env, tc.err)
			got := strings.Contains(errBuf.String(), "iOS media delivery is unreliable")
			if got != tc.want {
				t.Errorf("note emitted=%v (%q), want %v", got, errBuf.String(), tc.want)
			}
		})
	}
}

func TestErrorHint_WebEmbeddedFallback(t *testing.T) {
	err := &waxtap.PlayabilityError{
		Status:   "ERROR",
		Reason:   "Video unavailable",
		Sentinel: waxtap.ErrVideoUnavailable,
		Embed:    true,
	}
	hint := errorHint(err)
	if !strings.Contains(hint, "--client web") {
		t.Errorf("errorHint = %q, want it to suggest --client web for a web_embedded fallback", hint)
	}
	// A regular unavailable video does not get web_embedded guidance.
	plain := &waxtap.PlayabilityError{Status: "ERROR", Reason: "Video unavailable", Sentinel: waxtap.ErrVideoUnavailable}
	if h := errorHint(plain); strings.Contains(h, "web_embedded") {
		t.Errorf("errorHint(non-embed) = %q, should not mention web_embedded", h)
	}
}

func TestChannelURLError(t *testing.T) {
	c := classifyError(waxtap.ErrIsChannel)
	if c.exitCode != 2 || c.code != "is-channel" {
		t.Errorf("classifyError(ErrIsChannel) = {exit %d, code %q}, want {2, is-channel}", c.exitCode, c.code)
	}
	msg := friendlyError(waxtap.ErrIsChannel)
	if strings.HasPrefix(msg, "waxtap:") {
		t.Errorf("friendlyError leaked the wrapped sentinel prefix: %q", msg)
	}
	if !strings.Contains(msg, "channel") || !strings.Contains(msg, "single video") {
		t.Errorf("friendlyError = %q, want it to name a channel URL and a single video", msg)
	}
}

func TestShortsPlaylistError(t *testing.T) {
	// Shorts shelf playlists are classified as unsupported input, not parser
	// failures.
	c := classifyError(waxtap.ErrShortsPlaylist)
	if c.exitCode != 2 || c.code != "unsupported-input" {
		t.Errorf("classifyError(ErrShortsPlaylist) = {exit %d, code %q}, want {2, unsupported-input}", c.exitCode, c.code)
	}
	msg := friendlyError(waxtap.ErrShortsPlaylist)
	if strings.HasPrefix(msg, "waxtap:") {
		t.Errorf("friendlyError leaked the wrapped sentinel prefix: %q", msg)
	}
	if !strings.Contains(msg, "Shorts") || !strings.Contains(msg, "uploads playlist") || !strings.Contains(msg, "with UU") {
		t.Errorf("friendlyError = %q, want it to name Shorts and explain how to select the uploads playlist", msg)
	}
	if strings.Contains(strings.ToLower(msg), "parser") || strings.Contains(strings.ToLower(msg), "stale") {
		t.Errorf("friendlyError = %q, must not describe Shorts playlists as a stale parser", msg)
	}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{context.Canceled, 130},
		{waxtap.ErrVideoUnavailable, 3},
		{waxtap.ErrLiveNotStarted, 3}, // availability verdicts share exit 3
		{waxtap.ErrAgeRestricted, 3},
		{waxtap.ErrMembersOnly, 3},
		{waxtap.ErrGeoBlocked, 3},
		{waxtap.ErrExtractionFailed, 4},
		{waxtap.ErrPlaylistParse, 4}, // maintainer-must-act, same class as extraction
		{waxtap.ErrRateLimited, 5},
		{waxtap.ErrFFmpegNotFound, 6},
		{waxtap.ErrIncompleteStream, 7}, // distinct from extraction and cipher failures
		{waxtap.ErrNeedsPOToken, 8},     // distinct precondition failure
		{&usageError{"bad"}, 2},
		{waxtap.ErrInvalidVideoID, 2},
		{waxtap.ErrVideoIDTooShort, 2},
		{waxtap.ErrInvalidPlaylistID, 2},
		{waxtap.ErrIncompatibleSpec, 2},
		{waxtap.ErrUnsupportedInput, 2}, // a correctable bad local input
		{waxtap.ErrShortsPlaylist, 2},   // known unsupported playlist type
		{waxtap.ErrIsPlaylist, 2},       // user can select the playlist command
		{waxtap.ErrIsChannel, 2},        // channel URL, not a single video
		{waxtap.ErrInvalidConfig, 2},
		{waxtap.ErrURLExpired, 7},                 // parity with incomplete-stream
		{waxtap.ErrRequestedFormatUnavailable, 2}, // correctable request error
		{&waxtap.ProviderError{Endpoint: "player-context", Cause: errFake("down")}, 9},
		{&url.Error{Op: "Get", URL: "x", Err: &net.OpError{Op: "proxyconnect", Err: errFake("refused")}}, 9},
		{&net.OpError{Op: "dial", Err: errFake("connection refused")}, 9},
		{&fs.PathError{Op: "mkdir", Path: "/root/x", Err: errFake("permission denied")}, 10},
		{errFake("other"), 1},
	}
	for _, tt := range cases {
		if got := exitCodeFor(tt.err); got != tt.want {
			t.Errorf("exitCodeFor(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestClassifyError_DeadPOTokenSidecar(t *testing.T) {
	se := &waxtap.SidecarError{Label: "bgutil PO-token server", Endpoint: "http://127.0.0.1:4417/get_pot", Err: &net.OpError{Op: "dial", Err: errFake("refused")}}
	wrapped := fmt.Errorf("%w: PO token provider failed: %w", waxtap.ErrNeedsPOToken, se)
	if got := exitCodeFor(wrapped); got != 9 {
		t.Errorf("unreachable PO-token sidecar exit = %d, want 9 (a dead sidecar is a network failure)", got)
	}
	if got := errorCode(wrapped); got != "network" {
		t.Errorf("code = %q, want network", got)
	}
	// A missing provider remains a token precondition failure.
	unconfigured := fmt.Errorf("%w: client %q requires a player PO token but no provider is configured", waxtap.ErrNeedsPOToken, "WEB")
	if got := exitCodeFor(unconfigured); got != 8 {
		t.Errorf("unconfigured PO-token exit = %d, want 8", got)
	}
	if got := errorCode(unconfigured); got != "needs-po-token" {
		t.Errorf("unconfigured code = %q, want needs-po-token", got)
	}
}

func TestClassifyError_SidecarAuth(t *testing.T) {
	for _, status := range []int{401, 403} {
		sre := &waxtap.SidecarResponseError{Label: "bgutil PO-token server", Endpoint: "http://127.0.0.1:4417/get_pot", StatusCode: status}
		c := classifyError(sre)
		if c.exitCode != 2 || c.code != "invalid-config" {
			t.Errorf("status %d = %+v, want invalid-config/2", status, c)
		}
		if !strings.Contains(c.hint, "--api-key") {
			t.Errorf("status %d hint = %q, want it to mention --api-key", status, c.hint)
		}
	}
	// Wrapped sidecar responses retain the authentication hint.
	wrapped := fmt.Errorf("%w: %w", waxtap.ErrNeedsPOToken, &waxtap.SidecarResponseError{Label: "bgutil", Endpoint: "http://h/get_pot", StatusCode: 401})
	if c := classifyError(wrapped); !strings.Contains(c.hint, "--api-key") {
		t.Errorf("wrapped 401 hint = %q, want it to mention --api-key", c.hint)
	}
}

// TestClassifyError_NewCodesAndHints covers network, I/O, format, and cipher
// classifications.
func TestClassifyError_NewCodesAndHints(t *testing.T) {
	if c := classifyError(&waxtap.ProviderError{Endpoint: "session", Cause: errFake("x")}); c.code != "network" || c.exitCode != 9 {
		t.Errorf("provider error = %+v, want network/9", c)
	}
	if c := classifyError(&fs.PathError{Op: "open", Path: "/x", Err: errFake("x")}); c.code != "io" || c.exitCode != 10 {
		t.Errorf("path error = %+v, want io/10", c)
	}
	// Input errors do not receive output-directory guidance.
	if c := classifyError(&fs.PathError{Op: "read", Path: "/in.flac", Err: errFake("x")}); c.hint != "" {
		t.Errorf("input read error hint = %q, want none (it is not an output failure)", c.hint)
	}
	// Output errors receive output-directory guidance.
	oe := tempfile.WrapOutput("create", &fs.PathError{Op: "open", Path: "/ro/out.flac", Err: errFake("permission denied")})
	if c := classifyError(oe); c.code != "io" || c.exitCode != 10 || !strings.Contains(c.hint, "output directory") {
		t.Errorf("output error = %+v, want io/10 with the output-directory hint", c)
	}
	// Rename failures remain classified as output I/O errors.
	re := tempfile.WrapOutput("rename", &os.LinkError{Op: "rename", Old: "/t/a", New: "/ro/b", Err: errFake("permission denied")})
	if c := classifyError(re); c.code != "io" || c.exitCode != 10 || !strings.Contains(c.hint, "output directory") {
		t.Errorf("rename output error = %+v, want io/10 with the output-directory hint", c)
	}
	cipher := fmt.Errorf("descramble: %w", waxtap.ErrCipherSolve)
	if c := classifyError(cipher); c.code != "cipher-solve" || c.exitCode != 4 || !strings.Contains(c.hint, "attested identity") {
		t.Errorf("cipher solve = %+v, want cipher-solve/4 with attested-identity hint", c)
	}
	rfe := &waxtap.RequestedFormatError{Selector: "itag(999)", Itags: []int{140, 251}}
	c := classifyError(rfe)
	if c.code != "format-unavailable" || c.exitCode != 2 || !strings.Contains(c.hint, "formats") {
		t.Errorf("requested-format = %+v, want format-unavailable/2 with a formats hint", c)
	}
	if !strings.Contains(c.message, "140") {
		t.Errorf("message = %q, want it to name the available itags", c.message)
	}
}

// TestClassifyError_AvailabilityCodes checks the four new availability verdicts
// map to distinct exit-3 code strings.
func TestClassifyError_AvailabilityCodes(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{waxtap.ErrLiveNotStarted, "live-not-started"},
		{waxtap.ErrAgeRestricted, "age-restricted"},
		{waxtap.ErrMembersOnly, "members-only"},
		{waxtap.ErrGeoBlocked, "geo-blocked"},
	}
	for _, tc := range cases {
		if c := classifyError(tc.err); c.exitCode != 3 || c.code != tc.code {
			t.Errorf("classifyError(%v) = {exit %d, code %q}, want {3, %q}", tc.err, c.exitCode, c.code, tc.code)
		}
	}
}

// TestClassifyError_SentinelBeatsStructural verifies that domain sentinels
// outrank wrapped transport and filesystem errors.
func TestClassifyError_SentinelBeatsStructural(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: errFake("connection reset by peer")}

	// A stream truncated by a mid-download TCP reset stays incomplete (exit 7),
	// not network (exit 9): scripts key on 7 for cross-client retry.
	incomplete := fmt.Errorf("%w: stream stalled at offset 524288: %w", waxtap.ErrIncompleteStream, reset)
	if c := classifyError(incomplete); c.exitCode != 7 || c.code != "incomplete-stream" {
		t.Errorf("incomplete-over-OpError = %+v, want incomplete-stream/7", c)
	}

	// A ProviderError whose cause is a meaningful sentinel ranks on that sentinel.
	rl := &waxtap.ProviderError{Endpoint: "session", Cause: waxtap.ErrRateLimited}
	if c := classifyError(rl); c.exitCode != 5 || c.code != "rate-limited" {
		t.Errorf("provider-over-rate-limited = %+v, want rate-limited/5", c)
	}
	unavail := &waxtap.ProviderError{Endpoint: "player-context", Cause: waxtap.ErrVideoUnavailable}
	if c := classifyError(unavail); c.exitCode != 3 || c.code != "video-unavailable" {
		t.Errorf("provider-over-unavailable = %+v, want video-unavailable/3", c)
	}

	// A ProviderError with no meaningful cause is still the network class.
	netProvider := &waxtap.ProviderError{Endpoint: "player-context", Cause: reset}
	if c := classifyError(netProvider); c.exitCode != 9 || c.code != "network" {
		t.Errorf("provider-over-OpError = %+v, want network/9", c)
	}
}

// TestClassifyError_TimeoutConsistentAcrossPhases verifies that dial and read
// deadlines share the timeout classification.
func TestClassifyError_TimeoutConsistentAcrossPhases(t *testing.T) {
	dial := &url.Error{Op: "Get", URL: "x", Err: &net.OpError{Op: "dial", Err: context.DeadlineExceeded}}
	read := &url.Error{Op: "Get", URL: "x", Err: context.DeadlineExceeded}
	for name, err := range map[string]error{"dial-phase": dial, "read-phase": read} {
		c := classifyError(err)
		if c.code != "timeout" || c.exitCode != 9 {
			t.Errorf("%s timeout = %+v, want code timeout and exit 9", name, c)
		}
	}
	// A connection failure with no deadline still classifies as the network class.
	refused := &net.OpError{Op: "dial", Err: errFake("connection refused")}
	if c := classifyError(refused); c.code != "network" || c.exitCode != 9 {
		t.Errorf("refused dial = %+v, want network/9", c)
	}
}

func TestClassifyError_SidecarResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		exit   int
		code   string
	}{
		{"bad request", 400, 2, "invalid-config"},
		{"unauthorized", 401, 2, "invalid-config"},
		{"too many requests", 429, 5, "rate-limited"},
		{"request timeout", 408, 9, "network"},
		{"server error", 500, 9, "network"},
		{"invalid 200 response", 0, 9, "network"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sre := &waxtap.SidecarResponseError{Label: "session endpoint", Endpoint: "http://127.0.0.1:4416/session", StatusCode: tc.status, Reason: "x"}
			prov := &waxtap.ProviderError{Endpoint: "session", Cause: sre}
			if c := classifyError(prov); c.exitCode != tc.exit || c.code != tc.code {
				t.Errorf("provider %s = %+v, want exit %d and code %s", tc.name, c, tc.exit, tc.code)
			}
			po := fmt.Errorf("%w: PO token provider failed: %w", waxtap.ErrNeedsPOToken, sre)
			if c := classifyError(po); c.exitCode != tc.exit || c.code != tc.code {
				t.Errorf("po-token %s = %+v, want exit %d and code %s", tc.name, c, tc.exit, tc.code)
			}
		})
	}
}

func TestFriendlyError_Sidecar429NamesSidecar(t *testing.T) {
	sre := &waxtap.SidecarResponseError{Label: "bgutil PO-token server", Endpoint: "http://user:pass@127.0.0.1:4417/get_pot?key=secret", StatusCode: 429}
	msg := friendlyError(sre)
	// The message names the specific provider (distinct from YouTube) and status;
	// the "check the sidecar's rate limits" advisory now rides on the hint channel.
	if !strings.Contains(msg, "bgutil PO-token server") || !strings.Contains(msg, "429") {
		t.Errorf("msg = %q, want it to attribute throttling to the named sidecar", msg)
	}
	if strings.Contains(msg, "secret") || strings.Contains(msg, "user:pass") {
		t.Errorf("msg = %q, want the endpoint redacted without query or user info", msg)
	}
	if h := classifyError(sre).hint; !strings.Contains(h, "rate limit") {
		t.Errorf("hint = %q, want a sidecar rate-limit advisory", h)
	}
	if yt := friendlyError(waxtap.ErrRateLimited); !strings.Contains(yt, "YouTube") {
		t.Errorf("YouTube rate-limit msg = %q, want it to name YouTube", yt)
	}
}

func TestClassifyError_PlaylistAvailability(t *testing.T) {
	if c := classifyError(&waxtap.PlaylistUnavailableError{Reason: "This playlist does not exist."}); c.exitCode != 3 || c.code != "playlist-unavailable" {
		t.Errorf("unavailable = %+v, want code playlist-unavailable and exit 3", c)
	}
	if c := classifyError(waxtap.ErrPlaylistEmpty); c.exitCode != 3 || c.code != "playlist-empty" {
		t.Errorf("empty = %+v, want code playlist-empty and exit 3", c)
	}
	if c := classifyError(waxtap.ErrInvalidPlaylistID); c.exitCode != 2 {
		t.Errorf("malformed ID exit = %d, want 2", c.exitCode)
	}
	// Preserve YouTube's reason in the user-facing message.
	if c := classifyError(&waxtap.PlaylistUnavailableError{Reason: "This playlist does not exist."}); !strings.Contains(c.message, "does not exist") {
		t.Errorf("message = %q, want YouTube's reason preserved", c.message)
	}
	// A browse 404 wraps the sentinel around an HTTPStatusError that embeds the
	// internal endpoint URL. Classification should keep the exit code while hiding
	// that URL from user-facing output.
	wrapped := fmt.Errorf("%w: %v", waxtap.ErrPlaylistUnavailable,
		&waxtap.HTTPStatusError{StatusCode: 404, URL: "https://www.youtube.com/youtubei/v1/browse"})
	if c := classifyError(wrapped); c.exitCode != 3 || c.code != "playlist-unavailable" {
		t.Errorf("wrapped 404 = %+v, want playlist-unavailable exit 3", c)
	}
	if c := classifyError(wrapped); strings.Contains(c.message, "youtubei") {
		t.Errorf("message = %q, leaked the internal endpoint URL", c.message)
	}
}

func TestFriendlyError_HTTPStatusNoURL(t *testing.T) {
	// An otherwise-unclassified status error, such as an innertube 503, should
	// render without leaking its endpoint URL and still name the right service.
	cases := []struct {
		name       string
		url        string
		wantSource string
	}{
		{"youtube innertube", "https://www.youtube.com/youtubei/v1/browse", "YouTube"},
		{"sponsorblock", "https://sponsor.ajay.app/api/skipSegments", "SponsorBlock"},
		{"googlevideo", "https://rr3---sn-abc.googlevideo.com/videoplayback?x=1", "googlevideo"},
		{"unknown host", "https://example.test/x", "the server"},
		{"no url", "", "the server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := friendlyError(&waxtap.HTTPStatusError{StatusCode: 503, URL: tc.url})
			if strings.Contains(msg, "youtubei") || strings.Contains(msg, "skipSegments") || strings.Contains(msg, "videoplayback") {
				t.Errorf("msg = %q, leaked the endpoint path", msg)
			}
			if !strings.Contains(msg, "503") {
				t.Errorf("msg = %q, want it to report the HTTP status", msg)
			}
			if !strings.Contains(msg, tc.wantSource) {
				t.Errorf("msg = %q, want it attributed to %q", msg, tc.wantSource)
			}
		})
	}
}

func TestFriendlyError_SponsorBlockNotMisattributed(t *testing.T) {
	// A SponsorBlock fetch failure under --sponsorblock-on-error fail wraps a
	// SponsorBlock HTTPStatusError; it must not be reported as a YouTube outage.
	wrapped := fmt.Errorf("waxtap: SponsorBlock fetch failed: %w",
		&waxtap.HTTPStatusError{StatusCode: 503, URL: "https://sponsor.ajay.app/api/skipSegments"})
	msg := friendlyError(wrapped)
	if strings.Contains(msg, "YouTube") {
		t.Errorf("msg = %q, misattributed a SponsorBlock failure to YouTube", msg)
	}
	if !strings.Contains(msg, "SponsorBlock") {
		t.Errorf("msg = %q, want it attributed to SponsorBlock", msg)
	}
}

// TestNormalizeExecuteError_FlagOrder verifies that a Cobra unknown-command for a
// YouTube-looking token becomes a usage error (exit 2) with the flag-order hint.
func TestNormalizeExecuteError_FlagOrder(t *testing.T) {
	err := normalizeExecuteError(errFake(`unknown command "dQw4w9WgXcQ" for "waxtap"`))
	if got := exitCodeFor(err); got != 2 {
		t.Errorf("unknown-command exit = %d, want 2", got)
	}
	if hint := errorHint(err); !strings.Contains(hint, "waxtap download dQw4w9WgXcQ") {
		t.Errorf("hint = %q, want a download suggestion", hint)
	}
	// A non-target unknown command becomes a plain usage error with no hint.
	plain := normalizeExecuteError(errFake(`unknown command "boguscmd" for "waxtap"`))
	if got := exitCodeFor(plain); got != 2 {
		t.Errorf("bogus-command exit = %d, want 2", got)
	}
	if hint := errorHint(plain); hint != "" {
		t.Errorf("hint = %q, want none for a non-target token", hint)
	}
}

// TestFlagOrderHint checks hints for unknown commands and leading-dash IDs,
// including the guard that an 11-character long flag gets no hint.
func TestFlagOrderHint(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string // substring the hint must contain; "" means no hint
	}{
		{"unknown command target", `unknown command "dQw4w9WgXcQ" for "waxtap"`, "waxtap download dQw4w9WgXcQ"},
		{"unknown command non-target", `unknown command "boguscmd" for "waxtap"`, ""},
		{"dash shorthand 11-char id", "unknown shorthand flag: 'b' in -bcdefghijk", "-- -bcdefghijk"},
		{"dash shorthand short token", "unknown shorthand flag: 'a' in -abc", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := flagOrderHint(&usageError{msg: tc.msg})
			if tc.want == "" {
				if got != "" {
					t.Errorf("flagOrderHint(%q) = %q, want no hint", tc.msg, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("flagOrderHint(%q) = %q, want it to contain %q", tc.msg, got, tc.want)
			}
		})
	}

	// Exercise pflag itself: a leading-dash video ID gets the dash hint, while an
	// unknown long flag gets no hint. Long-flag hints stay disabled because real
	// command flags can also look like video IDs.
	t.Run("real shorthand dash id", func(t *testing.T) {
		err := newInfoCmd().ParseFlags([]string{"-bcdefghijk"})
		if err == nil {
			t.Fatal("expected pflag to reject -bcdefghijk as shorthand flags")
		}
		if got := flagOrderHint(&usageError{msg: err.Error()}); !strings.Contains(got, "-- -bcdefghijk") {
			t.Errorf("hint = %q, want the leading-dash guidance", got)
		}
	})
	t.Run("real long flag no hint", func(t *testing.T) {
		err := newInfoCmd().ParseFlags([]string{"--collision"})
		if err == nil {
			t.Fatal("expected info to reject the unknown --collision flag")
		}
		if got := flagOrderHint(&usageError{msg: err.Error()}); got != "" {
			t.Errorf("hint = %q, want none for an 11-char long flag", got)
		}
	})
}

func TestFlagBeforeSubcommand(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"flag before subcommand", []string{"--no-cache", "--cache-dir", "/p", "info", "id"}, true},
		{"single flag before subcommand", []string{"--no-cache", "info", "id"}, true},
		{"subcommand first", []string{"info", "--no-cache", "id"}, false},
		{"no subcommand", []string{"--json", "boguscmd"}, false},
		{"bare dash is not a flag", []string{"-", "info"}, false},
		{"terminator is not a flag", []string{"--", "info"}, false},
		{"alias recognized", []string{"--no-cache", "sb", "id"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flagBeforeSubcommand(tc.args, rootSubcommandNames); got != tc.want {
				t.Errorf("flagBeforeSubcommand(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestFlagOrderHint_GenericOrder(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()

	// A flag before the subcommand can make cobra report the next token as an
	// unknown command. The hint explains ordering even when the token is not
	// target-shaped, and it does not name a specific subcommand.
	os.Args = []string{"waxtap", "--no-cache", "--cache-dir", "/p", "info", "dQw4w9WgXcQ"}
	got := flagOrderHint(&usageError{msg: `unknown command "/p" for "waxtap"`})
	if !strings.Contains(got, "before the subcommand") || !strings.Contains(got, "after it") {
		t.Errorf("flagOrderHint = %q, want the generic flag-ordering guidance", got)
	}

	// A flag value that coincides with a subcommand name must not be reported as the
	// swallowed subcommand; the generic hint avoids naming it.
	os.Args = []string{"waxtap", "--cache-dir", "version", "abc123"}
	got = flagOrderHint(&usageError{msg: `unknown command "abc123" for "waxtap"`})
	if strings.Contains(got, "version") {
		t.Errorf("flagOrderHint = %q, must not name a flag value as the subcommand", got)
	}

	// A genuine subcommand typo (no preceding flag) gets no generic hint.
	os.Args = []string{"waxtap", "boguscmd"}
	if got := flagOrderHint(&usageError{msg: `unknown command "boguscmd" for "waxtap"`}); got != "" {
		t.Errorf("flagOrderHint = %q, want no hint for a bare subcommand typo", got)
	}
}

// TestRootSubcommandNamesMatchTree keeps the static order-hint set aligned with
// the live command tree.
func TestRootSubcommandNamesMatchTree(t *testing.T) {
	root := newRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	want := map[string]bool{}
	for _, c := range root.Commands() {
		want[c.Name()] = true
		for _, a := range c.Aliases {
			want[a] = true
		}
	}
	for name := range want {
		if !rootSubcommandNames[name] {
			t.Errorf("command %q is in the tree but missing from rootSubcommandNames", name)
		}
	}
	for name := range rootSubcommandNames {
		if !want[name] {
			t.Errorf("rootSubcommandNames has %q, which is not a command name or alias in the tree", name)
		}
	}
}

func TestErrorCode(t *testing.T) {
	if got := errorCode(waxtap.ErrFFmpegNotFound); got != "ffmpeg-not-found" {
		t.Errorf("errorCode(ffmpeg) = %q", got)
	}
	if got := errorCode(&usageError{"x"}); got != "usage" {
		t.Errorf("errorCode(usage) = %q", got)
	}
	if got := errorCode(waxtap.ErrPlaylistParse); got != "stale-parser" {
		t.Errorf("errorCode(playlist parse) = %q, want stale-parser", got)
	}
	if got := errorCode(waxtap.ErrIncompleteStream); got != "incomplete-stream" {
		t.Errorf("errorCode(incomplete) = %q, want incomplete-stream", got)
	}
	if got := errorCode(waxtap.ErrInvalidPlaylistID); got != "invalid-input" {
		t.Errorf("errorCode(invalid playlist) = %q, want invalid-input", got)
	}
	if got := errorCode(waxtap.ErrVideoIDTooShort); got != "invalid-input" {
		t.Errorf("errorCode(too-short id) = %q, want invalid-input", got)
	}
	if got := errorCode(waxtap.ErrInvalidConfig); got != "invalid-config" {
		t.Errorf("errorCode(invalid config) = %q, want invalid-config", got)
	}
}

func TestIsProxyError(t *testing.T) {
	// net/http wraps a proxyconnect OpError in a url.Error.
	typed := &url.Error{Op: "Get", URL: "https://www.youtube.com", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("connection refused")}}
	if !isProxyError(typed) {
		t.Error("typed proxyconnect OpError should be detected")
	}
	// String fallback for transports that do not expose a typed proxyconnect.
	if !isProxyError(errors.New("proxyconnect tcp: dial tcp 127.0.0.1:8080: connection refused")) {
		t.Error("string-match proxyconnect should be detected")
	}
	if isProxyError(errors.New("some unrelated network error")) {
		t.Error("a non-proxy error must not be classified as a proxy failure")
	}
}

func TestFriendlyError_ProxyAndInvalidPlaylist(t *testing.T) {
	proxy := &url.Error{Op: "Get", URL: "x", Err: &net.OpError{Op: "proxyconnect", Err: errors.New("refused")}}
	if msg := friendlyError(proxy); !strings.Contains(msg, "proxy connection failed") {
		t.Errorf("proxy friendlyError = %q, want a proxy failure message", msg)
	}
	if msg := friendlyError(waxtap.ErrInvalidPlaylistID); !strings.Contains(msg, "playlist ID") {
		t.Errorf("invalid-playlist friendlyError = %q", msg)
	}
}

func TestFriendlyError_SidecarUnreachableBeatsPOToken(t *testing.T) {
	se := &waxtap.SidecarError{Label: "bgutil PO-token server", Endpoint: "http://127.0.0.1:4417/get_pot", Err: errors.New("connection refused")}
	// The YouTube layer wraps provider failures with ErrNeedsPOToken.
	wrapped := fmt.Errorf("%w: PO token provider failed: %w", waxtap.ErrNeedsPOToken, se)
	msg := friendlyError(wrapped)
	if !strings.Contains(msg, "unreachable") || !strings.Contains(msg, "127.0.0.1:4417") {
		t.Errorf("friendlyError = %q, want it to name the unreachable provider", msg)
	}
	if strings.Contains(msg, "verified PO token") {
		t.Errorf("friendlyError = %q, want the unreachable message to win over the generic PO-token text", msg)
	}
}

// TestAlreadyRenderedMarker verifies that the marker suppresses rendering while
// preserving the wrapped failure's exit code.
func TestAlreadyRenderedMarker(t *testing.T) {
	if alreadyRendered(nil) != nil {
		t.Error("alreadyRendered(nil) must stay nil (success)")
	}
	wrapped := alreadyRendered(waxtap.ErrNeedsPOToken)
	if _, ok := errors.AsType[*alreadyRenderedError](wrapped); !ok {
		t.Error("alreadyRendered(err) must be recognizable as the marker")
	}
	if got := exitCodeFor(wrapped); got != 8 {
		t.Errorf("exitCodeFor(alreadyRendered(needs-po-token)) = %d, want 8 (unwrapped cause drives the exit)", got)
	}
	// normalizeExecuteError must not strip the marker even when the cause's message
	// begins with "unknown ..." (which would otherwise rewrap it as a usage error).
	marker := alreadyRendered(errFake(`unknown subcommand "x" for "waxtap doctor"`))
	if _, ok := errors.AsType[*alreadyRenderedError](normalizeExecuteError(marker)); !ok {
		t.Error("normalizeExecuteError stripped the already-rendered marker")
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
