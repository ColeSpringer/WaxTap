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

// A destination whose last component is a symlink is written through the link:
// the staged file is placed beside the target and renamed over it, so the link
// survives and the target is replaced. A user who symlinks an output name onto
// another disk keeps their redirection rather than having the first run that
// writes there silently replace it.
//
// There is a replaced file now, the target, so its mode is preserved through
// the rename the way every other replacement's is.
func TestCommitSymlinkDestinationWritesThroughTheLink(t *testing.T) {
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
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced; the write must go through it")
	}
	ti, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if ti.Mode().Perm() != 0o777 {
		t.Errorf("target mode = %v, want the replaced file's 0777 preserved through the rename", ti.Mode().Perm())
	}
	if b, _ := os.ReadFile(target); string(b) != "" {
		t.Errorf("target content = %q, want the newly written (empty) file", b)
	}
}

// A dangling link has no target to write through, so the rename replaces the
// link itself: that is the only answer the filesystem offers.
func TestCommitDanglingSymlinkIsReplaced(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")
	if err := os.Symlink(filepath.Join(dir, "absent.bin"), dst); err != nil {
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
		t.Fatal("a dangling link must be replaced by the written file")
	}
}

// The rename publish answers the same way as the staged one. They are the same
// publish reached by different routes (a staging file on another filesystem
// takes the rename path), so a link that survives one must survive the other.
func TestResolveLinkIsSharedByBothPublishRoutes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "out.bin")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLink(link); got != target {
		t.Errorf("ResolveLink(%q) = %q, want the target %q", link, got, target)
	}
	// A plain path and a dangling link are their own answers.
	if got := ResolveLink(target); got != target {
		t.Errorf("ResolveLink on a regular file = %q, want it unchanged", got)
	}
	dangling := filepath.Join(dir, "dangling.bin")
	if err := os.Symlink(filepath.Join(dir, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLink(dangling); got != dangling {
		t.Errorf("ResolveLink on a dangling link = %q, want it unchanged", got)
	}
}
