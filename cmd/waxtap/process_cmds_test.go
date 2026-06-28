package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
