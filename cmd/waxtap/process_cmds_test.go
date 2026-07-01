package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
)

// TestProcessSourceCheckedBeforeCollision verifies that a missing input is
// reported before an existing output path is considered.
func TestProcessSourceCheckedBeforeCollision(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.wav")
	existing := filepath.Join(dir, "existing.flac")
	if err := os.WriteFile(existing, []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"transcode", []string{"transcode", missing, "-f", "flac", "-o", existing}},
		{"cut", []string{"cut", missing, "--cut-range", "0-1", "-f", "flac", "-o", existing}},
		{"normalize", []string{"normalize", missing, "-f", "flac", "-o", existing}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			root.SetArgs(tc.args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			err := root.Execute()
			if err == nil {
				t.Fatal("expected an error for a missing source")
			}
			msg := err.Error()
			if !strings.Contains(msg, "no such file") {
				t.Errorf("error = %q, want it to report the missing source", msg)
			}
			if strings.Contains(msg, "already exists") {
				t.Errorf("error = %q, the existing output masked the missing source", msg)
			}
		})
	}
}

func TestDispatchProcessMangledPath(t *testing.T) {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	env := &appEnv{
		client: client,
		cfg:    &appConfig{},
		out:    io.Discard,
		errOut: io.Discard,
		log:    slog.New(slog.DiscardHandler),
	}

	// A non-existent path that is neither a YouTube URL nor an 11-character ID must be
	// reported as a missing file (usage, exit 2), not "invalid characters in
	// video ID". This returns before any network work.
	_, derr := dispatchProcess(context.Background(), env, "no such file.mp3",
		waxtap.BestAudio(), waxtap.MinimizeLoss(),
		waxtap.ProcessSpec{Output: waxtap.ToFile("out.flac")}, false)
	if _, ok := errors.AsType[*usageError](derr); !ok {
		t.Fatalf("err = %v (%T), want a usageError", derr, derr)
	}
	if !strings.Contains(derr.Error(), "no such file") {
		t.Errorf("message = %q, want it to mention the missing file", derr)
	}
}

func TestDispatchProcessIDLikeFilename(t *testing.T) {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	env := &appEnv{
		client: client,
		cfg:    &appConfig{},
		out:    io.Discard,
		errOut: io.Discard,
		log:    slog.New(slog.DiscardHandler),
	}

	// A missing path whose stem is exactly an 11-character ID, matching the
	// --output-template shape, should stay a missing-file error. The same rule
	// applies when a separator or drive prefix appears before the ID.
	for _, source := range []string{
		"testVideo01.flac",
		"/tmp/x/testVideo01",
		"wrong name testVideo01",
		"D:testVideo01",
	} {
		t.Run(source, func(t *testing.T) {
			_, derr := dispatchProcess(context.Background(), env, source,
				waxtap.BestAudio(), waxtap.MinimizeLoss(),
				waxtap.ProcessSpec{Output: waxtap.ToFile("out.flac")}, false)
			if _, ok := errors.AsType[*usageError](derr); !ok {
				t.Fatalf("err = %v (%T), want a usageError", derr, derr)
			}
			if !strings.Contains(derr.Error(), "no such file") {
				t.Errorf("message = %q, want it to mention the missing file", derr)
			}
		})
	}
}

// runProcessCmd executes a process subcommand through the root command with
// discarded output and returns the error.
func runProcessCmd(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

// TestRejectStdoutOutput verifies that `-o -` and positional `-` are rejected on
// process commands before format inference.
func TestRejectStdoutOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	if err := os.WriteFile(in, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"transcode -o - no format", []string{"transcode", in, "-o", "-"}},
		{"transcode -o - with format", []string{"transcode", in, "-f", "wav", "-o", "-"}},
		{"transcode positional -", []string{"transcode", in, "-"}},
		{"normalize -o - no format", []string{"normalize", in, "-o", "-"}},
		{"normalize -o - with format", []string{"normalize", in, "-f", "flac", "-o", "-"}},
		{"cut -o - no format", []string{"cut", in, "--cut-range", "0-1", "-o", "-"}},
		{"cut -o - with format", []string{"cut", in, "--cut-range", "0-1", "-f", "flac", "-o", "-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runProcessCmd(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), "stdout streaming") {
				t.Errorf("%v = %v, want the stdout-streaming rejection", tc.args, err)
			}
		})
	}
}

