package resolver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/colespringer/waxtap/waxerr"
)

// compileSource compiles src in non-strict mode (so bare global assignments like
// "f=function(){}" define a callable global), matching how cipher snippets run.
func compileSource(t *testing.T, name, src string) (*goja.Program, error) {
	t.Helper()
	return goja.Compile(name, src, false)
}

func TestRunTransform_OK(t *testing.T) {
	prog, err := compileSource(t, "t", `f=function(a){return a+"!"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := runTransform(context.Background(), prog, "f", "x", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != "x!" {
		t.Errorf("got %q, want x!", got)
	}
}

func TestRunTransform_NotCallable(t *testing.T) {
	prog, err := compileSource(t, "t", `f=42`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runTransform(context.Background(), prog, "f", "x", time.Second)
	if !errors.Is(err, waxerr.ErrCipherSolve) {
		t.Fatalf("err = %v, want ErrCipherSolve", err)
	}
}

// TestRunTransform_Timeout checks that a runaway transform is interrupted by the
// configured timeout.
func TestRunTransform_Timeout(t *testing.T) {
	prog, err := compileSource(t, "loop", `f=function(a){for(;;){}return a}`)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = runTransform(context.Background(), prog, "f", "x", 50*time.Millisecond)
	if !errors.Is(err, waxerr.ErrCipherSolve) {
		t.Fatalf("err = %v, want ErrCipherSolve (timeout)", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout did not fire promptly: %v", elapsed)
	}
}

// TestRunTransform_Cancel checks that caller cancellation interrupts execution
// and is reported as the context error.
func TestRunTransform_Cancel(t *testing.T) {
	prog, err := compileSource(t, "loop", `f=function(a){for(;;){}return a}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the AfterFunc fires immediately

	_, err = runTransform(ctx, prog, "f", "x", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, waxerr.ErrCipherSolve) {
		t.Fatalf("caller cancel misreported as cipher failure: %v", err)
	}
}
