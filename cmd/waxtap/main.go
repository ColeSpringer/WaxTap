// Command waxtap provides the WaxTap CLI for YouTube audio downloads and local
// audio processing.
package main

import (
	"context"
	"errors"
	"io"
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
	// Cobra does not type unknown-command errors, so classify them first: it
	// matches on the message prefix, which a cancellation join would push out of
	// place.
	err = normalizeExecuteError(err)
	// A failure that followed the signal is a cancellation, whatever it reports.
	// stop() is deferred and cannot have run, so ctx.Err() is set only by a signal.
	err = finalError(ctx, err)
	os.Exit(report(os.Stdout, os.Stderr, os.Args[1:], err))
}

// report renders a terminal error and returns the process exit code. It lives
// apart from main so tests can reach it: they call root.Execute() and never
// main(), and the flag-parse case is only observable here.
func report(stdout, stderr io.Writer, args []string, err error) int {
	// Some commands write their own JSON failure document. Keep the wrapped exit
	// code, but do not write another document.
	if _, rendered := errors.AsType[*alreadyRenderedError](err); rendered {
		return exitCodeFor(err)
	}
	// rootFlagsValue holds the parsed flags, which is the answer whenever parsing
	// got that far. Only a failure that preceded it has to re-read the command
	// line, and only those are marked; see usageError.
	jsonMode := rootFlagsValue.json
	if ue, ok := errors.AsType[*usageError](err); ok && ue.flagsUnparsed {
		jsonMode = jsonMode || jsonRequested(args)
	}
	// JSON errors go to stdout; human-readable errors go to stderr.
	out := stderr
	if jsonMode {
		out = stdout
	}
	renderError(out, jsonMode, err, args)
	return exitCodeFor(err)
}