// TestChannelURLErrorPrecedence verifies that channel URLs fail as channel URLs,
// even when output-format validation would also fail later.
func TestChannelURLErrorPrecedence(t *testing.T) {
	const channel = "https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv"
	cases := []struct {
		name string
		args []string
	}{
		{"transcode", []string{"transcode", channel}},
		{"normalize", []string{"normalize", channel}},
		{"cut with bitrate", []string{"cut", channel, "--cut-range", "0-1", "--bitrate", "128000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runProcessCmd(t, tc.args...)
			if !errors.Is(err, waxtap.ErrIsChannel) {
				t.Errorf("%v = %v, want ErrIsChannel", tc.args, err)
			}
			if err != nil && strings.Contains(err.Error(), "--format") {
				t.Errorf("%v = %v, the channel error must precede the format/bitrate error", tc.args, err)
			}
		})
	}
}

// TestCutInfersFormatFromExtension verifies that re-encoding cuts can infer their
// format from a recognized output extension.
func TestCutInfersFormatFromExtension(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac") // 1s sine

	// A crossfade with a recognized -o extension infers flac and runs to success.
	if err := runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--crossfade", "100ms", "-o", filepath.Join(dir, "out.flac")); err != nil {
		t.Errorf("crossfade cut into .flac should infer flac and run: %v", err)
	}
	// Accurate mode with -o out.mp3 infers mp3, and the inference runs before the
	// "--bitrate requires --format" check so --bitrate pairs with the inferred format.
	if err := runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--cut-mode", "accurate", "--bitrate", "128000", "-o", filepath.Join(dir, "out.mp3")); err != nil {
		t.Errorf("accurate cut into .mp3 should infer mp3 and run: %v", err)
	}
	// An unrecognized extension (.mka) is only a container hint: --format is still required.
	err := runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--crossfade", "100ms", "-o", filepath.Join(dir, "out.mka"))
	if err == nil || !strings.Contains(err.Error(), "--format") {
		t.Errorf("crossfade cut into .mka should still demand --format, got %v", err)
	}
	// The copy/remux pseudo-formats are not real container extensions, so
	// `-o out.copy` falls through to the "pass --format" error like any
	// unrecognized extension.
	err = runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--crossfade", "100ms", "-o", filepath.Join(dir, "out.copy"))
	if err == nil || !strings.Contains(err.Error(), "--format") {
		t.Errorf("crossfade cut into .copy should still demand --format, got %v", err)
	}
}

// TestCutRejectsDirectoryOutput verifies that cut reports directory outputs at
// the CLI boundary instead of surfacing a downstream IO error.
func TestCutRejectsDirectoryOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	if err := os.WriteFile(in, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runProcessCmd(t, "cut", in, "--cut-range", "0-1", "-o", dir)
	if err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Errorf("cut into a directory output = %v, want the clean directory message", err)
	}
}

// roundTripErr is a transport that fails every request, so a download attempt
// returns fast without network access.
type roundTripErr struct{}

func (roundTripErr) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network")
}

// TestDispatchProcessNotesDroppedPlaylist verifies that process commands note an
// ignored playlist before starting the download.
func TestDispatchProcessNotesDroppedPlaylist(t *testing.T) {
	client, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: roundTripErr{}}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	env := &appEnv{
		client: client,
		cfg:    &appConfig{},
		out:    io.Discard,
		errOut: &buf,
		log:    slog.New(slog.DiscardHandler),
	}
	// The download fails on the injected transport; the note is emitted before it.
	_, _ = dispatchProcess(context.Background(), env,
		"https://www.youtube.com/watch?v=dummyVideo0&list=PLtest123456789",
		waxtap.BestAudio(), waxtap.MinimizeLoss(),
		waxtap.ProcessSpec{Output: waxtap.ToFile(filepath.Join(t.TempDir(), "out.flac"))}, false)
	if !strings.Contains(buf.String(), "ignoring playlist PLtest123456789") {
		t.Errorf("errOut = %q, want a dropped-playlist note", buf.String())
	}
}
