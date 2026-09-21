//go:build unix

package waxtap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// --collision overwrite replaces the file's content, which is what it promises.
// It should not also reset the permissions the user set on it. The behavior is
// unix-only (Windows maps chmod to the read-only attribute, which os.Rename
// then refuses to replace), so this file carries the matching build tag rather
// than a GOOS check that would leave wasip1 or plan9 running it against the
// no-op stub.
func TestOverwritePreservesReadOnlyMode(t *testing.T) {
	dir := t.TempDir()
	in := writeWAV(t, dir, "in.wav")
	out := filepath.Join(dir, "out.flac")
	if err := os.WriteFile(out, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(out, 0o444); err != nil {
		t.Fatal(err)
	}

	res, err := mustClient(t).Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC}, Output: ToFile(out)},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	fi, err := os.Stat(res.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o444 {
		t.Errorf("mode after overwrite = %v, want 0444 preserved", got)
	}
	if fi.Size() < 3 {
		t.Errorf("size = %d; the content should have been replaced by the encode", fi.Size())
	}
}

// An output path whose last component is a symlink is written through the
// link: a user who points an output name at another disk keeps that
// redirection rather than having the first run replace it with a regular file.
func TestProcessWritesThroughASymlinkedOutput(t *testing.T) {
	dir := t.TempDir()
	in := writeWAV(t, dir, "in.wav")
	target := filepath.Join(dir, "target.flac")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "outlink.flac")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := mustClient(t).Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC}, Output: ToFile(link)},
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a regular file; the write must go through it")
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 4 || string(b[:4]) != "fLaC" {
		t.Errorf("target holds %d bytes starting %q, want the written FLAC", len(b), b[:min(4, len(b))])
	}
}
