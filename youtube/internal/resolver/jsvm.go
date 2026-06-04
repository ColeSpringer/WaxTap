package resolver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dop251/goja"

	"github.com/colespringer/waxtap/waxerr"
)

// errCipherTimeout is the cause attached when cipher JS exceeds its execution
// budget. It unwraps to waxerr.ErrCipherSolve: a transform that will not solve in
// time is, for the caller, a cipher-solve failure (and a maintenance signal).
var errCipherTimeout = fmt.Errorf("%w: cipher JS execution timed out", waxerr.ErrCipherSolve)

// playerProgram holds the cipher transforms extracted from one base.js, compiled
// into shareable goja programs. A *goja.Program is safe to share across
// goroutines, but a *goja.Runtime is not, so each execution gets a throwaway
// runtime built from these programs.
//
// Extraction is tolerant per transform: a video served direct (unsigned) URLs
// needs only n, while a ciphered video needs both. A missing transform is
// recorded as an error and surfaced only if that transform is actually used.
type playerProgram struct {
	playerURL string

	sigProgram *goja.Program
	sigName    string
	sigErr     error

	nProgram *goja.Program
	nName    string
	nErr     error
}

// compilePlayerProgram extracts and compiles the signature and n transforms from
// base.js. It never fails as a whole: per-transform extraction/compile errors are
// retained on the returned value so a usable transform still works even if the
// other could not be located.
func compilePlayerProgram(playerURL, js string) *playerProgram {
	p := &playerProgram{playerURL: playerURL}

	if src, name, err := extractSignatureSource(js); err != nil {
		p.sigErr = err
	} else if prog, cerr := goja.Compile(playerURL+"#sig", src, false); cerr != nil {
		p.sigErr = fmt.Errorf("%w: compile signature function: %v", waxerr.ErrCipherSolve, cerr)
	} else {
		p.sigProgram, p.sigName = prog, name
	}

	if src, name, err := extractNSource(js); err != nil {
		p.nErr = err
	} else if prog, cerr := goja.Compile(playerURL+"#n", src, false); cerr != nil {
		p.nErr = fmt.Errorf("%w: compile n function: %v", waxerr.ErrCipherSolve, cerr)
	} else {
		p.nProgram, p.nName = prog, name
	}

	return p
}

// extractedTransform reports whether at least one cipher transform compiled.
// Real base.js can have one locator break while the other still works; HTML and
// other non-player bodies generally produce neither and are unsafe to persist.
func (p *playerProgram) extractedTransform() bool {
	return p.sigErr == nil || p.nErr == nil
}

// decipherSignature runs the extracted signature transform on s.
func (p *playerProgram) decipherSignature(ctx context.Context, s string, timeout time.Duration) (string, error) {
	if p.sigErr != nil {
		return "", p.sigErr
	}
	return runTransform(ctx, p.sigProgram, p.sigName, s, timeout)
}

// decodeN runs the extracted n-parameter transform on n.
func (p *playerProgram) decodeN(ctx context.Context, n string, timeout time.Duration) (string, error) {
	if p.nErr != nil {
		return "", p.nErr
	}
	return runTransform(ctx, p.nProgram, p.nName, n, timeout)
}

// runTransform evaluates a compiled cipher program in a fresh runtime and calls
// the named function with arg.
//
// A child context bounds execution by timeout, and context.AfterFunc interrupts
// the VM when that context is done. The interrupt hook is stopped before return.
func runTransform(ctx context.Context, prog *goja.Program, name, arg string, timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, errCipherTimeout)
		defer cancel()
	}

	vm := goja.New()
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(context.Cause(ctx)) })
	defer stop()

	if _, err := vm.RunProgram(prog); err != nil {
		return "", cipherRunError(ctx, "load cipher program", err)
	}
	fn, ok := goja.AssertFunction(vm.Get(name))
	if !ok {
		return "", fmt.Errorf("%w: cipher function %q is not callable", waxerr.ErrCipherSolve, name)
	}
	out, err := fn(goja.Undefined(), vm.ToValue(arg))
	if err != nil {
		return "", cipherRunError(ctx, "run cipher function", err)
	}
	return out.String(), nil
}

// cipherRunError maps a goja failure to the context cause (cancel or
// errCipherTimeout) when the runtime was interrupted, otherwise to an
// ErrCipherSolve carrying the underlying JS error.
func cipherRunError(ctx context.Context, stage string, err error) error {
	if _, ok := errors.AsType[*goja.InterruptedError](err); ok {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return ctx.Err()
	}
	return fmt.Errorf("%w: %s: %v", waxerr.ErrCipherSolve, stage, err)
}
