package tempfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommit(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()

	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}

	// No stray .part files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (only the final file)", len(entries))
	}
}

func TestDiscardRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := f.Write([]byte("partial")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("final file should not exist after Discard, stat err = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("dir has %d entries, want 0 after Discard", len(entries))
	}
}

func TestDiscardAfterCommitIsNoop(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Discard after a successful Commit must not remove the committed file.
	if err := f.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("committed file should still exist, stat err = %v", err)
	}
}

func TestScratchCleanup(t *testing.T) {
	dir := t.TempDir()
	f, cleanup, err := Scratch(dir, "")
	if err != nil {
		t.Fatalf("Scratch: %v", err)
	}
	name := f.Name()
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("scratch file should exist: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("scratch file should be gone after cleanup, stat err = %v", err)
	}
	// Idempotent.
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
}
