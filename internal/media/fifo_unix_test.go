//go:build unix

package media

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// A FIFO with no writer blocks an ordinary open forever. The source is opened
// non-blocking so the engine's regular-file check can refuse it by name, which
// is both what the caller needs to hear and the only way the call returns at
// all.
func TestProbeRefusesAFifoWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.wav")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	r := NewRunner(RunnerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := r.Probe(ctx, fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, waxerr.ErrUnsupportedInput) {
			t.Fatalf("Probe(fifo) = %v, want ErrUnsupportedInput", err)
		}
		if !strings.Contains(err.Error(), "named pipe") {
			t.Errorf("err = %v, want it to name the pipe", err)
		}
	case <-ctx.Done():
		t.Fatal("Probe blocked on a FIFO with no writer; the open is not non-blocking")
	}
}
