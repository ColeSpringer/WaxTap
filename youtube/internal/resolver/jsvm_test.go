package resolver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2/waxerr"
)

// TestSolve_CompileErrPropagates checks that a body the solver cannot prepare
// surfaces its compile failure (not a generic empty result) on every solve.
func TestSolve_CompileErrPropagates(t *testing.T) {
	p := compilePlayerProgram("u", `var x = 1;`) // no player IIFE
	if p.compileErr == nil {
		t.Fatal("expected a compile error for a non-player body")
	}
	if _, err := p.decodeN(context.Background(), "12345", time.Second); !errors.Is(err, waxerr.ErrCipherSolve) {
		t.Fatalf("err = %v, want ErrCipherSolve", err)
	}
}

// TestSolve_Timeout checks that a descrambler that spins is interrupted by the
// configured timeout and reported as a cipher-solve failure.
func TestSolve_Timeout(t *testing.T) {
	p := compilePlayerProgram("u", wrapPlayer(descLoop))
	if p.compileErr != nil {
		t.Fatal(p.compileErr)
	}
	start := time.Now()
	_, err := p.decodeN(context.Background(), "12345", 50*time.Millisecond)
	if !errors.Is(err, waxerr.ErrCipherSolve) {
		t.Fatalf("err = %v, want ErrCipherSolve (timeout)", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout did not fire promptly: %v", elapsed)
	}
}

// TestSolve_Cancel checks that caller cancellation interrupts the solve and is
// reported as the context error, not a cipher failure.
func TestSolve_Cancel(t *testing.T) {
	p := compilePlayerProgram("u", wrapPlayer(descLoop))
	if p.compileErr != nil {
		t.Fatal(p.compileErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the AfterFunc fires and the spin is interrupted

	_, err := p.decodeN(ctx, "12345", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, waxerr.ErrCipherSolve) {
		t.Fatalf("caller cancel misreported as cipher failure: %v", err)
	}
}
