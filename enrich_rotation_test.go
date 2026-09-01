package waxtap

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// The two failure shapes the loop distinguishes, as the fake produces them: the
// throttle's UNPLAYABLE disguise, and the ERROR status every dead, private, or
// nonexistent video answers with. The real predicate is throttleShaped, tested
// separately below; the loop tests inject fakeShaped so they exercise the
// sequencing, not the classification.
var (
	errShapedUnavailable  = errors.New("Video unavailable (status UNPLAYABLE)")
	errVerdictUnavailable = errors.New("This video is unavailable (status ERROR)")
)

func fakeShaped(err error) bool { return errors.Is(err, errShapedUnavailable) }

// passFake plays the metadata throttle as measured: an identity answers about
// the first window entries it is asked about and refuses every later one with
// the throttle's UNPLAYABLE shape, and a rotation resets that budget. Entries
// in alwaysFail are genuinely unavailable and answer the ERROR shape under any
// identity.
type passFake struct {
	window     int // requests one identity answers before refusing; 0 = unlimited
	alwaysFail map[int]bool
	asked      int // requests made under the current identity
	rotations  int
	passes     [][]int // indices each pass was asked about
	reported   int     // entries counted as settled
}

func (f *passFake) pass(idxs []int, report bool) ([]int, []error) {
	f.passes = append(f.passes, append([]int(nil), idxs...))
	var failed []int
	var causes []error
	for _, i := range idxs {
		f.asked++
		throttled := f.window > 0 && f.asked > f.window
		switch {
		case f.alwaysFail[i]:
			failed = append(failed, i)
			causes = append(causes, errVerdictUnavailable)
		case throttled:
			failed = append(failed, i)
			causes = append(causes, errShapedUnavailable)
		}
		if report {
			f.reported++
		}
	}
	return failed, causes
}

func (f *passFake) rotate(int) bool { f.rotations++; f.asked = 0; return true }

