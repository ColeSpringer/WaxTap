//go:build !windows

package testfs

import (
	"os"
	"testing"
)

func denyAccess(t testing.TB, path string, fi os.FileInfo) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a 0000 mode")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	// The mode the path had, so the test sees the file it staged again and
	// not one this helper decided on.
	restore := fi.Mode().Perm()
	t.Cleanup(func() { _ = os.Chmod(path, restore) })
}
