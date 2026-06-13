// Command waxtap provides the WaxTap CLI for YouTube audio downloads and local
// audio processing.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// A first interrupt cancels in-flight work. A second uses the default signal
	// behavior and exits immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := newRootCmd()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return
	}
	// Some commands write their own JSON failure document. Keep the wrapped exit
	// code, but do not write another document.
	if _, rendered := errors.AsType[*alreadyRenderedError](err); rendered {
		os.Exit(exitCodeFor(err))
	}
	// Cobra does not type unknown-command errors, so classify them before rendering.
	err = normalizeExecuteError(err)
	// JSON errors go to stdout; human-readable errors go to stderr.
	out := os.Stderr
	if rootFlagsValue.json {
		out = os.Stdout
	}
	renderError(out, rootFlagsValue.json, err)
	os.Exit(exitCodeFor(err))
}
