package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

// TestResolveStdoutSink covers -o -: it records the stdout writer and rejects the
// path-only flags that a writer sink cannot honor.
func TestResolveStdoutSink(t *testing.T) {
	newDF := func() (*downloadFlags, *cobra.Command) {
		df := &downloadFlags{}
		cmd := &cobra.Command{Use: "download"}
		bindDownloadFlags(cmd, df)
		return df, cmd
	}

	t.Run("-o - sets streamW", func(t *testing.T) {
		df, cmd := newDF()
		mustSet(t, cmd, "out", "-")
		if err := df.resolve(cmd, testResolveEnv()); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if df.streamW == nil {
			t.Fatal("streamW = nil, want the stdout writer")
		}
	})

	t.Run("-o - rejects --write-info-json", func(t *testing.T) {
		df, cmd := newDF()
		mustSet(t, cmd, "out", "-")
		mustSet(t, cmd, "write-info-json", "true")
		err := df.resolve(cmd, testResolveEnv())
		if !isUsageError(err) || !strings.Contains(err.Error(), "write-info-json") {
			t.Fatalf("resolve err = %v, want a --write-info-json usage error", err)
		}
	})

	t.Run("-o - rejects explicit --collision", func(t *testing.T) {
		df, cmd := newDF()
		mustSet(t, cmd, "out", "-")
		mustSet(t, cmd, "collision", "skip")
		err := df.resolve(cmd, testResolveEnv())
		if !isUsageError(err) || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("resolve err = %v, want a --collision usage error", err)
		}
	})

	t.Run("a real path leaves streamW nil", func(t *testing.T) {
		df, cmd := newDF()
		mustSet(t, cmd, "out", "track.opus")
		if err := df.resolve(cmd, testResolveEnv()); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if df.streamW != nil {
			t.Errorf("streamW = %v, want nil for a file sink", df.streamW)
		}
	})
}

// TestStreamPreStreamErrorStaysOffStdout is the single-seam guard: when streaming
// to stdout, a failure before any audio is written (here an invalid target, which
// fails in resolveItem) must render to stderr and come back alreadyRendered so main
// never writes a JSON error document to the audio sink.
func TestStreamPreStreamErrorStaysOffStdout(t *testing.T) {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	env := &appEnv{client: client, cfg: &appConfig{json: true}, out: &out, errOut: &errOut}
	df := &downloadFlags{streamW: &out} // streaming enabled

	rerr := runSingleDownload(context.Background(), env, df, "not a valid target")
	if _, already := errors.AsType[*alreadyRenderedError](rerr); !already {
		t.Fatalf("err = %v, want alreadyRendered so main writes nothing to stdout", rerr)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want empty (audio sink stays pure on a pre-stream error)", out.String())
	}
	if errOut.Len() == 0 {
		t.Error("want the error rendered to stderr")
	}
}
