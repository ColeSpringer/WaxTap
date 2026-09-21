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
	// Both denials happen in subtests, so a helper that skips under root
	// would leave the closing checks passing over a path nothing ever
	// denied. Geteuid is -1 on Windows, where the deny ACE refuses even the
	// administrator who wrote it.
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a 0000 mode")
	}
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
	if f, err := os.CreateTemp(sub, "y"); err != nil {
		t.Errorf("after cleanup, CreateTemp = %v, want the directory writable again", err)
	} else {
		f.Close()
	}
}
