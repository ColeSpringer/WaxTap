// Command waxtap provides the WaxTap CLI for YouTube audio downloads and local
// audio processing.
package main

import (
	"context"
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
	if err := root.ExecuteContext(ctx); err != nil {
		renderError(os.Stderr, rootFlagsValue.json, err)
		os.Exit(exitCodeFor(err))
	}
}
