package tempfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewWrapsFailureAsOutputError(t *testing.T) {
	// A temp under a missing directory fails at os.CreateTemp.
	bad := filepath.Join(t.TempDir(), "missing-dir", "out.bin")
	_, err := New(bad)
	if err == nil {
		t.Fatal("New into a missing directory should fail")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	// The message names the destination, not the random ".part" staging name.
	if msg := err.Error(); !strings.Contains(msg, bad+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, bad)
	}
	if strings.Contains(err.Error(), ".part") {
		t.Errorf("message = %q, leaked the .part staging suffix", err)
	}
}

func TestCommitSyncFailureNamesFinalPath(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")
	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()
	// Close the underlying file before Commit so Sync fails while the message still
	// names the destination.
	if err := f.File.Close(); err != nil {
		t.Fatalf("close fd: %v", err)
	}
	err = f.Commit()
	if err == nil {
		t.Fatal("Commit should fail when the fd is already closed")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, final+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, final)
	}
	if strings.Contains(msg, ".part") {
		t.Errorf("message = %q, leaked the .part staging suffix", msg)
	}
}

func TestCommitRenameFailureNamesFinalPath(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")
	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Remove the staged temp so Commit fails on rename but still reports the
	// destination.
	if err := os.Remove(f.tmpPath); err != nil {
		t.Fatalf("remove temp: %v", err)
	}
	err = f.Commit()
	if err == nil {
		t.Fatal("Commit should fail after the staged temp is removed")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, final+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, final)
	}
	if strings.Contains(msg, ".part") {
		t.Errorf("message = %q, leaked the .part staging suffix", msg)
	}
}

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

	e, err := NewExternal(final, "")
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

	e, err := NewExternal(final, "")
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
	e, err := NewExternal(filepath.Join(dir, "my.track.v1"), "")
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
	e, err := NewExternal(filepath.Join(dir, "out.mp3"), "")
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

	e, err := NewExternal(final, "")
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
	e, err := NewExternal(filepath.Join(dir, "noext"), "")
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

func TestExternalStageExtOverridesContainer(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "track") // no extension
	for _, ext := range []string{"webm", ".m4a"} {
		e, err := NewExternal(final, ext)
		if err != nil {
			t.Fatalf("NewExternal(%q): %v", ext, err)
		}
		want := ext
		if want[0] != '.' {
			want = "." + want
		}
		if got := filepath.Ext(e.Path()); got != want {
			t.Errorf("ext %q: staged path %q has extension %q, want %q", ext, e.Path(), got, want)
		}
		if e.Final() != final {
			t.Errorf("Final() = %q, want the extensionless %q", e.Final(), final)
		}
		_ = e.Discard()
	}
}

func TestNewExternalWrapsFailureAsOutputError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing-dir", "out.flac")
	_, err := NewExternal(bad, "")
	if err == nil {
		t.Fatal("NewExternal into a missing directory should fail")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	// The message names the destination, not the random staging name.
	if msg := err.Error(); !strings.Contains(msg, bad+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, bad)
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
