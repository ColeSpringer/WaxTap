//go:build unix

package tempfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// readUmask reads and restores the current process umask.
func readUmask(t *testing.T) os.FileMode {
	t.Helper()
	old := syscall.Umask(0)
	syscall.Umask(old)
	return os.FileMode(old)
}

func TestNew_HonorsUmask(t *testing.T) {
	want := os.FileMode(0o666) &^ readUmask(t)

	final := filepath.Join(t.TempDir(), "out.bin")
	f, err := New(final)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("data"); err != nil {
		t.Fatal(err)
	}
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("New output mode = %o, want %o (0666 &^ umask); CreateTemp's private 0600 must be widened", got, want)
	}
}

func TestNewExternal_HonorsUmask(t *testing.T) {
	want := os.FileMode(0o666) &^ readUmask(t)

	final := filepath.Join(t.TempDir(), "out.flac")
	ext, err := NewExternal(final)
	if err != nil {
		t.Fatal(err)
	}
	// An external writer truncates and rewrites the reserved path; an O_TRUNC
	// reopen keeps the existing mode, so the requested 0600 here is ignored.
	if err := os.WriteFile(ext.Path(), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ext.Commit(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("NewExternal output mode = %o, want %o (0666 &^ umask)", got, want)
	}
}
