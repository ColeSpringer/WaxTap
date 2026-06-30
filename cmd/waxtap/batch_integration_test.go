package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
}

// synthAudio writes a one-second sine fixture encoded with the given ffmpeg codec.
func synthAudio(t *testing.T, path, encoder string) {
	t.Helper()
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=" + strconv.Itoa(1),
		"-ac", "2", "-c:a", encoder, path,
	}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("synth %s: %v: %s", path, err, b)
	}
}

// TestBatchTranscodeCommandIntegration covers re-encoding, unchanged copies, and
// ignored non-audio files.
func TestBatchTranscodeCommandIntegration(t *testing.T) {
	requireFFmpeg(t)
	root := t.TempDir()
	synthAudio(t, filepath.Join(root, "a.flac"), "flac")
	synthAudio(t, filepath.Join(root, "b.mp3"), "libmp3lame") // already mp3 -> copy-through
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(root, "out")

	cmd := newTranscodeCmd()
	cmd.SetArgs([]string{root, "--format", "mp3", "--dir", outDir})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("transcode dir: %v\n%s", err, buf.String())
	}
	for _, name := range []string{"a.mp3", "b.mp3"} {
		if fi, err := os.Stat(filepath.Join(outDir, name)); err != nil || fi.Size() == 0 {
			t.Errorf("missing or empty output %s (err=%v)\n%s", name, err, buf.String())
		}
	}
	// b.mp3 was copied through unchanged; a.flac was re-encoded.
	if got := buf.String(); !bytes.Contains([]byte(got), []byte("copied:")) {
		t.Errorf("output did not report a copy-through:\n%s", got)
	}
	// Directory processing reports per-item progress.
	if got := buf.String(); !bytes.Contains([]byte(got), []byte("[2/2]")) {
		t.Errorf("expected streamed [N/total] progress on stderr:\n%s", got)
	}
}

func TestBatchForceReencodesNoOp(t *testing.T) {
	requireFFmpeg(t)
	root := t.TempDir()
	synthAudio(t, filepath.Join(root, "b.mp3"), "libmp3lame")
	outDir := filepath.Join(root, "out")

	cmd := newTranscodeCmd()
	cmd.SetArgs([]string{root, "--format", "mp3", "--dir", outDir, "--force"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("forced transcode dir: %v\n%s", err, buf.String())
	}
	if got := buf.String(); bytes.Contains([]byte(got), []byte("copied:")) {
		t.Errorf("--force should re-encode, not copy:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(outDir, "b.mp3")); err != nil {
		t.Errorf("forced output missing: %v", err)
	}
}

func TestBatchNormalizeMeasureIntegration(t *testing.T) {
	requireFFmpeg(t)
	root := t.TempDir()
	synthAudio(t, filepath.Join(root, "a.flac"), "flac")
	synthAudio(t, filepath.Join(root, "b.wav"), "pcm_s16le")

	cmd := newNormalizeCmd()
	cmd.SetArgs([]string{root, "--measure-loudness"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("normalize dir: %v\n%s", err, buf.String())
	}
	got := buf.String()
	// The loudness table header and both files should appear, with no output files.
	for _, want := range []string{"LUFS", "a.flac", "b.wav", "2 files: 2 measured"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("measure output missing %q:\n%s", want, got)
		}
	}
}

func TestBatchNormalizeMeasureRejectsDir(t *testing.T) {
	root := t.TempDir()

	cmd := newNormalizeCmd()
	cmd.SetArgs([]string{root, "--measure-loudness", "--dir", filepath.Join(root, "normalized")})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("normalize --measure-loudness --dir = %v (%T), want usageError", err, err)
	}
}

func TestTranscodeDirRejectsOutputFlag(t *testing.T) {
	root := t.TempDir()
	cmd := newTranscodeCmd()
	cmd.SetArgs([]string{root, "--format", "mp3", "-o", "out.mp3"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("transcode <dir> -o = %v (%T), want usageError", err, err)
	}
}

func TestTranscodeEmptyDirReportsNoAudio(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newTranscodeCmd()
	cmd.SetArgs([]string{root, "--format", "mp3", "--dir", filepath.Join(root, "out")})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("empty dir should exit 0, got %v\n%s", err, buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("no recognized audio files")) {
		t.Errorf("expected a no-audio-files note:\n%s", buf.String())
	}
}

func TestCutRejectsDirectory(t *testing.T) {
	root := t.TempDir()
	cmd := newCutCmd()
	cmd.SetArgs([]string{root, "--cut-range", "0-1"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("cut <dir> = %v (%T), want usageError", err, err)
	}
}
