package pipeline

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
)

// searchFake stands in for the encoder: write records the gain it was handed, and
// measure hands back the next scripted loudness for the file that write left
// behind. Real audio cannot drive the search's tail branches on demand, and those
// are the ones that decide which file the user is left with.
type searchFake struct {
	lufs  []float64 // delivered loudness, one entry per measured pass
	seen  int       // measurements served
	gains []float64 // gain handed to each write, in order
}

func (s *searchFake) measure() (loudness.Loudness, error) {
	if s.seen >= len(s.lufs) {
		return loudness.Loudness{}, errors.New("measure: no reading available")
	}
	v := s.lufs[s.seen]
	s.seen++
	return loudness.Loudness{IntegratedLUFS: v, TruePeakDBTP: -1, LRA: 4}, nil
}

func (s *searchFake) write(enc media.Spec) error {
	s.gains = append(s.gains, enc.GainDB)
	return nil
}

// run drives converge over the script, with the first pass already written at gain
// 0, and returns what the caller would report.
func (s *searchFake) run(t *testing.T, target float64) (*loudness.Loudness, int) {
	t.Helper()
	out, passes, err := converge(context.Background(), target, media.Spec{}, s.measure, s.write, func(Stage) {})
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	return out, passes
}

// delivered asserts the loudness converge reported, which by contract describes
// the file left on disk.
func delivered(t *testing.T, got *loudness.Loudness, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("no measurement reported, want %.1f LUFS", want)
	}
	if math.Abs(got.IntegratedLUFS-want) > 1e-9 {
		t.Errorf("delivered %.3f LUFS, want %.1f", got.IntegratedLUFS, want)
	}
}

// TestConvergeRestoresBestPassOnLastWrite covers the budget's edge: a final pass
// that overshoots is still a write, and the restoring rewrite has to happen anyway.
// Bounding the restore by maxLoudnessWrites left the rejected pass on disk, which
// is the silent wrong delivery the search exists to prevent.
func TestConvergeRestoresBestPassOnLastWrite(t *testing.T) {
	// Three improving passes, then an overshoot past the target.
	s := &searchFake{lufs: []float64{-16, -15, -14.5, -12}}
	out, passes := s.run(t, -14)

	if len(s.gains) != 4 {
		t.Fatalf("writes = %d (gains %v), want 4: three corrections and one restore", len(s.gains), s.gains)
	}
	// The last write must put back the gain that produced the best pass (-14.5),
	// not leave the 2 LU overshoot the search itself rejected.
	if s.gains[3] != s.gains[1] {
		t.Errorf("final write used gain %.3f, want %.3f (the best pass)", s.gains[3], s.gains[1])
	}
	delivered(t, out, -14.5)
	if passes != maxLoudnessWrites+1 {
		t.Errorf("LoudnessPasses = %d, want %d", passes, maxLoudnessWrites+1)
	}
}

// TestConvergeSpendsNoExtraWriteWhenLastPassIsBest: the restoring write is allowed
// past the budget, so it must not fire on a run that ends on its best pass. That
// run is the common one, and an extra encode there is pure cost.
func TestConvergeSpendsNoExtraWriteWhenLastPassIsBest(t *testing.T) {
	// Improving throughout, never reaching tolerance: the budget ends the search.
	s := &searchFake{lufs: []float64{-20, -18, -16.5, -15.2}}
	out, passes := s.run(t, -14)

	if len(s.gains) != maxLoudnessWrites-1 {
		t.Fatalf("writes = %d (gains %v), want %d with no restore", len(s.gains), s.gains, maxLoudnessWrites-1)
	}
	delivered(t, out, -15.2)
	if passes != maxLoudnessWrites {
		t.Errorf("LoudnessPasses = %d, want %d", passes, maxLoudnessWrites)
	}
}

// TestConvergeStopsInsideTolerance: reaching the target ends the search
// immediately, budget or not.
func TestConvergeStopsInsideTolerance(t *testing.T) {
	s := &searchFake{lufs: []float64{-16, -14.1}}
	out, passes := s.run(t, -14)

	if len(s.gains) != 1 {
		t.Fatalf("writes = %d (gains %v), want 1", len(s.gains), s.gains)
	}
	delivered(t, out, -14.1)
	if passes != 2 {
		t.Errorf("LoudnessPasses = %d, want 2", passes)
	}
}

// TestConvergeRestoresBestPassWhenMeasurementFails: a pass that cannot be measured
// is a file of unknown loudness. The search puts back the last pass it does know,
// so the reported measurement keeps describing what is on disk.
func TestConvergeRestoresBestPassWhenMeasurementFails(t *testing.T) {
	s := &searchFake{lufs: []float64{-16}} // the second measurement fails
	out, passes := s.run(t, -14)

	if len(s.gains) != 2 {
		t.Fatalf("writes = %d (gains %v), want 2: one correction and one restore", len(s.gains), s.gains)
	}
	if s.gains[1] != 0 {
		t.Errorf("restored gain %.3f, want the first pass's 0", s.gains[1])
	}
	delivered(t, out, -16)
	if passes != 3 {
		t.Errorf("LoudnessPasses = %d, want 3", passes)
	}
}

// TestConvergeSilenceStopsImmediately: a non-finite measurement is silence, which
// no amount of gain moves. Reporting nothing is correct, and so is not writing
// again.
func TestConvergeSilenceStopsImmediately(t *testing.T) {
	s := &searchFake{lufs: []float64{math.Inf(-1)}}
	out, passes := s.run(t, -14)

	if len(s.gains) != 0 {
		t.Errorf("writes = %d (gains %v), want 0", len(s.gains), s.gains)
	}
	if out != nil {
		t.Errorf("reported %+v, want no measurement", out)
	}
	if passes != 1 {
		t.Errorf("LoudnessPasses = %d, want 1", passes)
	}
}

// TestConvergeCanceledMidSearchReturnsError: cancellation is the one failure the
// search must not swallow. The file on disk is complete but carries an
// uncorrected gain, so reporting success would present a loudness nobody asked
// for as the delivered result.
func TestConvergeCanceledMidSearchReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &searchFake{lufs: []float64{-16}}
	measure := func() (loudness.Loudness, error) {
		out, err := s.measure()
		cancel() // the second measurement lands on a canceled context
		return out, err
	}

	_, _, err := converge(ctx, -14, media.Spec{}, measure, s.write, func(Stage) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
