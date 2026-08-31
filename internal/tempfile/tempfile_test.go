package tempfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewWrapsFailureAsOutputError(t *testing.T) {
	// A temp under a missing directory fails at os.CreateTemp.
	bad := filepath.Join(t.TempDir(), "missing-dir", "out.bin")
	_, err := New(bad)
	if err == nil {
		t.Fatal("New into a missing directory should fail")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	// The message names the destination, not the random ".part" staging name.
	if msg := err.Error(); !strings.Contains(msg, bad+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, bad)
	}
	if strings.Contains(err.Error(), ".part") {
		t.Errorf("message = %q, leaked the .part staging suffix", err)
	}
}

func TestCommitSyncFailureNamesFinalPath(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")
	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()
	// Close the underlying file before Commit so Sync fails while the message still
	// names the destination.
	if err := f.File.Close(); err != nil {
		t.Fatalf("close fd: %v", err)
	}
	err = f.Commit()
	if err == nil {
		t.Fatal("Commit should fail when the fd is already closed")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, final+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, final)
	}
	if strings.Contains(msg, ".part") {
		t.Errorf("message = %q, leaked the .part staging suffix", msg)
	}
}

func TestCommitRenameFailureNamesFinalPath(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")
	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Put a directory in the way of the final path so Commit fails on rename but
	// still reports the destination. (Removing the staged temp instead would need
	// its open handle gone first on Windows, and closing it fails Commit at sync.)
	if err := os.Mkdir(final, 0o755); err != nil {
		t.Fatalf("mkdir obstacle: %v", err)
	}
	err = f.Commit()
	if err == nil {
		t.Fatal("Commit should fail when a directory blocks the final path")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, final+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, final)
	}
	if strings.Contains(msg, ".part") {
		t.Errorf("message = %q, leaked the .part staging suffix", msg)
	}
}

func TestCommit(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()

	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}

	// No stray .part files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (only the final file)", len(entries))
	}
}

func TestDiscardRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := f.Write([]byte("partial")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Fatalf("final file should not exist after Discard, stat err = %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("dir has %d entries, want 0 after Discard", len(entries))
	}
}

func TestDiscardAfterCommitIsNoop(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := f.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Discard after a successful Commit must not remove the committed file.
	if err := f.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("committed file should still exist, stat err = %v", err)
	}
}

