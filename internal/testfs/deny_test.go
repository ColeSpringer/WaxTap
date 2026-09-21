package testfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// DenyAccess refuses the process that asked for it: a denied file will not
// open for reading but still answers a stat, a denied directory takes no
// new file, and both come back once the subtest's cleanup has run, which is
// what lets the TempDir go.
func TestDenyAccessRefusesTheCaller(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "d")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Run("file", func(t *testing.T) {
		DenyAccess(t, file)
		f, err := os.Open(file)
		if err == nil {
			f.Close()
			t.Fatal("Open succeeded on a denied file")
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("Open = %v, want a permission error", err)
		}
		if _, err := os.Stat(file); err != nil {
			t.Errorf("Stat = %v, want the denied file still visible", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		DenyAccess(t, sub)
		f, err := os.CreateTemp(sub, "x")
		if err == nil {
			f.Close()
			t.Fatal("CreateTemp succeeded in a denied directory")
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("CreateTemp = %v, want a permission error", err)
		}
	})
	if f, err := os.Open(file); err != nil {
		t.Errorf("after cleanup, Open = %v, want the file readable again", err)
	} else {
		f.Close()
	}
	// What it was staged with, not a mode the helper decided on: a test that
	// asserts on the mode it wrote has to see that mode again.
	if fi, err := os.Stat(file); err != nil {
		t.Error(err)
	} else if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("after cleanup the file is %v, want the 0644 it was staged with", got)
	}
	if f, err := os.CreateTemp(sub, "y"); err != nil {
		t.Errorf("after cleanup, CreateTemp = %v, want the directory writable again", err)
	} else {
		f.Close()
	}
}
