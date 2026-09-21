//go:build windows

package testfs

import (
	"os"
	"os/exec"
	"testing"
)

// everyone is the well-known Everyone SID, spelled the way icacls takes a
// SID rather than a name, so no locale is involved.
const everyone = "*S-1-1-0"

// denyAccess writes a deny ACE for exactly what the test needs refused,
// so a stat still answers: read data on a file (Open needs it, Stat does
// not), add file and add subdirectory on a directory (CreateTemp needs
// the first). The owner keeps WRITE_DAC implicitly whatever the DACL says,
// so the cleanup can lift its own deny before TempDir removes the tree.
//
// Only a missing icacls skips. A deny that fails is a test failure, not a
// skip: skipping there would leave the Windows leg green with both callers
// silently untested, which is the state this helper exists to end. The CI
// runner's temp root is NTFS.
func denyAccess(t testing.TB, path string, fi os.FileInfo) {
	t.Helper()
	icacls, err := exec.LookPath("icacls")
	if err != nil {
		t.Skip("icacls is not on PATH")
	}
	rights := "(RD)"
	if fi.IsDir() {
		rights = "(WD,AD)"
	}
	if out, err := exec.Command(icacls, path, "/deny", everyone+":"+rights).CombinedOutput(); err != nil {
		t.Fatalf("icacls could not deny access on %s: %v: %s", path, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command(icacls, path, "/remove:d", everyone).CombinedOutput(); err != nil {
			t.Errorf("icacls could not lift the deny on %s, so TempDir may fail to remove it: %v: %s", path, err, out)
		}
	})
}
