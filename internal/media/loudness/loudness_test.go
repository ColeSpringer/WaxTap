package loudness

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

func TestGainForNormal(t *testing.T) {
	// A quiet track (-20 LUFS, peak well under the ceiling) boosts to the target.
	g := GainFor(-14, Loudness{IntegratedLUFS: -20, TruePeakDBTP: -10})
	if math.Abs(g-6) > 1e-9 {
		t.Errorf("GainFor = %v, want +6 (target -14 minus -20)", g)
	}
}

func TestGainForSilenceIsZero(t *testing.T) {
	// Silence reports -Inf integrated loudness; the gain must be a no-op, not +Inf
	// (which WaxFlow would reject).
	if g := GainFor(-14, Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1)}); g != 0 {
		t.Errorf("silent GainFor = %v, want 0", g)
	}
	if g := GainFor(-14, Loudness{IntegratedLUFS: math.NaN()}); g != 0 {
		t.Errorf("NaN GainFor = %v, want 0", g)
	}
}

func TestGainForTruePeakHeadClamp(t *testing.T) {
	// A source already above the ceiling: peak protection wins over hitting the
	// exact LUFS, so the gain attenuates to keep the peak under -1 dBTP.
	g := GainFor(-14, Loudness{IntegratedLUFS: -30, TruePeakDBTP: 0.5})
	want := TruePeakCeilingDB - 0.5 // -1.5
	if math.Abs(g-want) > 1e-9 {
		t.Errorf("head-clamped GainFor = %v, want %v (peak-limited, not the +16 LUFS boost)", g, want)
	}
}

func TestGainForClampsToMax(t *testing.T) {
	// Near-silence with a very low peak: the raw gain (186 dB) exceeds both the
	// peak headroom and maxGainDB, so the maxGainDB clamp binds.
	if g := GainFor(-14, Loudness{IntegratedLUFS: -200, TruePeakDBTP: -200}); g != maxGainDB {
		t.Errorf("extreme GainFor = %v, want clamp to %v", g, maxGainDB)
	}
}

func TestRawGain(t *testing.T) {
	if g := RawGain(-14, -20); math.Abs(g-6) > 1e-9 {
		t.Errorf("RawGain = %v, want +6", g)
	}
	if g := RawGain(-14, math.Inf(-1)); g != 0 {
		t.Errorf("silent RawGain = %v, want 0", g)
	}
}

func TestRawGainIgnoresTruePeak(t *testing.T) {
	// The same measurement GainFor head-clamps to -1.5 dB. RawGain is the other
	// peak policy: it asks for the full +16 and leaves the peak to the limiter.
	m := Loudness{IntegratedLUFS: -30, TruePeakDBTP: 0.5}
	if g := RawGain(-14, m.IntegratedLUFS); math.Abs(g-16) > 1e-9 {
		t.Errorf("RawGain = %v, want +16 (unclamped)", g)
	}
	if g := GainFor(-14, m); g >= 0 {
		t.Errorf("GainFor = %v, want a negative head-clamped gain for the same source", g)
	}
}

func TestPeakShortfall(t *testing.T) {
	// The clamp binds: want +29, head -1-0 = -1, so it costs 30 dB.
	if got := PeakShortfall(-14, Loudness{IntegratedLUFS: -43, TruePeakDBTP: 0}); math.Abs(got-30) > 1e-9 {
		t.Errorf("PeakShortfall = %v, want 30", got)
	}
	// Headroom to spare: want +6, head +9, nothing held back.
	if got := PeakShortfall(-14, Loudness{IntegratedLUFS: -20, TruePeakDBTP: -10}); got != 0 {
		t.Errorf("PeakShortfall with spare headroom = %v, want 0", got)
	}
	// Attenuating: the ceiling cannot hold back a negative gain.
	if got := PeakShortfall(-23, Loudness{IntegratedLUFS: -9, TruePeakDBTP: -6}); got != 0 {
		t.Errorf("attenuating PeakShortfall = %v, want 0", got)
	}
	// Silence measures -Inf, which would otherwise be an infinite shortfall.
	if got := PeakShortfall(-14, Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1)}); got != 0 {
		t.Errorf("silent PeakShortfall = %v, want 0", got)
	}
}

