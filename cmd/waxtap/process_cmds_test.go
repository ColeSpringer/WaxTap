package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
)

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
