package waxtap

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/media"
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

// TestPeakModeCapMissesAndWarns pins both peak policies on a source whose peak and
// loudness are far apart: the default cap policy lands well short of the target and
// says so, and limit either reaches the target or reports that it could not.
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

	// The invariant, on the fixture most likely to defeat the gain search: limit mode
	// never misses by more than the reporting threshold in silence, in either
	// direction, and never warns about a run that reached the target.
	//
	// Between the two thresholds nothing is asserted, because mapping.go documents
	// that band as deliberately silent: a miss under loudnessMissWarnDB is inside the
	// noise of a lossy encode and not worth telling the user about. Requiring
	// "converged or warned" would fail this test for a limiter improvement that lands
	// 0.6 LU short, which is behaving exactly as designed.
	//
	// This deliberately does not assert a fixed LU figure. QuietWithTransientWAV is
	// about -41 LUFS peaking near 0 dBTP, roughly 41 dB of crest, so it is the
	// saturation case: how close the limiter can be driven to a normal target is a
	// property of the limiter, not of WaxTap, and pinning a number here would turn an
	// upstream improvement into a test failure.
	limited := normalizeTo(t, in, filepath.Join(dir, "limit.flac"), target, PeakLimit)
	got := limited.Loudness.Output.IntegratedLUFS
	miss := math.Abs(got - target)
	warned := hasWarning(limited, WarnLoudnessTargetMissed)
	if miss > loudnessMissWarnDB && !warned {
		t.Errorf("limit output = %.3f LUFS misses %g by %.3f LU, past the %g LU reporting threshold, and said nothing: %+v",
			got, target, miss, loudnessMissWarnDB, limited.Warnings)
	}
	if miss <= loudness.ConvergeToleranceDB && warned {
		t.Errorf("limit output = %.3f LUFS converged but still warned: %+v", got, limited.Warnings)
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
	// PeakLimit has no clamp to read, so the cap-shaped input alone (with no output
	// measurement) tells it nothing and it stays silent.
	if warned(apply(PeakLimit), pres(clamped)) {
		t.Error("PeakLimit with no output measurement must not warn")
	}
	if warned(apply(PeakCap), pres(marginal)) {
		t.Errorf("a %g dB shortfall is below the %g dB threshold", 0.5, loudnessMissWarnDB)
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

// TestWarnLimiterTargetMissed covers the limit-mode branch, which is derived from
// the measured output because there is no clamp to attribute a miss to.
func TestWarnLimiterTargetMissed(t *testing.T) {
	limit := &LoudnessSpec{Mode: LoudnessApply, Target: -14, PeakMode: PeakLimit}
	out := func(lufs float64) pipeline.Result {
		return pipeline.Result{
			OutputLoudness: &loudness.Loudness{IntegratedLUFS: lufs, TruePeakDBTP: -1, LRA: 3},
			LoudnessPasses: 3,
		}
	}
	detail := func(p pipeline.Result) string {
		var got string
		em := newEmitter(func(e Event) {
			if e.Stage == StageWarning && e.Warning != nil && e.Warning.Code == WarnLoudnessTargetMissed {
				got = e.Warning.Detail
			}
		}, "")
		warnLoudnessTargetMissed(em, limit, p)
		return got
	}

	// A 1.4 LU shortfall: reported, with the achieved loudness and the pass count.
	d := detail(out(-15.4))
	for _, want := range []string{"1.4 LU short", "-14 LUFS target", "3 encode passes", "-15.4 LUFS"} {
		if !strings.Contains(d, want) {
			t.Errorf("shortfall detail = %q, want it to contain %q", d, want)
		}
	}
	// An overshoot is as much a silently wrong delivery as a shortfall.
	if d := detail(out(-12.0)); !strings.Contains(d, "2.0 LU above") {
		t.Errorf("overshoot detail = %q, want it to report an overshoot", d)
	}
	// Inside the reporting threshold: silent on both sides.
	for _, lufs := range []float64{-14.5, -13.5, -14} {
		if d := detail(out(lufs)); d != "" {
			t.Errorf("%.1f LUFS is within %g LU of the target but warned: %q", lufs, loudnessMissWarnDB, d)
		}
	}
	// No usable measurement: nothing honest to report.
	if d := detail(pipeline.Result{}); d != "" {
		t.Errorf("nil OutputLoudness warned: %q", d)
	}
	nonFinite := pipeline.Result{
		OutputLoudness: &loudness.Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1)},
		LoudnessPasses: 1,
	}
	if d := detail(nonFinite); d != "" {
		t.Errorf("silent output warned: %q", d)
	}
}