// TestPeakShortfallExcludesMaxGainClamp: GainFor bounds its result to maxGainDB
// as well as to the peak ceiling, so target-integrated minus GainFor would blame
// the ceiling for a maxGainDB clamp. PeakShortfall reports only the peak's share,
// which is what the caller names in user-facing text.
func TestPeakShortfallExcludesMaxGainClamp(t *testing.T) {
	// A source far too quiet to reach the target within maxGainDB, with the peak
	// equally far down so the head clamp has nothing to do: want +186, head +185.
	// WaxFlow's -70 LUFS gate keeps a real measurement out of this range, so this
	// is a guard against the constants moving, not a live case.
	m := Loudness{IntegratedLUFS: -200, TruePeakDBTP: -186}
	if g := GainFor(-14, m); g != maxGainDB {
		t.Fatalf("GainFor = %v, want the maxGainDB clamp to bind for this fixture", g)
	}
	naive := (-14.0 - m.IntegratedLUFS) - GainFor(-14, m)
	if naive <= 60 {
		t.Fatalf("fixture does not exercise the maxGainDB clamp (naive shortfall %v)", naive)
	}
	// The peak's actual share is 1 dB, not the 66 dB the naive subtraction reports.
	if got := PeakShortfall(-14, m); math.Abs(got-1) > 1e-9 {
		t.Errorf("PeakShortfall = %v, want 1 (the head clamp's share, not maxGainDB's)", got)
	}
}

func TestFinite(t *testing.T) {
	if !(Loudness{IntegratedLUFS: -14, TruePeakDBTP: -2, LRA: 5}).Finite() {
		t.Error("finite measurement reported non-finite")
	}
	if (Loudness{IntegratedLUFS: math.Inf(-1)}).Finite() {
		t.Error("silence reported finite")
	}
}

func TestMeasureMatchesSignal(t *testing.T) {
	// The pure-Go fixture is a 440 Hz sine at 0.5 amplitude (-6 dBFS), so its true
	// peak measures near -6 dBTP.
	r := media.NewRunner(media.RunnerConfig{})
	in := filepath.Join(t.TempDir(), "s.wav")
	if err := os.WriteFile(in, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Measure(context.Background(), r, in, 0)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if !l.Finite() {
		t.Fatalf("measurement not finite: %+v", l)
	}
	if math.Abs(l.TruePeakDBTP-(-6)) > 1.0 {
		t.Errorf("true peak = %v dBTP, want ~-6 for a -6 dBFS sine", l.TruePeakDBTP)
	}
}

func TestMeasureDownmixFoldsTruePeak(t *testing.T) {
	// Summing identical surround channels into a stereo fold raises the true peak.
	// The downmix-aware measurement must observe that, so a later downmix+gain does
	// not clip against a peak the source layout hid.
	r := media.NewRunner(media.RunnerConfig{})
	in := filepath.Join(t.TempDir(), "surround.wav")
	if err := os.WriteFile(in, mediatest.SineWAV(2, 6), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := Measure(context.Background(), r, in, 0) // 6-channel source layout
	if err != nil {
		t.Fatalf("measure source: %v", err)
	}
	folded, err := Measure(context.Background(), r, in, 2) // measured as a stereo downmix
	if err != nil {
		t.Fatalf("measure folded: %v", err)
	}
	if math.Abs(folded.TruePeakDBTP-src.TruePeakDBTP) < 1.0 {
		t.Errorf("folded true peak %.2f dBTP barely differs from the 6ch source %.2f; the fold was not applied",
			folded.TruePeakDBTP, src.TruePeakDBTP)
	}
}

func TestMeasureCut(t *testing.T) {
	r := media.NewRunner(media.RunnerConfig{})
	in := filepath.Join(t.TempDir(), "s.wav")
	os.WriteFile(in, mediatest.SineWAV(3, 2), 0o644)
	// Measuring a 1s slice of a steady tone yields the same loudness as the whole.
	l, err := MeasureCut(context.Background(), r, in, []cutrange.Range{{Start: 0, End: time.Second}}, 3*time.Second, 0, 0)
	if err != nil {
		t.Fatalf("measure cut: %v", err)
	}
	if !l.Finite() {
		t.Errorf("cut measurement not finite: %+v", l)
	}
}

func TestMeasureAlbum(t *testing.T) {
	r := media.NewRunner(media.RunnerConfig{})
	dir := t.TempDir()
	var inputs []string
	for _, n := range []string{"a.wav", "b.wav"} {
		p := filepath.Join(dir, n)
		os.WriteFile(p, mediatest.SineWAV(2, 2), 0o644)
		inputs = append(inputs, p)
	}
	album, perTrack, err := MeasureAlbum(context.Background(), r, inputs)
	if err != nil {
		t.Fatalf("measure album: %v", err)
	}
	if len(perTrack) != 2 {
		t.Fatalf("perTrack = %d, want 2", len(perTrack))
	}
	if !album.Finite() || !perTrack[0].Finite() {
		t.Errorf("album/track measurements not finite: album=%+v track0=%+v", album, perTrack[0])
	}
}
