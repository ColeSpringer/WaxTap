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

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{context.Canceled, 130},
		{waxtap.ErrVideoUnavailable, 3},
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
		{waxtap.ErrIsPlaylist, 2},       // user can select the playlist command
		{waxtap.ErrInvalidConfig, 2},
		{waxtap.ErrURLExpired, 7},                 // parity with incomplete-stream
		{waxtap.ErrRequestedFormatUnavailable, 2}, // correctable request error
		{waxtap.ErrDeliveryUnsupported, 2},        // compatibility sentinel
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
	se := &sidecarError{label: "bgutil PO-token server", endpoint: "http://127.0.0.1:4417/get_pot", err: &net.OpError{Op: "dial", Err: errFake("refused")}}
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
		sre := &sidecarResponseError{label: "bgutil PO-token server", endpoint: "http://127.0.0.1:4417/get_pot", statusCode: status}
		c := classifyError(sre)
		if c.exitCode != 2 || c.code != "invalid-config" {
			t.Errorf("status %d = %+v, want invalid-config/2", status, c)
		}
		if !strings.Contains(c.hint, "--api-key") {
			t.Errorf("status %d hint = %q, want it to mention --api-key", status, c.hint)
		}
	}
	// Wrapped sidecar responses retain the authentication hint.
	wrapped := fmt.Errorf("%w: %w", waxtap.ErrNeedsPOToken, &sidecarResponseError{label: "bgutil", endpoint: "http://h/get_pot", statusCode: 401})
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
			sre := &sidecarResponseError{label: "session endpoint", endpoint: "http://127.0.0.1:4416/session", statusCode: tc.status, reason: "x"}
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
	sre := &sidecarResponseError{label: "bgutil PO-token server", endpoint: "http://user:pass@127.0.0.1:4417/get_pot?key=secret", statusCode: 429}
	msg := friendlyError(sre)
	if !strings.Contains(msg, "sidecar") {
		t.Errorf("msg = %q, want it to attribute throttling to the sidecar", msg)
	}
	if strings.Contains(msg, "secret") || strings.Contains(msg, "user:pass") {
		t.Errorf("msg = %q, want the endpoint redacted without query or user info", msg)
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
	se := &sidecarError{label: "bgutil PO-token server", endpoint: "http://127.0.0.1:4417/get_pot", err: errors.New("connection refused")}
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