// TestProcessWarnsOutputClipping runs the whole chain: a float source with
// planted overs, transcoded to an integer output through the facade, must come
// back with the output-clipping warning and the clamp count.
func TestProcessWarnsOutputClipping(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "hot.wav")
	if err := os.WriteFile(in, mediatest.HotFloatWAV(1, 2, 13), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "out.flac")),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := findWarning(res.Warnings, WarnOutputClipping)
	if !ok {
		t.Fatalf("no output-clipping warning: %+v", res.Warnings)
	}
	for _, want := range []string{"clipping: 26 of ", "normalize"} {
		if !strings.Contains(w.Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", w.Detail, want)
		}
	}
}

// TestProcessWarnsTruePeakOnly runs the between-samples branch end to end: a
// source whose stored samples are in range but whose waveform crosses full
// scale must come back with the true-peak wording and a remedy that names the
// true peak, not the level.
func TestProcessWarnsTruePeakOnly(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "isp.wav")
	if err := os.WriteFile(in, mediatest.IntersampleHotWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "isp.flac")),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, ok := findWarning(res.Warnings, WarnOutputClipping)
	if !ok {
		t.Fatalf("no output-clipping warning: %+v", res.Warnings)
	}
	for _, want := range []string{"true peak:", "normalize to bring the true peak under full scale"} {
		if !strings.Contains(w.Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", w.Detail, want)
		}
	}
}

// TestWarnOutputClipping covers the level-warning policy: silent without a
// note, silent for a lossy source (the decoder manufactures the overshoot
// there), and a remedy chosen by what the run already tried.
func TestWarnOutputClipping(t *testing.T) {
	clipped := pipeline.Result{SourceCodec: "pcm", Levels: media.Levels{
		ClippedSamples: 26, Samples: 44100, Channels: 2, TruePeak: 1.0, Quantized: true,
	}}
	isp := pipeline.Result{SourceCodec: "pcm", Levels: media.Levels{
		Samples: 44100, Channels: 2, TruePeak: 1.2, Quantized: true,
	}}

	detail := func(ls *LoudnessSpec, p pipeline.Result) string {
		var got string
		em := newEmitter(func(e Event) {
			if e.Stage == StageWarning && e.Warning != nil && e.Warning.Code == WarnOutputClipping {
				got = e.Warning.Detail
			}
		}, "")
		warnOutputClipping(em, ls, p)
		return got
	}

	if d := detail(nil, pipeline.Result{SourceCodec: "pcm"}); d != "" {
		t.Errorf("no levels but warned: %q", d)
	}

	// A lossy decode legitimately overshoots full scale on loud masters, and the
	// clamp is inherent to any faithful integer conversion, so a lossy source
	// must not warn however many samples clipped.
	for _, codec := range []string{"opus", "aac", "mp3", "vorbis"} {
		lossy := clipped
		lossy.SourceCodec = codec
		if d := detail(nil, lossy); d != "" {
			t.Errorf("%s source warned: %q", codec, d)
		}
	}

	d := detail(nil, clipped)
	for _, want := range []string{"clipping: 26 of 88200", "normalize to bring the level under full scale"} {
		if !strings.Contains(d, want) {
			t.Errorf("unnormalized clip detail = %q, want it to carry %q", d, want)
		}
	}

	// The true-peak-only branch names what is actually over: the stored samples
	// are all in range, so the remedy speaks of the true peak, not the level.
	d = detail(nil, isp)
	for _, want := range []string{"true peak:", "normalize to bring the true peak under full scale"} {
		if !strings.Contains(d, want) {
			t.Errorf("unnormalized true-peak detail = %q, want it to carry %q", d, want)
		}
	}

	// Measure-only is not normalization: the remedy still applies.
	if d := detail(&LoudnessSpec{Mode: LoudnessMeasureOnly}, clipped); !strings.Contains(d, "normalize") {
		t.Errorf("measure-only detail = %q, want the normalize remedy", d)
	}

	// A limit-mode normalization can attenuate, which runs no limiter, so a hot
	// source can still clip; cap derives its clamp from the measured peak and is
	// the next knob to point at.
	limit := &LoudnessSpec{Mode: LoudnessApply, Target: -14, PeakMode: PeakLimit}
	if d := detail(limit, clipped); !strings.Contains(d, "--peak-mode cap") {
		t.Errorf("limit-mode detail = %q, want the --peak-mode cap remedy", d)
	}

	// Cap mode already held what it could see; there is nothing left to suggest.
	capped := &LoudnessSpec{Mode: LoudnessApply, Target: -14, PeakMode: PeakCap}
	if d := detail(capped, clipped); d != clipped.Levels.Note() {
		t.Errorf("cap-mode detail = %q, want the bare note %q", d, clipped.Levels.Note())
	}

	if got := WarnOutputClipping.String(); got != "output-clipping" {
		t.Errorf("WarnOutputClipping.String() = %q, want %q", got, "output-clipping")
	}
}
