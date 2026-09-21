//go:build unix

package testfs

import (
	"os"
	"path/filepath"
	"testing"
)

// The unix helper takes the mode down to 0000 and puts back the one it
// found, not a plausible default: a test that asserts on the mode it staged
// has to see that mode again. It is staged 0640 so a helper that restored
// 0644 would be caught. Windows has no mode to put back, which is why this
// half lives here.
func TestDenyAccessRestoresTheStagedMode(t *testing.T) {
	// The helper skips under root instead of taking the mode down, and a
	// subtest that skips leaves this one asserting on a mode no cleanup ever
	// put back: a pass it did not earn.
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a 0000 mode")
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Chmod rather than the WriteFile mode, which any umask may cut into.
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	t.Run("denied", func(t *testing.T) {
		DenyAccess(t, file)
		if fi, err := os.Stat(file); err != nil {
			t.Fatal(err)
		} else if got := fi.Mode().Perm(); got != 0 {
			t.Errorf("denied file is %v, want the 0000 the helper sets", got)
		}
	})
	if fi, err := os.Stat(file); err != nil {
		t.Fatal(err)
	} else if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("after cleanup the file is %v, want the 0640 it was staged with", got)
	}
}
