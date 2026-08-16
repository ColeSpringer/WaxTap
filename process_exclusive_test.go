package waxtap

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// writeWAV writes a short synthetic source and returns its path.
func writeWAV(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// leftovers lists everything in dir except the named files, so a test can assert
// no staging file survived.
func leftovers(t *testing.T, dir string, expected ...string) []string {
	t.Helper()
	keep := make(map[string]bool, len(expected))
	for _, e := range expected {
		keep[filepath.Base(e)] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var extra []string
	for _, e := range entries {
		if !keep[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

// TestProcessToNewFile is F6's library half, end to end through the real
// pipeline: an exclusive output publishes onto a free path and refuses an
// occupied one, without the pipeline ever holding the destination itself.
func TestProcessToNewFile(t *testing.T) {
	ctx := context.Background()
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("free path delivers and stages nothing", func(t *testing.T) {
		dir := t.TempDir()
		in := writeWAV(t, dir, "in.wav")
		out := filepath.Join(dir, "out.flac")

		res, err := c.Process(ctx, ProcessRequest{
			Input:       in,
			ProcessSpec: ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC}, Output: ToNewFile(out)},
		})
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		if res.OutputPath != out {
			t.Errorf("OutputPath = %q, want %q", res.OutputPath, out)
		}
		if res.OutputBytes <= 0 {
			t.Errorf("OutputBytes = %d, want the delivered size", res.OutputBytes)
		}
		if extra := leftovers(t, dir, in, out); len(extra) != 0 {
			t.Errorf("staging files left behind: %v", extra)
		}
	})

	t.Run("occupied path fails and leaves the existing file", func(t *testing.T) {
		dir := t.TempDir()
		in := writeWAV(t, dir, "in.wav")
		out := filepath.Join(dir, "out.flac")
		if err := os.WriteFile(out, []byte("winner"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := c.Process(ctx, ProcessRequest{
			Input:       in,
			ProcessSpec: ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC}, Output: ToNewFile(out)},
		})
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("Process onto an occupied path = %v, want fs.ErrExist", err)
		}
		if !strings.Contains(err.Error(), out) {
			t.Errorf("error = %q, want it to name the destination", err)
		}
		if got, _ := os.ReadFile(out); string(got) != "winner" {
			t.Errorf("existing file = %q, want it untouched", got)
		}
		if extra := leftovers(t, dir, in, out); len(extra) != 0 {
			t.Errorf("staging files left behind: %v", extra)
		}
	})

	// ToFile is unchanged: library callers that opt into last-writer-wins keep it.
	t.Run("ToFile still replaces", func(t *testing.T) {
		dir := t.TempDir()
		in := writeWAV(t, dir, "in.wav")
		out := filepath.Join(dir, "out.flac")
		if err := os.WriteFile(out, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := c.Process(ctx, ProcessRequest{
			Input:       in,
			ProcessSpec: ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC}, Output: ToFile(out)},
		}); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if got, _ := os.ReadFile(out); string(got) == "stale" {
			t.Error("ToFile left the existing file in place, want it replaced")
		}
	})

	// Measure-only delivers a copy of the input rather than a pipeline output, so
	// it publishes through copyFile and needs the same exclusivity.
	t.Run("measure-only copy is exclusive too", func(t *testing.T) {
		dir := t.TempDir()
		in := writeWAV(t, dir, "in.wav")
		out := filepath.Join(dir, "copy.wav")
		if err := os.WriteFile(out, []byte("winner"), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := c.Process(ctx, ProcessRequest{
			Input:       in,
			ProcessSpec: ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly}, Output: ToNewFile(out)},
		})
		if !errors.Is(err, fs.ErrExist) {
			t.Fatalf("measure-only onto an occupied path = %v, want fs.ErrExist", err)
		}
		if got, _ := os.ReadFile(out); string(got) != "winner" {
			t.Errorf("existing file = %q, want it untouched", got)
		}
		if extra := leftovers(t, dir, in, out); len(extra) != 0 {
			t.Errorf("staging files left behind: %v", extra)
		}
	})
}

// TestWarnName covers the display path warnings use. A staged delivery writes a
// temp name the user never asked for and will never see, so a warning that names
// it ("could not carry metadata into out.mp3.3810293847.mp3") points at a file
// that no longer exists by the time it is read.
func TestWarnName(t *testing.T) {
	staged := filepath.Join("out", "track.mp3.3810293847.mp3")
	dest := filepath.Join("out", "track.mp3")

	if got := warnName(dest, staged); got != "track.mp3" {
		t.Errorf("warnName(dest, staged) = %q, want the destination's name", got)
	}
	// A writer sink has no destination path, so the written file is all there is.
	if got := warnName("", staged); got != "track.mp3.3810293847.mp3" {
		t.Errorf("warnName(\"\", staged) = %q, want the written file's name", got)
	}
	// A direct write names itself either way.
	if got := warnName(dest, dest); got != "track.mp3" {
		t.Errorf("warnName(dest, dest) = %q, want %q", got, "track.mp3")
	}
}

// TestProcessToNewFileWarningsNameTheDestination is the end-to-end half: nothing
// a run reports may leak the staging name.
func TestProcessToNewFileWarningsNameTheDestination(t *testing.T) {
	dir := t.TempDir()
	in := writeWAV(t, dir, "in.wav")
	out := filepath.Join(dir, "out.flac")

	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC}, Output: ToNewFile(out)},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	// The staged name is the destination's name plus a temp infix, so any warning
	// carrying "out.flac." is naming the staging file.
	for _, w := range res.Warnings {
		if strings.Contains(w.Detail, "out.flac.") {
			t.Errorf("warning %s names the staging file: %q", w.Code, w.Detail)
		}
	}
}
