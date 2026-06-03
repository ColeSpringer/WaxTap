// Package tempfile encodes WaxTap's temp-file ownership contract: intermediate
// data is written to a temporary file in the destination directory and only
// becomes visible at the final path via an atomic rename on success. On
// cancellation or error the temp is removed, so a partial or expired-URL
// download is never mistaken for a finished file.
//
// Ownership rule: whoever creates a temp owns cleaning it up. Callers should
// `defer f.Discard()` immediately after New (Discard is a no-op once Commit
// succeeds), giving correct cleanup on every error and context-cancel path.
package tempfile

import (
	"errors"
	"os"
	"path/filepath"
)

// File is a staged output: write to it, then Commit (atomic rename to the final
// path) or Discard (remove the temp). The temp is created in the final path's
// directory so the rename stays on one filesystem and is therefore atomic.
type File struct {
	*os.File
	finalPath string
	tmpPath   string
	committed bool
}

// New creates a staging file for eventual atomic rename to finalPath. The
// returned *File embeds *os.File, so callers write to it directly.
func New(finalPath string) (*File, error) {
	dir := filepath.Dir(finalPath)
	base := filepath.Base(finalPath)
	f, err := os.CreateTemp(dir, base+".*.part")
	if err != nil {
		return nil, err
	}
	return &File{File: f, finalPath: finalPath, tmpPath: f.Name()}, nil
}

// Commit flushes and closes the temp, then atomically renames it to the final
// path. After a successful Commit, Discard is a no-op.
func (f *File) Commit() error {
	if f.committed {
		return nil
	}
	if err := f.File.Sync(); err != nil {
		_ = f.File.Close()
		return err
	}
	if err := f.File.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.tmpPath, f.finalPath); err != nil {
		return err
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
	_ = f.File.Close()
	if err := os.Remove(f.tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Path returns the final destination path (valid only after Commit).
func (f *File) Path() string { return f.finalPath }

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
