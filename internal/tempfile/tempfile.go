// Package tempfile stages output in the destination directory and publishes it
// with an atomic rename. Failed or canceled work removes the staged file, leaving
// the final path untouched.
//
// The creator owns cleanup. Defer the returned value's Discard method
// immediately after New or NewExternal; Discard is a no-op after Commit succeeds.
package tempfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// OutputError marks an error that occurred while staging or publishing output.
// It unwraps to the underlying cause.
type OutputError struct {
	Op    string // the failed step, such as "create", "chmod", or "rename"
	cause error
}

func (e *OutputError) Error() string { return e.cause.Error() }
func (e *OutputError) Unwrap() error { return e.cause }

// WrapOutput wraps err as an output failure. It returns nil when err is nil.
func WrapOutput(op string, err error) error {
	if err == nil {
		return nil
	}
	return &OutputError{Op: op, cause: err}
}

// PublishNew publishes src as dst without replacing anything already there. src
// must live on the destination's filesystem, which is what New and NewExternal
// guarantee by staging in the destination directory.
//
// os.Link fails with EEXIST when dst exists, on both POSIX and Windows/NTFS, so
// the existence check and the publish are one operation: two processes racing
// for the same output path cannot both succeed. src is removed once the link is
// in place, so the net effect matches Commit's rename.
//
// Filesystems without hard links (FAT, exFAT, some network shares) fall back to
// a stat and an os.Rename. That fallback still refuses an existing destination,
// but the two steps are not atomic, so a concurrent writer can still be lost.
//
// A failure to unlink src after a successful link is not an error: the
// destination is published either way. Discard removes the leftover.
// link is os.Link, replaced in tests to exercise the no-hard-link fallback on a
// filesystem that does support links.
var link = os.Link

func PublishNew(src, dst string) error {
	err := link(src, dst)
	switch {
	case err == nil:
		_ = os.Remove(src)
		return nil
	case errors.Is(err, fs.ErrExist):
		return existsError(dst)
	}
	// Any other link failure is read as "this filesystem will not link". A real
	// permission or path problem fails again on the rename below and is reported
	// from there, so nothing is swallowed.
	if _, statErr := os.Stat(dst); statErr == nil {
		return existsError(dst)
	}
	if err := os.Rename(src, dst); err != nil {
		return WrapOutput("publish", retargetPathError(dst, err))
	}
	return nil
}

// MaxPublishRenumber bounds the renumbering retry. A destination whose first
// thousand siblings are all taken is a runaway loop, not a busy directory.
//
// It is exported for the CLI's auto-number pre-flight, which walks the same
// sequence with a stat before anything is staged. A pre-flight that searched
// further than the publish would hand the publish a name the publish then
// refuses; one constant keeps both ends giving up at the same place.
const MaxPublishRenumber = 1000

// numberedSuffix matches a trailing " (n)" on a file stem.
var numberedSuffix = regexp.MustCompile(`^(.*) \((\d+)\)$`)

// NumberedVariant returns the "stem (n).ext" sibling of path: the one statement
// of the auto-number naming convention, shared by the CLI pre-flight and the
// publish retry. It always appends: a parenthesized number already in the name
// may be title content, which numbering must not delete. Continuing an existing
// " (n)" sequence is the publish retry's job, via splitNumbered.
func NumberedVariant(path string, n int) string {
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	return filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
}

// splitNumbered separates an existing " (n)" stem suffix from path, returning
// the un-numbered path and the number it carried. A path with no such suffix
// comes back unchanged with n == 0.
func splitNumbered(path string) (base string, n int) {
	dir := filepath.Dir(path)
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(filepath.Base(path), ext)
	m := numberedSuffix.FindStringSubmatch(stem)
	if m == nil {
		return path, 0
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return path, 0 // a number too large to be one
	}
	return filepath.Join(dir, m[1]+ext), n
}

