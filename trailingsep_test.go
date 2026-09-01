package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// An output path ending in a separator names a directory, not a file, and the
// library must refuse it before anything is created. It used to reach
// MkdirAll(filepath.Dir(p)), which for "d/a.wav/" is "d/a.wav" - so WaxTap made
// a directory named after the requested file, then spent the rest of the run
// asking collision questions about it.
func TestProcessRejectsTrailingSeparatorOutput(t *testing.T) {
	dir := t.TempDir()
	in := wavFrom(t, dir, "in.wav", mediatest.SineWAV(1, 2))
	out := filepath.Join(dir, "out.flac") + string(os.PathSeparator)

	_, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	})
	if err == nil {
		t.Fatal("a trailing-separator output was accepted")
	}
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("err = %v, want ErrIncompatibleSpec (exit 2), not an output failure", err)
	}
	if !strings.Contains(err.Error(), "path separator") {
		t.Errorf("err = %q, want it to name the trailing separator", err)
	}
	// Nothing may be left behind, least of all a directory wearing the output's
	// name.
	if _, serr := os.Stat(filepath.Join(dir, "out.flac")); serr == nil {
		t.Error("the rejected path was created anyway")
	}

	// The skip pre-check must not beat the rejection: a directory already at the
	// bad path stats as existing, and answering "already done" to a request that
	// never named a file reports work nobody did.
	if err := os.Mkdir(filepath.Join(dir, "have.flac"), 0o777); err != nil {
		t.Fatal(err)
	}
	res, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:       ToFile(filepath.Join(dir, "have.flac") + string(os.PathSeparator)),
			Transcode:    &TranscodeSpec{Format: FormatFLAC},
			SkipIfExists: true,
		},
	})
	if err == nil {
		t.Fatalf("skip-if-exists reported %+v for a trailing-separator path instead of rejecting it", res)
	}
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("err = %v, want ErrIncompatibleSpec", err)
	}
}

// ProcessAlbum funnels through the same guard, one track at a time.
func TestProcessAlbumRejectsTrailingSeparatorOutput(t *testing.T) {
	dir := t.TempDir()
	in := wavFrom(t, dir, "in.wav", mediatest.SineWAV(1, 2))

	_, err := newOfflineClient(t).ProcessAlbum(context.Background(),
		[]AlbumTrack{{Input: in, Output: filepath.Join(dir, "t1.flac") + string(os.PathSeparator)}},
		-14, TranscodeSpec{Format: FormatFLAC})
	if err == nil {
		t.Fatal("a trailing-separator album output was accepted")
	}
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("err = %v, want ErrIncompatibleSpec", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "t1.flac")); serr == nil {
		t.Error("the rejected path was created anyway")
	}
}

// An ordinary path is untouched by the guard, including a bare filename whose
// parent is "." and a nested one the guard must still create.
func TestEnsureParentDirAcceptsOrdinaryPaths(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "out.flac")
	if err := ensureParentDir(nested); err != nil {
		t.Fatalf("nested path: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(nested)); err != nil {
		t.Errorf("parent not created: %v", err)
	}
	if err := ensureParentDir("out.flac"); err != nil {
		t.Errorf("bare filename: %v", err)
	}
}
