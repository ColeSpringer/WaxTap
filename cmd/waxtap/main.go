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
	// A first SIGINT or SIGTERM cancels in-flight work. A second uses the
	// default signal behavior and exits immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Published so a per-item record, which classifies its own error with no
	// access to this context, can still tell an interrupt from a cancellation
	// WaxTap made itself.
	setRunSignal(ctx)

	root := newRootCmd()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return
	}
	// Cobra does not type unknown-command errors, so classify them first: it
	// matches on the message prefix, which a cancellation join would push out of
	// place.
	err = normalizeExecuteError(err, os.Args[1:])
	// A failure that followed the signal is a cancellation, whatever it reports,
	// and is marked as one: stop() is deferred and cannot have run, so ctx.Err()
	// is set only by a signal. Without the marker the renderer cannot tell this
	// from a cancellation WaxTap made itself, which is exit 1 and a defect.
	err = finalError(ctx, err)
	os.Exit(report(os.Stdout, os.Stderr, os.Args[1:], outputFlags(root).json, err))
}

// report renders a terminal error and returns the process exit code. It lives
// apart from main so tests can reach it: they call root.Execute() and never
// main(), and the flag-parse case is only observable here. jsonMode is the
// root's --json, read after Execute returns; it is the only output flag a
// terminal error consults.
func report(stdout, stderr io.Writer, args []string, jsonMode bool, err error) int {
	// Some commands write their own JSON failure document. Keep the wrapped exit
	// code, but do not write another document.
	if _, rendered := errors.AsType[*alreadyRenderedError](err); rendered {
		return exitCodeFor(err)
	}
	// The parsed flag is the answer whenever parsing got that far. Only a
	// failure that preceded it has to re-read the command line, and only those
	// are marked; see usageError.
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
