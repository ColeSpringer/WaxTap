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

func TestExternalCommit(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.flac")

	e, err := NewExternal(final)
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()

	// The temp path must preserve the final extension so external muxers can
	// infer the container, and it must sit in the destination directory.
	if filepath.Ext(e.Path()) != ".flac" {
		t.Errorf("temp path %q does not preserve the .flac extension", e.Path())
	}
	if filepath.Dir(e.Path()) != dir {
		t.Errorf("temp path %q is not in the destination dir %q", e.Path(), dir)
	}
	if e.Final() != final {
		t.Errorf("Final() = %q, want %q", e.Final(), final)
	}

	// Simulate an external process overwriting the reserved path.
	if err := os.WriteFile(e.Path(), []byte("audio"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := e.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile(final): %v", err)
	}
	if string(got) != "audio" {
		t.Fatalf("final content = %q, want %q", got, "audio")
	}
	// Only the final file remains; no temp left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (only the final file)", len(entries))
	}
}

func TestExternalExtensionlessFinalStagesExtensionless(t *testing.T) {
	// A dot before the random suffix would create a false numeric extension.
	dir := t.TempDir()
	final := filepath.Join(dir, "track")

	e, err := NewExternal(final)
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()

	if ext := filepath.Ext(e.Path()); ext != "" {
		t.Errorf("extensionless final produced temp %q with pseudo-extension %q, want none", e.Path(), ext)
	}
	if filepath.Dir(e.Path()) != dir {
		t.Errorf("temp path %q is not in the destination dir %q", e.Path(), dir)
	}
}

func TestExternalDottedFinalPreservesExtension(t *testing.T) {
	// Preserve extensions even when their container is unknown.
	dir := t.TempDir()
	e, err := NewExternal(filepath.Join(dir, "my.track.v1"))
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()
	if ext := filepath.Ext(e.Path()); ext != ".v1" {
		t.Errorf("temp path %q did not preserve the .v1 extension (got %q)", e.Path(), ext)
	}
}

func TestExternalDiscardRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	e, err := NewExternal(filepath.Join(dir, "out.mp3"))
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := os.WriteFile(e.Path(), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := e.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	// Discard is idempotent and safe after the temp is already gone.
	if err := e.Discard(); err != nil {
		t.Fatalf("second Discard: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("dir has %d entries, want 0 after Discard", len(entries))
	}
}

func TestExternalDiscardAfterCommitIsNoop(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.wav")

	e, err := NewExternal(final)
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := os.WriteFile(e.Path(), []byte("data"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := e.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := e.Discard(); err != nil {
		t.Fatalf("Discard after Commit: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("committed file should still exist, stat err = %v", err)
	}
}

func TestExternalNoExtension(t *testing.T) {
	dir := t.TempDir()
	e, err := NewExternal(filepath.Join(dir, "noext"))
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()
	// With no extension to preserve there is nothing to assert about one; the
	// temp must still land in the destination directory for an atomic rename.
	if filepath.Dir(e.Path()) != dir {
		t.Errorf("temp path %q is not in the destination dir %q", e.Path(), dir)
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
