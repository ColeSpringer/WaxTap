package main

import (
	"context"
	"encoding/json"
	"math"
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
		{waxtap.ErrInvalidPlaylistID, 1},
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
}

type errFake string

func (e errFake) Error() string { return string(e) }
