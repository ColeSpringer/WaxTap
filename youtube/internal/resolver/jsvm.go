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

// playerProgram holds one base.js compiled for solving. program is the whole
// player with its IIFE body hoisted to global scope; descramblers are the
// candidate transform functions found by AST fingerprint, each carrying a
// compiled driver. A *goja.Program is safe to share across goroutines, but a
// *goja.Runtime is not, so each solve builds a throwaway runtime.
//
// Only the compiled program and candidate metadata are retained — never the raw
// source or AST (a compiled player measures tens of MB on its own).
type playerProgram struct {
	playerURL    string
	program      *goja.Program // full base.js, IIFE unwrapped to global scope
	descramblers []candidate   // direct-stmt alr/yes matches, resolved by consensus
	compileErr   error         // parse/unwrap/compile/no-candidate failure (wraps ErrCipherSolve)

	// sts is the signature timestamp parsed from base.js. stsOK is false when no
	// recognized pattern matched. Both are independent of descrambler discovery:
	// a player whose descrambler is not found still yields its sts.
	sts   int
	stsOK bool
}

// compilePlayerProgram parses base.js, hoists its IIFE body to global scope,
// compiles the result, and fingerprints the descrambler candidates. Any failure
// along the way is retained in compileErr (wrapping ErrCipherSolve) rather than
// returned, so the signature timestamp — parsed up front and independent of the
// descrambler — survives even when no candidate is found.
func compilePlayerProgram(playerURL, js string) *playerProgram {
	p := &playerProgram{playerURL: playerURL}

	// Parse sts first and unconditionally: it must not depend on descrambler
	// discovery succeeding.
	p.sts, p.stsOK = extractSignatureTimestamp(js)

	iife, argName, err := parsePlayer(js)
	if err != nil {
		p.compileErr = err
		return p
	}
	unwrapped, err := unwrapPlayer(js, iife, argName)
	if err != nil {
		p.compileErr = err
		return p
	}
	program, err := goja.Compile(playerURL, unwrapped, false)
	if err != nil {
		p.compileErr = fmt.Errorf("%w: compile player %s: %v", waxerr.ErrCipherSolve, playerURL, err)
		return p
	}
	cands := findDescramblers(playerURL, iife)
	if len(cands) == 0 {
		p.compileErr = fmt.Errorf("%w: no descrambler candidates in player %s", waxerr.ErrCipherSolve, playerURL)
		return p
	}

	p.program = program
	p.descramblers = cands
	return p
}

// extractedTransform reports whether the player compiled and yielded at least one
// descrambler candidate. HTML interstitials and other non-player bodies fail to
// parse or carry no candidate, so this also gates whether a body is safe to
// persist to the source cache.
func (p *playerProgram) extractedTransform() bool {
	return p.compileErr == nil
}

// decipherSignature runs the player's descrambler on the encoded signature s in
// its own runtime. Resolve, which needs both transforms, shares one session
// instead (see openSession).
func (p *playerProgram) decipherSignature(ctx context.Context, s string, timeout time.Duration) (string, error) {
	return p.solve(ctx, kindSig, s, timeout)
}

// decodeN runs the player's descrambler on the throttling parameter n in its own
// runtime.
func (p *playerProgram) decodeN(ctx context.Context, n string, timeout time.Duration) (string, error) {
	return p.solve(ctx, kindN, n, timeout)
}

// solve is the single-shot path: open a session, solve one transform, close. It
// backs DescrambleN and the inspect paths, which need only one value.
func (p *playerProgram) solve(ctx context.Context, kind transformKind, arg string, timeout time.Duration) (string, error) {
	sess, err := p.openSession(ctx, timeout)
	if err != nil {
		return "", err
	}
	defer sess.close()
	return sess.solve(kind, arg)
}

// solveSession is a runtime with the browser stub and player loaded, so several
// transforms can be solved against one (expensive) player execution. Executing
// the whole player is the dominant per-resolution cost, and the descrambler is a
// pure transform, so reusing the loaded player across the signature and n solves
// is safe (yt-dlp does the same). It is NOT goroutine-safe: open one per
// resolution and close it when done.
type solveSession struct {
	p      *playerProgram
	vm     *goja.Runtime
	ctx    context.Context
	cancel context.CancelFunc // non-nil only when a timeout context was created
	stop   func() bool        // cancels the interrupt hook
}

// openSession builds a runtime and runs the env stub and player once. The whole
// load-and-solve window is bounded by timeout via one context + vm.Interrupt
// hook. The returned session must be closed.
func (p *playerProgram) openSession(ctx context.Context, timeout time.Duration) (*solveSession, error) {
	if p.compileErr != nil {
		return nil, p.compileErr
	}
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, errCipherTimeout)
	}
	vm := goja.New()
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(context.Cause(ctx)) })
	s := &solveSession{p: p, vm: vm, ctx: ctx, cancel: cancel, stop: stop}

	if _, err := vm.RunProgram(envStubProgram); err != nil {
		s.close()
		return nil, cipherRunError(ctx, "load browser environment stub", err)
	}
	if _, err := vm.RunProgram(p.program); err != nil {
		s.close()
		return nil, cipherRunError(ctx, "load player program", err)
	}
	return s, nil
}

// close stops the interrupt hook and releases the timeout context.
func (s *solveSession) close() {
	s.stop()
	if s.cancel != nil {
		s.cancel()
	}
}

// solve drives every descrambler candidate for one transform and returns the
// single distinct valid result (consensus). Candidates that throw are skipped; a
// VM interrupt (timeout or caller cancel) is fatal. With no valid result, or two
// that disagree, it returns a self-describing ErrCipherSolve carrying the player
// URL, sts, and candidate count.
func (s *solveSession) solve(kind transformKind, arg string) (string, error) {
	// The driver reads both inputs regardless of kind, so both must be defined to
	// avoid a ReferenceError; the unused one is undefined.
	switch kind {
	case kindN:
		s.vm.Set("__wt_n", arg)
		s.vm.Set("__wt_sig", goja.Undefined())
	case kindSig:
		s.vm.Set("__wt_sig", arg)
		s.vm.Set("__wt_n", goja.Undefined())
	}

	survivors := make(map[string]struct{}, 2)
	for _, cand := range s.p.descramblers {
		out, err := runCandidate(s.vm, cand, kind)
		if err != nil {
			if _, ok := errors.AsType[*goja.InterruptedError](err); ok {
				return "", cipherRunError(s.ctx, "run descrambler", err)
			}
			continue // a candidate that throws produces nothing; exclude it
		}
		if validResult(kind, arg, out) {
			survivors[out] = struct{}{}
		}
	}
	if len(survivors) == 1 {
		for out := range survivors {
			return out, nil
		}
	}
	return "", fmt.Errorf("%w: %s solve found %d distinct valid result(s) from %d candidate(s) [player %s sts=%d]",
		waxerr.ErrCipherSolve, kind, len(survivors), len(s.p.descramblers), s.p.playerURL, s.p.sts)
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
