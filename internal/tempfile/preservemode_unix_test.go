//go:build unix

package tempfile

import (
	"os"
	"path/filepath"
	"testing"
)

// Replacing a file discards its permissions today: the staged temp carries the
// umask's mode and the rename brings that with it, so a read-only or
// group-restricted output silently widens on the next overwrite. The content is
// still replaced without a prompt, which is the documented behavior; only the
// mode is preserved.
func TestCommitPreservesReplacedMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o444, 0o600, 0o640} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "out.bin")
			if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dst, mode); err != nil {
				t.Fatal(err)
			}

			f, err := New(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Discard()
			if _, err := f.Write([]byte("new")); err != nil {
				t.Fatal(err)
			}
			if err := f.Commit(); err != nil {
				t.Fatal(err)
			}

			fi, err := os.Stat(dst)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != mode {
				t.Errorf("mode after overwrite = %v, want %v", got, mode)
			}
			b, err := os.ReadFile(dst)
			if err != nil || string(b) != "new" {
				t.Errorf("content = %q, %v; want %q (the mode survives, the content is replaced)", b, err, "new")
			}
		})
	}
}

// A destination that does not exist has no mode to inherit, so the umask
// decides as it always has.
func TestCommitAbsentDestinationUsesUmask(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "fresh.bin")
	f, err := New(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Discard()
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := 0o666 &^ readUmask(t); fi.Mode().Perm() != want {
		t.Errorf("mode = %v, want the umask default %v", fi.Mode().Perm(), want)
	}
}

// An exclusive publish never replaces anything, so there is no mode to inherit
// and the umask decides.
func TestCommitNewUsesUmask(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "fresh.bin")
	f, err := New(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Discard()
	if err := f.CommitNew(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if want := 0o666 &^ readUmask(t); fi.Mode().Perm() != want {
		t.Errorf("mode = %v, want the umask default %v", fi.Mode().Perm(), want)
	}
}

// A destination that is not a regular file (a directory, a device) has no mode
// worth inheriting; the publish fails on its own terms rather than here.
func TestCommitDirectoryDestinationKeepsUmask(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "adir")
	if err := os.Mkdir(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := New(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Discard()
	// The rename onto a directory fails; what matters is that preservation did
	// not chmod the staged file to the directory's 0700 first, which the staged
	// file's own mode proves.
	if err := f.Commit(); err == nil {
		t.Fatal("Commit onto a directory succeeded; the test's premise is gone")
	}
	fi, err := os.Stat(f.tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := 0o666 &^ readUmask(t); fi.Mode().Perm() != want {
		t.Errorf("staged mode = %v, want the umask default %v (the directory's mode must not be inherited)", fi.Mode().Perm(), want)
	}
	if fi, err := os.Stat(dst); err == nil && !fi.IsDir() {
		t.Error("the directory was replaced")
	}
}

// os.Rename over a symlink replaces the link itself and leaves its target
// untouched, so there is no replaced file whose mode could be preserved:
// following the link would copy permissions from a file this run never
// modifies.
func TestCommitSymlinkDestinationKeepsUmask(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o777); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.bin")
	if err := os.Symlink(target, dst); err != nil {
		t.Fatal(err)
	}

	f, err := New(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Discard()
	if err := f.Commit(); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the rename should have replaced the symlink with a regular file")
	}
	if want := 0o666 &^ readUmask(t); fi.Mode().Perm() != want {
		t.Errorf("mode = %v, want the umask default %v, not the link target's 0777", fi.Mode().Perm(), want)
	}
	ti, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if ti.Mode().Perm() != 0o777 {
		t.Errorf("target mode = %v; the target must be untouched", ti.Mode().Perm())
	}
	if b, _ := os.ReadFile(target); string(b) != "target" {
		t.Error("target content changed; the rename must replace the link, not the target")
	}
}
