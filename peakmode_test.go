package waxtap

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
)

// peakFixture writes the quiet-body/loud-transient WAV that separates the two
// peak policies: its true peak sits at ~0 dBTP while its integrated loudness is
// near -41 LUFS, so PeakCap can take almost no gain.
func peakFixture(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "loud-peak.wav")
	if err := os.WriteFile(p, mediatest.QuietWithTransientWAV(6, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func normalizeTo(t *testing.T, in, out string, target float64, mode PeakMode) *Result {
	t.Helper()
	res, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Loudness:  &LoudnessSpec{Mode: LoudnessApply, Target: target, PeakMode: mode},
		},
	})
	if err != nil {
		t.Fatalf("Process (%v): %v", mode, err)
	}
	if res.Loudness == nil || res.Loudness.Output == nil {
		t.Fatalf("no output loudness measured: %+v", res)
	}
	return res
}

func hasWarning(res *Result, code WarningCode) bool {
	for _, w := range res.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// TestPeakModeCapMissesAndWarns pins the defect F5 reported: on a source whose
// peak and loudness are far apart, the default cap policy lands well short of the
// target and says so, while limit hits it.
func TestPeakModeCapMissesAndWarns(t *testing.T) {
	dir := t.TempDir()
	in := peakFixture(t, dir)
	const target = -14.0

	capped := normalizeTo(t, in, filepath.Join(dir, "cap.flac"), target, PeakCap)
	if got := capped.Loudness.Output.IntegratedLUFS; got > target-5 {
		t.Errorf("cap output = %.1f LUFS, want well short of %g (the head-clamp should bind)", got, target)
	}
	if !hasWarning(capped, WarnLoudnessTargetMissed) {
		t.Errorf("cap mode missed the target without WarnLoudnessTargetMissed: %+v", capped.Warnings)
	}

	// A few LU of residual gap is expected, not a miss: the transient contributed
	// to the measured input loudness, and the limiter takes it back on the way out.
	limited := normalizeTo(t, in, filepath.Join(dir, "limit.flac"), target, PeakLimit)
	if got := limited.Loudness.Output.IntegratedLUFS; math.Abs(got-target) > 4 {
		t.Errorf("limit output = %.1f LUFS, want within 4 LU of %g", got, target)
	}
	if hasWarning(limited, WarnLoudnessTargetMissed) {
		t.Errorf("limit mode hit the target but still warned: %+v", limited.Warnings)
	}
}

// TestPeakModeAttenuatingNeverWarns: normalizing down cannot be held back by the
// ceiling, so neither policy warns and both land on the target.
func TestPeakModeAttenuatingNeverWarns(t *testing.T) {
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 3, "flac") // ~-9 LUFS, peak ~-6 dBTP
	const target = -23.0

	for _, tc := range []struct {
		name string
		mode PeakMode
	}{{"cap", PeakCap}, {"limit", PeakLimit}} {
		res := normalizeTo(t, in, filepath.Join(dir, tc.name+".flac"), target, tc.mode)
		if hasWarning(res, WarnLoudnessTargetMissed) {
			t.Errorf("%s: attenuating normalization warned: %+v", tc.name, res.Warnings)
		}
		if got := res.Loudness.Output.IntegratedLUFS; math.Abs(got-target) > 2 {
			t.Errorf("%s: output = %.1f LUFS, want within 2 LU of %g", tc.name, got, target)
		}
	}
}

// TestWarnLoudnessTargetMissed covers the branches the end-to-end tests cannot
// reach cheaply: the shortfall threshold, silence, and a missing measurement.
func TestWarnLoudnessTargetMissed(t *testing.T) {
	pres := func(in *loudness.Loudness) pipeline.Result { return pipeline.Result{InputLoudness: in} }
	// -43 LUFS peaking at 0 dBTP: the clamp holds the gain 30 dB short.
	clamped := &loudness.Loudness{IntegratedLUFS: -43, TruePeakDBTP: 0}
	// Short by 0.5 dB: real, but below the reporting threshold.
	marginal := &loudness.Loudness{IntegratedLUFS: -20, TruePeakDBTP: -6.5}
	apply := func(m PeakMode) *LoudnessSpec {
		return &LoudnessSpec{Mode: LoudnessApply, Target: -14, PeakMode: m}
	}

	warned := func(ls *LoudnessSpec, p pipeline.Result) bool {
		var got bool
		em := newEmitter(func(e Event) {
			if e.Stage == StageWarning && e.Warning != nil && e.Warning.Code == WarnLoudnessTargetMissed {
				got = true
			}
		}, "")
		warnLoudnessTargetMissed(em, ls, p)
		return got
	}

	if !warned(apply(PeakCap), pres(clamped)) {
		t.Error("a 30 dB clamp should warn")
	}
	if warned(apply(PeakLimit), pres(clamped)) {
		t.Error("PeakLimit does not clamp, so it must not warn")
	}
	if warned(apply(PeakCap), pres(marginal)) {
		t.Errorf("a %g dB shortfall is below the %g dB threshold", 0.5, loudnessClampWarnDB)
	}
	// Silence measures -Inf, which would make the shortfall +Inf.
	if warned(apply(PeakCap), pres(&loudness.Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1)})) {
		t.Error("silence must not warn")
	}
	if warned(apply(PeakCap), pres(nil)) {
		t.Error("an unmeasured input must not warn")
	}
	if warned(&LoudnessSpec{Mode: LoudnessMeasureOnly, Target: -14}, pres(clamped)) {
		t.Error("measure-only applies no gain, so it must not warn")
	}
	if warned(nil, pres(clamped)) {
		t.Error("nil loudness spec must not warn")
	}
}