// PublishNewNumbered is PublishNew for a destination that may renumber: when the
// publish finds the path taken it retries the next " (n)" sibling, and reports
// the path it actually published. Any other failure returns immediately, so a
// missing directory fails once rather than a thousand times.
//
// A destination already carrying a " (n)" suffix continues that sequence
// ("t (3).flac" retries as "t (4).flac"): the suffix is normally the CLI
// pre-flight's own pick, and nesting a second one would grow the name on every
// race. A literal parenthesized number in a requested name is indistinguishable
// here and is treated the same; the pre-flight, which does know the requested
// name, appends instead (see NumberedVariant).
//
// It exists for auto-number collision policies, whose pre-flight pick is a stat
// and therefore cannot see a writer that claims the name in between. Under
// concurrency the numbering is not deterministic: racing runs can land (2) and
// (3) with (1) belonging to neither.
func PublishNewNumbered(src, dst string) (string, error) {
	return publishNewNumbered(src, dst, MaxPublishRenumber)
}

// ErrRenumberExhausted reports that PublishNewNumbered gave up: the destination
// and every numbered sibling it tried were taken. It rides alongside
// fs.ErrExist so collision classification is unchanged, while messaging can
// stop advising auto-number to the mode that just ran out of numbers.
var ErrRenumberExhausted = errors.New("every numbered variant was taken")

// publishNewNumbered is PublishNewNumbered with an explicit bound, so tests can
// reach the exhausted case without creating a thousand files.
func publishNewNumbered(src, dst string, cap int) (string, error) {
	base, start := splitNumbered(dst)
	candidate := dst
	for n := start; ; n++ {
		err := PublishNew(src, candidate)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
		if n-start >= cap {
			return "", WrapOutput("publish", &os.PathError{
				Op: "publish", Path: dst,
				Err: fmt.Errorf("%w: %w", ErrRenumberExhausted, fs.ErrExist),
			})
		}
		candidate = NumberedVariant(base, n+1)
	}
}

// existsError reports an occupied destination. It unwraps to fs.ErrExist so
// callers can tell a collision from an I/O failure.
func existsError(dst string) error {
	return WrapOutput("publish", &os.PathError{Op: "publish", Path: dst, Err: fs.ErrExist})
}

// retargetPathError replaces temp paths in OS errors with finalPath. That keeps
// output errors focused on the requested destination. Rename errors arrive as
// *os.LinkError, which is converted to *os.PathError for a single-path message.
func retargetPathError(finalPath string, err error) error {
	if pe, ok := errors.AsType[*os.PathError](err); ok {
		return &os.PathError{Op: pe.Op, Path: finalPath, Err: pe.Err}
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return &os.PathError{Op: le.Op, Path: finalPath, Err: le.Err}
	}
	return err
}

// File is a staged output: write to it, then Commit (atomic rename to the final
// path) or Discard (remove the temp). The temp is created in the final path's
// directory so the rename stays on one filesystem and is therefore atomic.
type File struct {
	*os.File
	finalPath string
	tmpPath   string
	committed bool
	closed    bool
}

func (f *File) close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return f.File.Close()
}

// New creates a staging file for eventual atomic rename to finalPath. The
// returned *File embeds *os.File, so callers write to it directly.
//
// A finalPath whose last component is a symlink is written through the link:
// see resolveLink.
func New(finalPath string) (*File, error) {
	finalPath = ResolveLink(finalPath)
	dir := filepath.Dir(finalPath)
	base := filepath.Base(finalPath)
	f, err := os.CreateTemp(dir, base+".*.part")
	if err != nil {
		return nil, WrapOutput("create", retargetPathError(finalPath, err))
	}
	if err := chmodUmask(f.Name()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, WrapOutput("chmod", retargetPathError(finalPath, err))
	}
	return &File{File: f, finalPath: finalPath, tmpPath: f.Name()}, nil
}

