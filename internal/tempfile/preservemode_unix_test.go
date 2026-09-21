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

// Only the last component is followed. A parent directory that is itself a
// symlink is the caller's own spelling of where the file lives, and rewriting
// it hands back a path the caller never named: on macOS every temp directory
// sits under /var, a link to /private/var, so resolving the whole path answers
// /private/var. The plain-path answer is returned untouched, so a whole-path
// resolve also publishes one directory under two spellings depending on
// whether the name happened to be a link.
func TestResolveLinkKeepsTheCallersDirectory(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "private", "d")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "d")
	if err := os.Symlink(real, dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "out.bin")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLink(link); got != target {
		t.Errorf("ResolveLink(%q) = %q, want %q: a symlinked parent is not this function's to rewrite", link, got, target)
	}
	if got := ResolveLink(target); got != target {
		t.Errorf("ResolveLink on a regular file = %q, want it unchanged", got)
	}
}

// A link may name another link, and the publish has to reach the file at the
// end of the chain rather than stop on an intermediate link. A relative target
// is read against the link's own directory. A cycle ends nowhere, so it
// answers like a dangling link: the rename replaces the link itself.
func TestResolveLinkFollowsAChainAndSurvivesACycle(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.bin")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(dir, "mid.bin")
	if err := os.Symlink(target, mid); err != nil {
		t.Fatal(err)
	}
	head := filepath.Join(dir, "head.bin")
	if err := os.Symlink(mid, head); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLink(head); got != target {
		t.Errorf("ResolveLink through a chain = %q, want the file at its end %q", got, target)
	}

	rel := filepath.Join(dir, "rel.bin")
	if err := os.Symlink("target.bin", rel); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLink(rel); got != target {
		t.Errorf("ResolveLink on a relative link = %q, want %q", got, target)
	}

	a := filepath.Join(dir, "a.bin")
	b := filepath.Join(dir, "b.bin")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLink(a); got != a {
		t.Errorf("ResolveLink on a cycle = %q, want the link itself %q", got, a)
	}
}

// A relative target's ".." is taken after the parent links are followed, so it
// cannot be cleaned away lexically: under a symlinked directory the two name
// different files. Getting it wrong is not a cosmetic miss. The wrong path
// does not exist, the link is left to be replaced, and the publish renames
// over the redirection this function exists to preserve.
func TestResolveLinkTakesDotDotAfterTheParentLink(t *testing.T) {
	root := t.TempDir()
	music := filepath.Join(root, "big", "music")
	storage := filepath.Join(root, "big", "storage")
	for _, d := range []string{music, storage} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(storage, "out.flac")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// dir is a link to music, so "../storage" off it means big/storage to the
	// kernel and root/storage to filepath.Join.
	dir := filepath.Join(root, "music")
	if err := os.Symlink(music, dir); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "out.flac")
	if err := os.Symlink(filepath.Join("..", "storage", "out.flac"), link); err != nil {
		t.Fatal(err)
	}
	// The kernel's answer, as a control: opening the link reaches the file.
	if b, err := os.ReadFile(link); err != nil || string(b) != "old" {
		t.Fatalf("the link does not reach the target: %q, %v", b, err)
	}
	if got := ResolveLink(link); got != target {
		t.Errorf("ResolveLink(%q) = %q, want %q", link, got, target)
	}
}

// macOS spells every temp directory through a link (/var is /private/var),
// so a ".." that leaves the link's own directory resolves to a path under a
// prefix the caller never spelled. The answer keeps the caller's spelling of
// the nearest directory the target is still under. The tree is reached
// through a root link here, which is that shape on any platform.
func TestResolveLinkKeepsTheCallersSpellingPastTheParent(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real")
	root := filepath.Join(t.TempDir(), "root") // the caller's spelling: a link to real
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"big/music", "big/storage"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "big", "storage", "out.flac")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "big", "music"), filepath.Join(root, "music")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "music", "out.flac")
	if err := os.Symlink(filepath.Join("..", "storage", "out.flac"), link); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLink(link); got != target {
		t.Errorf("ResolveLink(%q) = %q, want %q: the caller's spelling of the root, not the resolved one", link, got, target)
	}
}

// A relative output path stays relative, as "-o out.flac" gives it. The
// resolve runs through EvalSymlinks, which keeps a relative path relative, so
// the prefix handed back has to be the caller's relative one rather than an
// absolute path they never named.
func TestResolveLinkKeepsARelativePathRelative(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.bin"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.bin", filepath.Join(dir, "out.bin")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got := ResolveLink("out.bin"); got != "target.bin" {
		t.Errorf("ResolveLink(\"out.bin\") = %q, want \"target.bin\"", got)
	}
}