func TestExternalCommit(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.flac")

	e, err := NewExternal(final, "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()

	// The temp path must preserve the final extension so external muxers can
	// infer the container, and it must sit in the destination directory.
	if filepath.Ext(e.Path()) != ".flac" {
		t.Errorf("temp path %q does not preserve the .flac extension", e.Path())
	}
	if filepath.Dir(e.Path()) != dir {
		t.Errorf("temp path %q is not in the destination dir %q", e.Path(), dir)
	}
	if e.Final() != final {
		t.Errorf("Final() = %q, want %q", e.Final(), final)
	}

	// Simulate an external process overwriting the reserved path.
	if err := os.WriteFile(e.Path(), []byte("audio"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := e.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile(final): %v", err)
	}
	if string(got) != "audio" {
		t.Fatalf("final content = %q, want %q", got, "audio")
	}
	// Only the final file remains; no temp left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (only the final file)", len(entries))
	}
}

func TestExternalExtensionlessFinalStagesExtensionless(t *testing.T) {
	// A dot before the random suffix would create a false numeric extension.
	dir := t.TempDir()
	final := filepath.Join(dir, "track")

	e, err := NewExternal(final, "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()

	if ext := filepath.Ext(e.Path()); ext != "" {
		t.Errorf("extensionless final produced temp %q with pseudo-extension %q, want none", e.Path(), ext)
	}
	if filepath.Dir(e.Path()) != dir {
		t.Errorf("temp path %q is not in the destination dir %q", e.Path(), dir)
	}
}

func TestExternalDottedFinalPreservesExtension(t *testing.T) {
	// Preserve extensions even when their container is unknown.
	dir := t.TempDir()
	e, err := NewExternal(filepath.Join(dir, "my.track.v1"), "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()
	if ext := filepath.Ext(e.Path()); ext != ".v1" {
		t.Errorf("temp path %q did not preserve the .v1 extension (got %q)", e.Path(), ext)
	}
}

func TestExternalDiscardRemovesTemp(t *testing.T) {
	dir := t.TempDir()
	e, err := NewExternal(filepath.Join(dir, "out.mp3"), "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := os.WriteFile(e.Path(), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := e.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	// Discard is idempotent and safe after the temp is already gone.
	if err := e.Discard(); err != nil {
		t.Fatalf("second Discard: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("dir has %d entries, want 0 after Discard", len(entries))
	}
}

func TestExternalDiscardAfterCommitIsNoop(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.wav")

	e, err := NewExternal(final, "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := os.WriteFile(e.Path(), []byte("data"), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	if err := e.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := e.Discard(); err != nil {
		t.Fatalf("Discard after Commit: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("committed file should still exist, stat err = %v", err)
	}
}

func TestExternalNoExtension(t *testing.T) {
	dir := t.TempDir()
	e, err := NewExternal(filepath.Join(dir, "noext"), "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()
	// With no extension to preserve there is nothing to assert about one; the
	// temp must still land in the destination directory for an atomic rename.
	if filepath.Dir(e.Path()) != dir {
		t.Errorf("temp path %q is not in the destination dir %q", e.Path(), dir)
	}
}

func TestExternalStageExtOverridesContainer(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "track") // no extension
	for _, ext := range []string{"webm", ".m4a"} {
		e, err := NewExternal(final, ext)
		if err != nil {
			t.Fatalf("NewExternal(%q): %v", ext, err)
		}
		want := ext
		if want[0] != '.' {
			want = "." + want
		}
		if got := filepath.Ext(e.Path()); got != want {
			t.Errorf("ext %q: staged path %q has extension %q, want %q", ext, e.Path(), got, want)
		}
		if e.Final() != final {
			t.Errorf("Final() = %q, want the extensionless %q", e.Final(), final)
		}
		_ = e.Discard()
	}
}

func TestNewExternalWrapsFailureAsOutputError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "missing-dir", "out.flac")
	_, err := NewExternal(bad, "")
	if err == nil {
		t.Fatal("NewExternal into a missing directory should fail")
	}
	if _, ok := errors.AsType[*OutputError](err); !ok {
		t.Fatalf("err = %v (%T), want *OutputError", err, err)
	}
	// The message names the destination, not the random staging name.
	if msg := err.Error(); !strings.Contains(msg, bad+":") {
		t.Errorf("message = %q, want it to name the destination %q", msg, bad)
	}
}

func TestScratchCleanup(t *testing.T) {
	dir := t.TempDir()
	f, cleanup, err := Scratch(dir, "")
	if err != nil {
		t.Fatalf("Scratch: %v", err)
	}
	name := f.Name()
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("scratch file should exist: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("scratch file should be gone after cleanup, stat err = %v", err)
	}
	// Idempotent.
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
}

// TestCommitNew publishes onto a free path and leaves nothing staged behind.
func TestCommitNew(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.CommitNew(); err != nil {
		t.Fatalf("CommitNew: %v", err)
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1 (only the final file); the staged link was not removed", len(entries))
	}
}

// TestCommitNewRefusesOccupiedPath is the F6 guarantee: the publish itself is
// what claims the path, so the loser fails and the existing file is untouched.
func TestCommitNewRefusesOccupiedPath(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(final, []byte("winner"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f, err := New(final)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer f.Discard()
	if _, err := f.Write([]byte("loser")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = f.CommitNew()
	if err == nil {
		t.Fatal("CommitNew onto an existing path succeeded, want fs.ErrExist")
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CommitNew error = %v, want it to wrap fs.ErrExist", err)
	}
	var oe *OutputError
	if !errors.As(err, &oe) {
		t.Fatalf("CommitNew error = %T, want an *OutputError", err)
	}
	if oe.Op != "publish" {
		t.Errorf("Op = %q, want %q", oe.Op, "publish")
	}
	if !strings.Contains(err.Error(), final) {
		t.Errorf("message = %q, want it to name the destination", err.Error())
	}

	got, _ := os.ReadFile(final)
	if string(got) != "winner" {
		t.Fatalf("existing file = %q, want it left intact", got)
	}
	// Discard must still clear the staged file the loser wrote.
	if err := f.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1; the failed publish left a temp behind", len(entries))
	}
}

// TestExternalCommitNew mirrors TestCommitNew for the staged-by-another-writer
// type, which is the seam Process and Download publish through.
func TestExternalCommitNew(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.flac")

	e, err := NewExternal(final, "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	defer e.Discard()
	if filepath.Ext(e.Path()) != ".flac" {
		t.Fatalf("staged path %q, want it to keep the destination extension", e.Path())
	}
	if err := os.WriteFile(e.Path(), []byte("audio"), 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	if err := e.CommitNew(); err != nil {
		t.Fatalf("CommitNew: %v", err)
	}
	if got, _ := os.ReadFile(final); string(got) != "audio" {
		t.Fatalf("content = %q, want %q", got, "audio")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1", len(entries))
	}

	// A second run staging the same destination loses the race.
	e2, err := NewExternal(final, "")
	if err != nil {
		t.Fatalf("NewExternal (second): %v", err)
	}
	defer e2.Discard()
	if err := os.WriteFile(e2.Path(), []byte("other"), 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	if err := e2.CommitNew(); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CommitNew error = %v, want fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(final); string(got) != "audio" {
		t.Fatalf("delivered file = %q, want the winner's bytes", got)
	}
}

// TestPublishNewRefusesOccupiedPath covers the link path directly, without a
// staged File or External in the way.
func TestPublishNewRefusesOccupiedPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "staged.bin")
	dst := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatalf("seed src: %v", err)
	}
	if err := os.WriteFile(dst, []byte("winner"), 0o644); err != nil {
		t.Fatalf("seed dst: %v", err)
	}
	if err := PublishNew(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("PublishNew onto an occupied path = %v, want fs.ErrExist", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "winner" {
		t.Fatalf("destination = %q, want it untouched", got)
	}

	free := filepath.Join(dir, "free.bin")
	if err := PublishNew(src, free); err != nil {
		t.Fatalf("PublishNew onto a free path: %v", err)
	}
	if got, _ := os.ReadFile(free); string(got) != "payload" {
		t.Fatalf("published = %q, want %q", got, "payload")
	}
	if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("source still present after PublishNew (stat err = %v)", err)
	}
}

// TestPublishNewFallsBackWithoutHardLinks covers the FAT/exFAT/SMB path, where
// os.Link is unavailable and the publish degrades to a stat and a rename. It
// forces that branch through the link seam, because a test filesystem that
// supports hard links never reaches it otherwise: without this, a regression
// that dropped the exists check and started overwriting on those filesystems
// would pass every test in this package.
func TestPublishNewFallsBackWithoutHardLinks(t *testing.T) {
	noLinks := func(t *testing.T) {
		t.Helper()
		prev := link
		link = func(string, string) error {
			return &os.LinkError{Op: "link", Err: errors.ErrUnsupported}
		}
		t.Cleanup(func() { link = prev })
	}

	t.Run("free path publishes by rename", func(t *testing.T) {
		noLinks(t)
		dir := t.TempDir()
		src := filepath.Join(dir, "staged.bin")
		dst := filepath.Join(dir, "out.bin")
		if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
			t.Fatalf("seed src: %v", err)
		}
		if err := PublishNew(src, dst); err != nil {
			t.Fatalf("PublishNew with no link support: %v", err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "payload" {
			t.Errorf("published = %q, want %q", got, "payload")
		}
		if _, err := os.Stat(src); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("source still present after the rename fallback (stat err = %v)", err)
		}
	})

	// The fallback is not atomic against a concurrent writer, but it must still
	// refuse a destination that is already there.
	t.Run("occupied path is still refused", func(t *testing.T) {
		noLinks(t)
		dir := t.TempDir()
		src := filepath.Join(dir, "staged.bin")
		dst := filepath.Join(dir, "out.bin")
		if err := os.WriteFile(src, []byte("loser"), 0o644); err != nil {
			t.Fatalf("seed src: %v", err)
		}
		if err := os.WriteFile(dst, []byte("winner"), 0o644); err != nil {
			t.Fatalf("seed dst: %v", err)
		}
		err := PublishNew(src, dst)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("PublishNew onto an occupied path = %v, want fs.ErrExist", err)
		}
		if got, _ := os.ReadFile(dst); string(got) != "winner" {
			t.Errorf("destination = %q, want it untouched by the fallback", got)
		}
	})
}

// TestDiscardClearsAStagedLeftover covers the unlink that PublishNew is allowed
// to lose: the destination is published either way, and Discard is the retry
// that keeps a staged file (which carries the destination's extension) from
// being picked up later as a real input.
func TestDiscardClearsAStagedLeftover(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "out.flac")

	e, err := NewExternal(final, "")
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := os.WriteFile(e.Path(), []byte("audio"), 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	if err := e.CommitNew(); err != nil {
		t.Fatalf("CommitNew: %v", err)
	}
	// Stand in for an unlink that lost the race: the publish succeeded, and the
	// staged copy is still on disk with an audio extension.
	if err := os.WriteFile(e.Path(), []byte("leftover"), 0o644); err != nil {
		t.Fatalf("recreate staged: %v", err)
	}
	if err := e.Discard(); err != nil {
		t.Fatalf("Discard after commit: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("dir has %d entries, want 1; Discard did not clear the leftover", len(entries))
	}
	if got, _ := os.ReadFile(final); string(got) != "audio" {
		t.Errorf("published file = %q, want it untouched by Discard", got)
	}
}

func TestNumberedVariant(t *testing.T) {
	join := filepath.Join
	dir := "/out"
	for _, tc := range []struct {
		path string
		n    int
		want string
	}{
		{join(dir, "t.flac"), 1, join(dir, "t (1).flac")},
		{join(dir, "t.flac"), 12, join(dir, "t (12).flac")},
		// Appending is unconditional: a parenthesized number in the requested
		// name may be title content ("Symphony No. 5 (2)"), so the pre-flight
		// must never strip it. Continuing an existing sequence is the publish
		// retry's job, expressed through splitNumbered.
		{join(dir, "t (3).flac"), 1, join(dir, "t (3) (1).flac")},
		{join(dir, "noext"), 1, join(dir, "noext (1)")},
		// filepath.Ext reads a leading-dot name as all extension, so the number
		// lands before it. Odd, but it is the convention the CLI pre-flight has
		// always used and this function exists to be the single copy of it.
		{join(dir, ".hidden"), 1, join(dir, " (1).hidden")},
		{join(dir, "t (x).flac"), 1, join(dir, "t (x) (1).flac")},
		{join(dir, "t ().flac"), 1, join(dir, "t () (1).flac")},
	} {
		if got := NumberedVariant(tc.path, tc.n); got != tc.want {
			t.Errorf("NumberedVariant(%q, %d) = %q, want %q", tc.path, tc.n, got, tc.want)
		}
	}
}

func TestSplitNumbered(t *testing.T) {
	join := filepath.Join
	dir := "/out"
	for _, tc := range []struct {
		path     string
		wantBase string
		wantN    int
	}{
		{join(dir, "t.flac"), join(dir, "t.flac"), 0},
		{join(dir, "t (3).flac"), join(dir, "t.flac"), 3},
		{join(dir, "t (x).flac"), join(dir, "t (x).flac"), 0},
		{join(dir, "t ().flac"), join(dir, "t ().flac"), 0},
		// Only the outermost suffix is one publish generation's worth.
		{join(dir, "t (2) (3).flac"), join(dir, "t (2).flac"), 3},
	} {
		base, n := splitNumbered(tc.path)
		if base != tc.wantBase || n != tc.wantN {
			t.Errorf("splitNumbered(%q) = %q, %d; want %q, %d", tc.path, base, n, tc.wantBase, tc.wantN)
		}
	}
}

// An auto-number pre-flight picks a free name by stat, which a concurrent
// writer can take before the publish. Renumbering at publish is what makes N
// racing runs produce N files instead of N-1 plus a failure.
func TestPublishNewNumbered(t *testing.T) {
	stage := func(t *testing.T, dir, content string) string {
		t.Helper()
		src := filepath.Join(dir, "staged.part")
		if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return src
	}
	occupy := func(t *testing.T, paths ...string) {
		t.Helper()
		for _, p := range paths {
			if err := os.WriteFile(p, []byte("taken"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("free path publishes exactly", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "out.flac")
		got, err := PublishNewNumbered(stage(t, dir, "mine"), dst)
		if err != nil {
			t.Fatal(err)
		}
		if got != dst {
			t.Errorf("published %q, want %q", got, dst)
		}
	})

	t.Run("renumbers past taken paths", func(t *testing.T) {
		for taken, want := range map[int]string{1: "out (1).flac", 2: "out (2).flac"} {
			dir := t.TempDir()
			dst := filepath.Join(dir, "out.flac")
			occupy(t, dst)
			for n := 1; n < taken; n++ {
				occupy(t, NumberedVariant(dst, n))
			}
			got, err := PublishNewNumbered(stage(t, dir, "mine"), dst)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(got) != want {
				t.Errorf("published %q, want %q", filepath.Base(got), want)
			}
			b, err := os.ReadFile(got)
			if err != nil || string(b) != "mine" {
				t.Errorf("published content = %q, %v; want %q", b, err, "mine")
			}
		}
	})

	t.Run("an already numbered destination continues its sequence", func(t *testing.T) {
		// The pre-flight picked "out (3).flac" because out..out (2) were taken;
		// a racing writer then took it too. The retry continues at (4) rather
		// than nesting a second suffix that would grow on every retry.
		dir := t.TempDir()
		dst := filepath.Join(dir, "out (3).flac")
		occupy(t, dst)
		got, err := PublishNewNumbered(stage(t, dir, "mine"), dst)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(got) != "out (4).flac" {
			t.Errorf("published %q, want %q", filepath.Base(got), "out (4).flac")
		}
	})

	t.Run("bounded", func(t *testing.T) {
		dir := t.TempDir()
		dst := filepath.Join(dir, "out.flac")
		occupy(t, dst, NumberedVariant(dst, 1), NumberedVariant(dst, 2))
		_, err := publishNewNumbered(stage(t, dir, "mine"), dst, 2)
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("err = %v, want it to unwrap to fs.ErrExist", err)
		}
		if _, ok := errors.AsType[*OutputError](err); !ok {
			t.Errorf("err = %v (%T), want an *OutputError", err, err)
		}
		// Exhaustion is a distinct condition: advising --collision auto-number
		// would be advising the mode that just failed.
		if !errors.Is(err, ErrRenumberExhausted) {
			t.Errorf("err = %v, want it to unwrap to ErrRenumberExhausted", err)
		}
	})

	t.Run("other failures surface unchanged", func(t *testing.T) {
		dir := t.TempDir()
		// A destination directory that does not exist is not a collision, so it
		// must fail immediately rather than being retried 1000 times.
		_, err := PublishNewNumbered(stage(t, dir, "mine"), filepath.Join(dir, "nope", "out.flac"))
		if err == nil {
			t.Fatal("err = nil, want the missing-directory failure")
		}
		if errors.Is(err, fs.ErrExist) {
			t.Errorf("err = %v, want it reported as itself, not as a collision", err)
		}
	})
}
