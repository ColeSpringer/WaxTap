//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A FIFO is not a file WaxTap reads. It passes the CLI's local-input check (it
// is not a directory) and the engine refuses it by name: exit 2, code
// unsupported-input, with the kind in the message. A writer holds it open so
// the run cannot be one that merely failed to open an unattached pipe.
func TestTranscodeRefusesAFifoByName(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.wav")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// A reader has to be attached before a writer can open, and a writer has
	// to be attached for the run to be refused on what the path is rather
	// than on a pipe nobody is writing.
	rd, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("open the read end: %v", err)
	}
	defer rd.Close()
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("open the write end: %v", err)
	}
	defer w.Close()

	stdout, stderr, code := runMain(t, "transcode", fifo, "--format", "flac", "-o", filepath.Join(dir, "x.flac"), "--json")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s%s", code, stdout, stderr)
	}
	doc := oneJSONDoc(t, stdout)
	e, _ := doc["error"].(map[string]any)
	if e["code"] != "unsupported-input" {
		t.Errorf("error = %v, want code unsupported-input", e)
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, "named pipe") {
		t.Errorf("message = %q, want it to name the pipe", msg)
	}
}
