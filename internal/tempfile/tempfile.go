// Package tempfile stages output in the destination directory and publishes it
// with an atomic rename. Failed or canceled work removes the staged file, leaving
// the final path untouched.
//
// The creator owns cleanup. Defer the returned value's Discard method
// immediately after New or NewExternal; Discard is a no-op after Commit succeeds.
package tempfile

import (
	"errors"
	"os"
	"path/filepath"
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
func New(finalPath string) (*File, error) {
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

// chmodUmask changes a staged file from os.CreateTemp's private mode to the mode
// selected by the process umask for a regular output file.
func chmodUmask(path string) error {
	return os.Chmod(path, 0o666&^currentUmask())
}

// Commit flushes and closes the temp, then atomically renames it to the final
// path. After a successful Commit, Discard is a no-op.
func (f *File) Commit() error {
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
	if err := os.Rename(f.tmpPath, f.finalPath); err != nil {
		return WrapOutput("rename", retargetPathError(f.finalPath, err))
	}
	f.committed = true
	return nil
}

// Discard closes and removes the temp file. It is safe to call multiple times
// and is a no-op after a successful Commit.
func (f *File) Discard() error {
	if f.committed {
		return nil
	}
	_ = f.close()
	if err := os.Remove(f.tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Path returns the final destination path (valid only after Commit).
func (f *File) Path() string { return f.finalPath }

// External stages output written by another process, such as ffmpeg. It reserves
// a temp path in the destination directory, then renames that path into place on
// Commit. Unlike File, External does not keep the file open for writing.
//
// Reserve a name with NewExternal, pass Path to the process, then call Commit to
// publish or Discard to remove the temp.
type External struct {
	finalPath string
	tmpPath   string
	committed bool
}

// NewExternal reserves a temp path next to finalPath for an external writer.
//
// The temp name carries a container extension because tools such as ffmpeg infer
// the output container from it. By default the extension comes from finalPath.
// A non-empty ext, with or without a leading dot, overrides the staged extension
// without changing the path used by Commit.
func NewExternal(finalPath, ext string) (*External, error) {
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

// Commit atomically renames the temp path to the final path. After a successful
// Commit, Discard is a no-op.
func (e *External) Commit() error {
	if e.committed {
		return nil
	}
	if err := os.Rename(e.tmpPath, e.finalPath); err != nil {
		return WrapOutput("rename", retargetPathError(e.finalPath, err))
	}
	e.committed = true
	return nil
}

// Discard removes the temp path. It is safe to call multiple times and is a
// no-op after a successful Commit.
func (e *External) Discard() error {
	if e.committed {
		return nil
	}
	if err := os.Remove(e.tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Final returns the destination path (valid after Commit).
func (e *External) Final() string { return e.finalPath }

// Scratch creates an unnamed temporary file in dir (or the OS temp dir if dir
// is "") and returns it with a cleanup func that closes and removes it. Use it
// for staging input that has no final destination (e.g. a downloaded source
// staged for ffmpeg). The cleanup is idempotent.
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
