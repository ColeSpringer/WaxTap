// Package testfs denies a test process access to a path the way its
// platform does, so a test can prove what a run reports when the filesystem
// refuses it. On unix that is a 0000 mode. Windows ignores the mode bits
// (Mkdir drops its argument; Open maps only the write bit onto the
// read-only attribute, which still opens for reading), so there it is a
// deny ACE written with icacls, the in-box tool, which keeps the module
// free of a dependency it would use in tests alone.
package testfs

import (
	"os"
	"testing"
)

// DenyAccess makes path unreadable when it is a file and unwritable when it
// is a directory, for the rest of the test, and restores what it found on
// cleanup so the test's TempDir can be removed. It skips the test only where
// the platform cannot refuse the caller at all: root on unix, and a Windows
// without icacls. A deny that icacls refuses to write fails the test.
func DenyAccess(t testing.TB, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	denyAccess(t, path, fi)
}
