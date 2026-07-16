package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
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
	if !strings.Contains(out, "downloading\n") {
		t.Fatalf("expected a downloading line, got %q", out)
	}
}

func TestProgress_WarningCarriesCode(t *testing.T) {
	var buf bytes.Buffer
	r := &progressReporter{w: &buf, enabled: true, tty: false}

	r.handle(waxtap.Event{Stage: waxtap.StageWarning, Warning: &waxtap.Warning{
		Code: waxtap.WarnFallbackProfile, Detail: "served WEB",
	}})

	out := buf.String()
	if !strings.Contains(out, "[fallback-profile]") {
		t.Errorf("live warning should include the code tag, got %q", out)
	}
	if !strings.Contains(out, "served WEB") {
		t.Errorf("live warning should include the detail, got %q", out)
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
