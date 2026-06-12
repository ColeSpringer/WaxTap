package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap"
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
		{&usageError{"bad"}, 2},
		{waxtap.ErrInvalidVideoID, 2},
		{waxtap.ErrVideoIDTooShort, 2},
		{waxtap.ErrInvalidPlaylistID, 2},
		{waxtap.ErrIncompatibleSpec, 2},
		{waxtap.ErrInvalidConfig, 2},
		{errFake("other"), 1},
	}
	for _, tt := range cases {
		if got := exitCodeFor(tt.err); got != tt.want {
			t.Errorf("exitCodeFor(%v) = %d, want %d", tt.err, got, tt.want)
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

type errFake string

func (e errFake) Error() string { return string(e) }
