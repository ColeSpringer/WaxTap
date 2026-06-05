package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
)

// Non-TTY progress is line-oriented. A download transition has no byte data, so
// it should not print a misleading "0 B" before real progress arrives.
func TestProgress_OffTTYAnnouncesStageWithoutByteCount(t *testing.T) {
	var buf bytes.Buffer
	r := &progressReporter{w: &buf, enabled: true, tty: false}

	r.handle(waxtap.Event{Stage: waxtap.StageDownloading}) // transition: 0 bytes
	r.handle(waxtap.Event{Stage: waxtap.StageDownloading, Bytes: 256 << 10, Total: 3 << 20})
	r.handle(waxtap.Event{Stage: waxtap.StageDownloading, Bytes: 3 << 20, Total: 3 << 20})

	out := buf.String()
	if strings.Contains(out, "0 B") {
		t.Fatalf("off-TTY output should not show a byte count, got %q", out)
	}
	if got := strings.Count(out, "downloading"); got != 1 {
		t.Fatalf("download stage should be announced once, got %d in %q", got, out)
	}
	if !strings.Contains(out, "downloading…") {
		t.Fatalf("expected \"downloading…\", got %q", out)
	}
}

// On a TTY, progress snapshots still draw the live byte bar.
func TestProgress_TTYRendersByteProgress(t *testing.T) {
	var buf bytes.Buffer
	r := &progressReporter{w: &buf, enabled: true, tty: true}

	r.handle(waxtap.Event{Stage: waxtap.StageDownloading, Bytes: 3 << 20, Total: 3 << 20})

	out := buf.String()
	if !strings.Contains(out, "100%") || !strings.Contains(out, "3.0 MiB") {
		t.Fatalf("expected a live byte bar on a TTY, got %q", out)
	}
}