func TestEnrichRotationLoop(t *testing.T) {
	seq := func(n int) []int {
		s := make([]int, n)
		for i := range s {
			s[i] = i
		}
		return s
	}

	// The finding itself, at the shape it was measured: a channel about half again
	// the size of one identity's window loses its tail, and one rotation restores
	// every entry of it.
	t.Run("a rotation recovers the throttled tail", func(t *testing.T) {
		all := seq(150)
		f := &passFake{window: 100}
		failed, _, unproven := enrichRotationLoop(context.Background(), all, f.pass, f.rotate, fakeShaped)
		if len(failed) != 0 {
			t.Errorf("%d entries still failing, want none after the rotation", len(failed))
		}
		if unproven {
			t.Error("unproven = true, but every entry was settled under a working identity")
		}
		if f.rotations != 1 {
			t.Errorf("rotations = %d, want exactly 1", f.rotations)
		}
		if f.reported != len(all) {
			t.Errorf("reported %d entries settled, want %d: an entry revisited must not count twice", f.reported, len(all))
		}
	})

	// The cost guard the review asked for: a genuinely removed video answers
	// status ERROR, which is not the throttle's shape, so it must not wipe a warm
	// guest identity on every run of the playlist that contains it.
	t.Run("an ERROR verdict never rotates", func(t *testing.T) {
		f := &passFake{alwaysFail: map[int]bool{2: true}}
		failed, causes, unproven := enrichRotationLoop(context.Background(), seq(5), f.pass, f.rotate, fakeShaped)
		if len(failed) != 1 || failed[0] != 2 {
			t.Fatalf("failed = %v, want just entry 2", failed)
		}
		if f.rotations != 0 {
			t.Errorf("rotations = %d, want 0: the shape already says the verdict is real", f.rotations)
		}
		if len(f.passes) != 1 {
			t.Errorf("%d passes, want 1", len(f.passes))
		}
		if unproven {
			t.Error("unproven = true for a verdict that needed no test")
		}
		if len(causes) != 1 || !errors.Is(causes[0], errVerdictUnavailable) {
			t.Errorf("causes = %v, want the original verdict untouched", causes)
		}
	})

	// A shaped failure that survives a fresh identity is proven by the retry: a
	// pass that recovers nothing ends the loop at one rotation.
	t.Run("a shaped failure that persists costs one rotation", func(t *testing.T) {
		// window 0 but a shaped always-failure: model an UNPLAYABLE reason that is
		// not the throttle (a region variant) by making the entry fail shaped
		// under every identity.
		f := &passFake{}
		shapedAlways := func(idxs []int, report bool) ([]int, []error) {
			f.passes = append(f.passes, append([]int(nil), idxs...))
			if report {
				f.reported += len(idxs)
			}
			return []int{0}, []error{errShapedUnavailable}
		}
		failed, _, unproven := enrichRotationLoop(context.Background(), seq(3), shapedAlways, f.rotate, fakeShaped)
		if len(failed) != 1 {
			t.Fatalf("failed = %v, want the persistent entry", failed)
		}
		if f.rotations != 1 {
			t.Errorf("rotations = %d, want 1: a pass that recovers nothing is the proof", f.rotations)
		}
		if unproven {
			t.Error("unproven = true, but a fresh identity gave the same answer")
		}
	})

	// A playlist too long for the rotation budget leaves shaped entries the loop
	// never got a working identity to ask about. Those are retriable, not
	// verdicts, and the caller must be told which.
	t.Run("a spent budget reports unproven", func(t *testing.T) {
		// Each pass clears one window, so the budget covers window*(rotations+1).
		f := &passFake{window: 100}
		all := seq(100 * (maxEnrichRotations + 2))
		failed, _, unproven := enrichRotationLoop(context.Background(), all, f.pass, f.rotate, fakeShaped)
		if len(failed) == 0 {
			t.Fatal("no entries left failing; the fixture is meant to outrun the budget")
		}
		if !unproven {
			t.Error("unproven = false, but the budget ran out with shaped failures still recovering")
		}
		if f.rotations != maxEnrichRotations {
			t.Errorf("rotations = %d, want the budget %d", f.rotations, maxEnrichRotations)
		}
	})

	// A client with no identity to retire keeps the old behavior rather than
	// re-asking under the identity that just refused.
	t.Run("a refused rotation stops immediately", func(t *testing.T) {
		all := seq(150)
		f := &passFake{window: 100}
		failed, _, unproven := enrichRotationLoop(context.Background(), all, f.pass, func(int) bool { return false }, fakeShaped)
		if len(failed) != 50 {
			t.Fatalf("%d entries failing, want the 50 past the window, unretried", len(failed))
		}
		if unproven {
			t.Error("unproven = true; without a rotation nothing was tested either way, and the causes stand as they came")
		}
		if len(f.passes) != 1 {
			t.Errorf("%d passes, want 1: there is no point re-asking under a refused rotation", len(f.passes))
		}
	})

	// Retries ask only about what failed, never the whole playlist again: the
	// recovered entries are done and re-asking would put the load straight back.
	t.Run("retries are scoped to the failures", func(t *testing.T) {
		f := &passFake{window: 100}
		enrichRotationLoop(context.Background(), seq(150), f.pass, f.rotate, fakeShaped)
		if len(f.passes) != 2 {
			t.Fatalf("%d passes, want 2", len(f.passes))
		}
		if len(f.passes[1]) != 50 {
			t.Errorf("retry asked about %d entries, want the 50 that failed", len(f.passes[1]))
		}
	})

	// A throttled tail that also holds a real casualty settles both in ONE
	// rotation: the tail comes back, and the leftover's ERROR shape says the
	// verdict is real without another identity spent proving it.
	t.Run("a real verdict inside a throttled tail", func(t *testing.T) {
		f := &passFake{window: 100, alwaysFail: map[int]bool{120: true}}
		failed, causes, unproven := enrichRotationLoop(context.Background(), seq(150), f.pass, f.rotate, fakeShaped)
		if len(failed) != 1 || failed[0] != 120 {
			t.Fatalf("failed = %v, want just the genuinely unavailable entry", failed)
		}
		if unproven {
			t.Error("unproven = true, but the leftover is an unshaped verdict")
		}
		if f.rotations != 1 {
			t.Errorf("rotations = %d, want 1: the leftover's shape ends the loop", f.rotations)
		}
		if len(causes) != 1 || !errors.Is(causes[0], errVerdictUnavailable) {
			t.Errorf("causes = %v, want the verdict's own cause", causes)
		}
	})
}

// throttleShaped itself, against the real error types: the throttle's measured
// UNPLAYABLE disguise matches, and the ERROR status every dead or private video
// answers does not, nor do other sentinels.
func TestThrottleShaped(t *testing.T) {
	throttle := &waxerr.PlayabilityError{Status: "UNPLAYABLE", Reason: "Video unavailable", Sentinel: ErrVideoUnavailable}
	dead := &waxerr.PlayabilityError{Status: "ERROR", Reason: "This video is unavailable", Sentinel: ErrVideoUnavailable}
	members := &waxerr.PlayabilityError{Status: "UNPLAYABLE", Reason: "members only", Sentinel: ErrMembersOnly}

	if !throttleShaped(fmt.Errorf("enrich x: %w", throttle)) {
		t.Error("the measured throttle shape did not match")
	}
	if throttleShaped(fmt.Errorf("enrich x: %w", dead)) {
		t.Error("an ERROR-status verdict matched; a removed video would cost a rotation")
	}
	if throttleShaped(fmt.Errorf("enrich x: %w", members)) {
		t.Error("a members-only refusal matched; it is a verdict, not the throttle")
	}
	if throttleShaped(errors.New("dial tcp: connection refused")) {
		t.Error("a transport failure matched")
	}
}