// ResolveLink follows a final path whose last component is a symlink to the
// file it names, so a publish replaces the target and the link survives. A
// user who symlinks an output name onto another disk keeps their redirection
// rather than having it silently replaced by the first run that writes there.
//
// It is exported because the staged writes here are not the only publish: the
// facade's rename path has to answer the same way, or whether the link
// survives would depend on which filesystem the staging landed on.
//
// It also keeps the staging beside the target rather than beside the link, so
// the rename stays on one filesystem, which is the whole point of the link in
// that case.
//
// A dangling link resolves to nothing, so the path is left alone and the
// rename replaces the link itself: there is no target to write through, and
// that is the only answer the filesystem offers.
func ResolveLink(finalPath string) string {
	fi, err := os.Lstat(finalPath)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return finalPath
	}
	target, err := filepath.EvalSymlinks(finalPath)
	if err != nil {
		return finalPath // dangling, or a loop: replace the link
	}
	dir := filepath.Dir(finalPath)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return target
	}
	rel, err := filepath.Rel(realDir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return target
	}
	return filepath.Join(dir, rel)
}

// chmodUmask changes a staged file from os.CreateTemp's private mode to the mode
// selected by the process umask for a regular output file.
func chmodUmask(path string) error {
	return os.Chmod(path, 0o666&^currentUmask())
}

// Commit flushes and closes the temp, then atomically renames it to the final
// path, replacing an existing file. After a successful Commit, Discard is a
// no-op.
func (f *File) Commit() error { return f.commit(false, false) }

// CommitNew is Commit for a destination that must not already exist. It
// publishes through PublishNew, so an occupied path fails with an OutputError
// wrapping fs.ErrExist instead of overwriting.
func (f *File) CommitNew() error { return f.commit(true, false) }

// CommitNewNumbered is CommitNew for a destination that may renumber: an
// occupied path publishes onto the first free " (n)" sibling instead of
// failing. It updates the final path and returns it.
func (f *File) CommitNewNumbered() (string, error) {
	if err := f.commit(true, true); err != nil {
		return "", err
	}
	return f.finalPath, nil
}

func (f *File) commit(exclusive, numbered bool) error {
	if f.committed {
		return nil
	}
	if err := f.File.Sync(); err != nil {
		_ = f.close()
		return WrapOutput("sync", retargetPathError(f.finalPath, err))
	}
	if err := f.close(); err != nil {
		return WrapOutput("close", retargetPathError(f.finalPath, err))
	}
	switch {
	case numbered:
		published, err := PublishNewNumbered(f.tmpPath, f.finalPath)
		if err != nil {
			return err
		}
		f.finalPath = published
	case exclusive:
		if err := PublishNew(f.tmpPath, f.finalPath); err != nil {
			return err
		}
	default:
		PreserveReplacedMode(f.tmpPath, f.finalPath)
		if err := os.Rename(f.tmpPath, f.finalPath); err != nil {
			return WrapOutput("rename", retargetPathError(f.finalPath, err))
		}
	}
	f.committed = true
	return nil
}

