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
