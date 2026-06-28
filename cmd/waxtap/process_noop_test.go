package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runTranscode executes the transcode command through the root command (so the
// persistent --json/--quiet flags are wired up) with separated stdout/stderr.
func runTranscode(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"transcode"}, args...))
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// ffprobeCodec returns the first audio stream's codec name.
func ffprobeCodec(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-hide_banner", "-loglevel", "error",
		"-select_streams", "a:0", "-show_entries", "stream=codec_name",
		"-of", "default=nokey=1:noprint_wrappers=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// pcmMD5 decodes a file's audio to raw PCM and hashes it, so a stream copy and a
// re-encode of the same source are distinguishable (a lossy re-encode changes the
// samples; a copy does not).
func pcmMD5(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-map", "0:a:0", "-f", "md5", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg md5 %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// transcodedFalse reports whether a --json transcode result says no encoding was
// performed.
func transcodedFalse(t *testing.T, stdout string) bool {
	t.Helper()
	var got struct {
		Transcoded bool `json:"transcoded"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal result JSON: %v\n%s", err, stdout)
	}
	return !got.Transcoded
}

func TestTranscodeSameFormatRemux(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")
	out := filepath.Join(dir, "out.flac")

	stdout, _, err := runTranscode(t, in, "--format", "flac", "-o", out, "--json")
	if err != nil {
		t.Fatalf("same-format transcode: %v", err)
	}
	if !transcodedFalse(t, stdout) {
		t.Errorf("same-format flac should not re-encode (want transcoded:false):\n%s", stdout)
	}
	if c := ffprobeCodec(t, out); c != "flac" {
		t.Errorf("output codec = %q, want flac", c)
	}
	if a, b := pcmMD5(t, in), pcmMD5(t, out); a != b {
		t.Errorf("samples changed: in=%s out=%s", a, b)
	}

	// Human mode prints the copy note on stderr.
	_, stderr, err := runTranscode(t, in, "--format", "flac", "-o", filepath.Join(dir, "out2.flac"))
	if err != nil {
		t.Fatalf("same-format transcode (human): %v", err)
	}
	if !strings.Contains(stderr, "copied without re-encoding") {
		t.Errorf("missing no-op note on stderr:\n%s", stderr)
	}
}

func TestTranscodeContainerChangeRemux(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	synthAudio(t, in, "libopus") // opus in a WebM container
	out := filepath.Join(dir, "out.opus")

	stdout, _, err := runTranscode(t, in, "--format", "opus", "-o", out, "--json")
	if err != nil {
		t.Fatalf("container-change remux: %v", err)
	}
	// transcoded:false proves the opus stream was copied, not re-encoded; ffprobe
	// confirms the .opus output is a valid opus file (not mislabeled). The decoded
	// PCM is not compared: opus preskip handling differs across containers even for
	// a pure stream copy, so it is not a reliable cross-container equality check.
	if !transcodedFalse(t, stdout) {
		t.Errorf("opus source -> .opus should remux, not re-encode:\n%s", stdout)
	}
	if c := ffprobeCodec(t, out); c != "opus" {
		t.Errorf("output codec = %q, want opus", c)
	}
}

func TestTranscodeSameFormatNonInferableExtReEncodes(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.m4a")
	synthAudio(t, in, "alac") // ALAC source in an MP4 container

	// .alac names a codec, not a container ffmpeg can infer. A stream copy has no
	// forced muxer, so the same-format shortcut must not run here. The normal ALAC
	// encode provides the MP4 muxer, and ALAC remains lossless.
	out := filepath.Join(dir, "out.alac")
	stdout, _, err := runTranscode(t, in, "--format", "alac", "-o", out, "--json")
	if err != nil {
		t.Fatalf("alac -> .alac: %v", err)
	}
	if transcodedFalse(t, stdout) {
		t.Errorf(".alac output must re-encode (a copy has no forced muxer), got transcoded:false:\n%s", stdout)
	}
	if c := ffprobeCodec(t, out); c != "alac" {
		t.Errorf("output codec = %q, want alac", c)
	}

	// The same ALAC source into an inferable .m4a does take the no-op remux.
	out2 := filepath.Join(dir, "out2.m4a")
	stdout2, _, err := runTranscode(t, in, "--format", "alac", "-o", out2, "--json")
	if err != nil {
		t.Fatalf("alac -> .m4a: %v", err)
	}
	if !transcodedFalse(t, stdout2) {
		t.Errorf("alac -> inferable .m4a should remux (transcoded:false):\n%s", stdout2)
	}
}

func TestTranscodeProbeFailureNotCopiedThrough(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	// A file with an audio extension but unreadable content: ProbeCodec fails, so
	// the no-op shortcut must fall through to the encode rather than copying
	// unreadable bytes as a successful same-codec output.
	in := filepath.Join(dir, "garbage.flac")
	if err := os.WriteFile(in, []byte("not real flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.flac")

	_, _, err := runTranscode(t, in, "--format", "flac", "-o", out)
	if err == nil {
		t.Fatal("garbage input was accepted as a same-format copy; want an encode error")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a failed encode must not leave an output file")
	}
}

func TestTranscodeQuietPrintsOnlyPath(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")
	out := filepath.Join(dir, "out.mp3")

	stdout, _, err := runTranscode(t, in, "--format", "mp3", "-o", out, "--quiet")
	if err != nil {
		t.Fatalf("quiet transcode: %v", err)
	}
	if got := strings.TrimRight(stdout, "\n"); got != out {
		t.Errorf("quiet stdout = %q, want exactly the output path %q", stdout, out)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("quiet stdout should be exactly one line, got %q", stdout)
	}

	// --quiet --json still prints the full JSON document to stdout.
	out2 := filepath.Join(dir, "out2.mp3")
	jstdout, _, err := runTranscode(t, in, "--format", "mp3", "-o", out2, "--quiet", "--json")
	if err != nil {
		t.Fatalf("quiet json transcode: %v", err)
	}
	var got struct {
		OutputPath string `json:"outputPath"`
		Transcoded bool   `json:"transcoded"`
	}
	if err := json.Unmarshal([]byte(jstdout), &got); err != nil {
		t.Fatalf("quiet --json stdout is not the full JSON document: %v\n%s", err, jstdout)
	}
	if got.OutputPath != out2 || !got.Transcoded {
		t.Errorf("quiet --json result = %+v, want full document with outputPath=%q transcoded=true", got, out2)
	}
}

func TestTranscodeForceBitrateDownmixBypassRemux(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp3")
	synthAudio(t, in, "libmp3lame")

	cases := []struct {
		name  string
		extra []string
	}{
		{"force", []string{"--force"}},
		{"bitrate", []string{"--bitrate", "128000"}},
		{"downmix", []string{"--downmix", "--channels", "mono"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(dir, c.name+".mp3")
			args := append([]string{in, "--format", "mp3", "-o", out, "--json"}, c.extra...)
			stdout, _, err := runTranscode(t, args...)
			if err != nil {
				t.Fatalf("%s transcode: %v", c.name, err)
			}
			if transcodedFalse(t, stdout) {
				t.Errorf("%s should re-encode (want transcoded:true):\n%s", c.name, stdout)
			}
		})
	}
}