// Discard closes and removes the temp file. It is safe to call multiple times.
//
// After a successful Commit the temp is already gone, so this is a no-op, with
// one exception worth the extra syscall: CommitNew links and then unlinks the
// staged file, and if that unlink lost a race (an antivirus scanner or indexer
// holding the handle) the staged copy survives. Retrying here usually clears it,
// which matters because a staged name keeps the destination's extension, so a
// later directory run would otherwise pick the leftover up as an input.
func (f *File) Discard() error {
	if !f.committed {
		_ = f.close()
	}
	if err := os.Remove(f.tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Path returns the final destination path (valid only after Commit).
func (f *File) Path() string { return f.finalPath }

// External stages output written through a path rather than through this
// package's own handle: the caller names a temp file in the destination
// directory, something else writes it, and Commit renames it into place.
// Unlike File, External does not keep the file open for writing, which is what
// makes it usable by a writer that opens the path itself. WaxTap's own
// exclusive publish is that writer (see stageExclusive): the pipeline writes
// the staged path and the publish claims the destination.
//
// Reserve a name with NewExternal, hand Path to the writer, then call Commit to
// publish or Discard to remove the temp.
type External struct {
	finalPath string
	tmpPath   string
	committed bool
}

// NewExternal reserves a temp path next to finalPath for a writer that opens
// the path itself.
//
// The temp name carries a container extension because a writer can infer the
// output container from it. By default the extension comes from finalPath. A
// non-empty ext, with or without a leading dot, overrides the staged extension
// without changing the path used by Commit.
func NewExternal(finalPath, ext string) (*External, error) {
	finalPath = ResolveLink(finalPath)
	dir := filepath.Dir(finalPath)
	base := filepath.Base(finalPath)
	switch {
	case ext == "":
		ext = filepath.Ext(base) // includes the dot, or "" when there is none
	case !strings.HasPrefix(ext, "."):
		ext = "." + ext
	}
	pattern := base + ".*" + ext
	if ext == "" {
		pattern = base + "-*"
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, WrapOutput("create", retargetPathError(finalPath, err))
	}
	name := f.Name()
	_ = f.Close() // the external process reopens and overwrites this path
	// Widen before the external writer truncates: an O_TRUNC reopen keeps the
	// existing mode, so the published output honors the umask.
	if err := chmodUmask(name); err != nil {
		_ = os.Remove(name)
		return nil, WrapOutput("chmod", retargetPathError(finalPath, err))
	}
	return &External{finalPath: finalPath, tmpPath: name}, nil
}

// Path returns the temp path reserved for the external writer.
func (e *External) Path() string { return e.tmpPath }

// Commit atomically renames the temp path to the final path, replacing an
// existing file. After a successful Commit, Discard is a no-op.
func (e *External) Commit() error { return e.commit(false, false) }

// CommitNew is Commit for a destination that must not already exist. It
// publishes through PublishNew, so an occupied path fails with an OutputError
// wrapping fs.ErrExist instead of overwriting.
func (e *External) CommitNew() error { return e.commit(true, false) }

// CommitNewNumbered is CommitNew for a destination that may renumber: an
// occupied path publishes onto the first free " (n)" sibling instead of
// failing. It updates the final path and returns it.
func (e *External) CommitNewNumbered() (string, error) {
	if err := e.commit(true, true); err != nil {
		return "", err
	}
	return e.finalPath, nil
}

func (e *External) commit(exclusive, numbered bool) error {
	if e.committed {
		return nil
	}
	switch {
	case numbered:
		published, err := PublishNewNumbered(e.tmpPath, e.finalPath)
		if err != nil {
			return err
		}
		e.finalPath = published
	case exclusive:
		if err := PublishNew(e.tmpPath, e.finalPath); err != nil {
			return err
		}
	default:
		PreserveReplacedMode(e.tmpPath, e.finalPath)
		if err := os.Rename(e.tmpPath, e.finalPath); err != nil {
			return WrapOutput("rename", retargetPathError(e.finalPath, err))
		}
	}
	e.committed = true
	return nil
}

// Discard removes the temp path. It is safe to call multiple times, and after a
// successful Commit there is normally nothing left to remove; see File.Discard
// for the one case where there is.
func (e *External) Discard() error {
	if err := os.Remove(e.tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Final returns the destination path (valid after Commit).
func (e *External) Final() string { return e.finalPath }

// Scratch creates an unnamed temporary file in dir (or the OS temp dir if dir
// is "") and returns it with a cleanup func that closes and removes it. Use it
// for staging input that has no final destination, such as a downloaded source
// staged for a local probe. The cleanup is idempotent.
func Scratch(dir, pattern string) (f *os.File, cleanup func() error, err error) {
	if pattern == "" {
		pattern = "waxtap-*.tmp"
	}
	f, err = os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, nil, err
	}
	name := f.Name()
	cleanup = func() error {
		_ = f.Close()
		if rmErr := os.Remove(name); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return rmErr
		}
		return nil
	}
	return f, cleanup, nil
}
