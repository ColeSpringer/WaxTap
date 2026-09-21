package loudness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
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
	l, err := MeasureCut(context.Background(), r, in, []cutrange.Range{{Start: 0, End: time.Second}}, 3*time.Second, 0, 0, 0)
	if err != nil {
		t.Fatalf("measure cut: %v", err)
	}
	if !l.Finite() {
		t.Errorf("cut measurement not finite: %+v", l)
	}
}

// A caller that measured nothing (sourceSamples 0) over a file whose headers
// only claim a length used to get a refusal here: the bounded span declared
// more than the truncated file holds. OpenComposed measures the source itself
// now, so the span asks for what the file has and the measurement succeeds at
// the length a read delivers, well short of the Xing-declared 2 s.
func TestMeasureCutWithNoLengthMeasuresTheSource(t *testing.T) {
	r := media.NewRunner(media.RunnerConfig{})
	dir := t.TempDir()
	ctx := context.Background()

	wavPath := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wavPath, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	intact := filepath.Join(dir, "intact.mp3")
	if _, err := r.Transcode(ctx, wavPath, intact, media.Spec{Codec: media.CodecMP3}); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	whole, err := os.ReadFile(intact)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(dir, "cut.mp3")
	if err := os.WriteFile(cut, whole[:len(whole)*6/10], 0o644); err != nil {
		t.Fatal(err)
	}
	pr, err := r.Probe(ctx, cut) // declares 2 s
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	measured, err := r.MeasureLength(ctx, cut)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	keeps := []cutrange.Range{{Start: 0, End: 1900 * time.Millisecond}}
	// A PathError here would be the old behaviour: a span past a truncated file
	// read as an I/O failure rather than as the file's own shortfall.
	l, err := MeasureCut(ctx, r, cut, keeps, pr.Format.Duration, 0, 0, 0)
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		t.Fatalf("got a PathError naming %s: a span past a truncated file is the file's shortfall, not a read failure", pe.Path)
	}
	if err != nil {
		t.Fatalf("MeasureCut: %v", err)
	}
	if !l.Finite() {
		t.Errorf("measurement not finite: %+v", l)
	}
	if measured.Duration >= pr.Format.Duration {
		t.Fatalf("measured %v against a declared %v: the fixture is not short", measured.Duration, pr.Format.Duration)
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
	album, perTrack, err := MeasureAlbum(context.Background(), r, inputs, nil)
	if err != nil {
		t.Fatalf("measure album: %v", err)
	}
	if len(perTrack) != 2 {
		t.Fatalf("perTrack = %d, want 2", len(perTrack))
	}
	if !album.Finite() || !perTrack[0].Finite() {
		t.Errorf("album/track measurements not finite: album=%+v track0=%+v", album, perTrack[0])
	}

	// A uniform fold moves both the tracks and the group: a stereo source
	// folded to mono is the audio a mono encode of the album would meter.
	mono, monoTracks, err := MeasureAlbum(context.Background(), r, inputs, []int{1, 1})
	if err != nil {
		t.Fatalf("measure album folded: %v", err)
	}
	if mono.IntegratedLUFS == album.IntegratedLUFS || monoTracks[0].IntegratedLUFS == perTrack[0].IntegratedLUFS {
		t.Errorf("folded album %+v / track %+v match the unfolded figures; the fold did not reach the measurement", mono, monoTracks[0])
	}
}

// TestMeasureAlbumNamesTheFirstBadTrack pins that the failure is reported by
// position and not by whichever read failed first. The album is a list and its
// failure has to name the earliest member a pass down it would have hit. The
// members are read in order, and the failure carries the member index, which
// the facade's albumTrackError turns into a file name.
//
// The fixtures are tiny: the bad tracks fail on their first header read, and
// the one good member is 50 ms of tone.
func TestMeasureAlbumNamesTheFirstBadTrack(t *testing.T) {
	r := media.NewRunner(media.RunnerConfig{})
	dir := t.TempDir()
	inputs := []string{filepath.Join(dir, "0-good.wav")}
	if err := os.WriteFile(inputs[0], mediatest.ToneWAVMs(440, 50, 2, 44100), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%d-bad.wav", i))
		if err := os.WriteFile(p, []byte("not audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, p)
	}
	for n := range 20 {
		_, _, err := MeasureAlbum(context.Background(), r, inputs, nil)
		if err == nil {
			t.Fatalf("album %d: measured an album of unreadable tracks", n)
		}
		if want := "group member 1"; !strings.Contains(err.Error(), want) {
			t.Fatalf("album %d: err = %v, want it to name %s, the earliest unreadable track", n, err, want)
		}
	}
}

// Each member is measured at its own width, not through an envelope: the
// per-track figures equal a standalone measurement of the same file at the
// same fold, and the group figure is a real number over the set. That is the
// property the Concat pass could not hold, because a timeline is built at one
// width: a mono member widened into a stereo envelope read 3 dB hot, and a
// 5.1 member widened to 7.1 and then folded landed about 1 dB under its own
// fold.
func TestMeasureAlbumMeasuresEachMemberAtItsOwnWidth(t *testing.T) {
	ctx := context.Background()
	r := media.NewRunner(media.RunnerConfig{})
	dir := t.TempDir()

	// A wide member's energy sits in its front pair, with the rest silent.
	// Every channel carrying the same tone folds back coherently to the same
	// loudness, which would hide what a widening does to the figure.
	wide := filepath.Join(dir, "wide.wav")
	if err := os.WriteFile(wide, mediatest.FrontsOnlyWAV(2, 6), 0o644); err != nil {
		t.Fatal(err)
	}
	mono := filepath.Join(dir, "mono.wav")
	if err := os.WriteFile(mono, mediatest.SineWAV(2, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	inputs := []string{wide, mono}
	folds := []int{2, 0}

	group, perTrack, err := MeasureAlbum(ctx, r, inputs, folds)
	if err != nil {
		t.Fatalf("measure album: %v", err)
	}
	if !group.Finite() {
		t.Errorf("group = %+v, want a finite figure", group)
	}
	for i, in := range inputs {
		solo, merr := Measure(ctx, r, in, folds[i])
		if merr != nil {
			t.Fatalf("measure %s alone: %v", filepath.Base(in), merr)
		}
		if d := math.Abs(perTrack[i].IntegratedLUFS - solo.IntegratedLUFS); d > 0.05 {
			t.Errorf("%s measured %.3f in the album, %.3f alone (%.3f LU apart)",
				filepath.Base(in), perTrack[i].IntegratedLUFS, solo.IntegratedLUFS, d)
		}
	}
	// The album's length is the members', since a group carries no format of
	// its own.
	if want := perTrack[0].Duration + perTrack[1].Duration; group.Duration != want {
		t.Errorf("group duration = %v, want the members' total %v", group.Duration, want)
	}
}

// TestAlbumGain covers the album-wide peak clamp and the guards that keep it from
// producing a gain WaxFlow would reject.
func TestAlbumGain(t *testing.T) {
	quiet := Loudness{IntegratedLUFS: -30, TruePeakDBTP: -20, LRA: 5}
	loud := Loudness{IntegratedLUFS: -12, TruePeakDBTP: -2, LRA: 5}
	silent := Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1)}
	hot := Loudness{IntegratedLUFS: -8, TruePeakDBTP: 0.5} // already over the ceiling
	album := Loudness{IntegratedLUFS: -20, TruePeakDBTP: -2, LRA: 6}

	// Without the clamp the gain is the plain offset, whatever the peaks are.
	if got := AlbumGain(-14, album, []Loudness{quiet, loud}, false); math.Abs(got-6) > 1e-9 {
		t.Errorf("limit gain = %v, want 6", got)
	}
	// With it, the least headroom wins: loud has -1 - -2 = 1 dB to give.
	if got := AlbumGain(-14, album, []Loudness{quiet, loud}, true); math.Abs(got-1) > 1e-9 {
		t.Errorf("cap gain = %v, want the 1 dB the loudest track allows", got)
	}
	// A track already over the ceiling attenuates the album, matching GainFor's
	// policy on a single file.
	if got := AlbumGain(-14, album, []Loudness{quiet, hot}, true); math.Abs(got-(-1.5)) > 1e-9 {
		t.Errorf("cap gain over a hot master = %v, want -1.5", got)
	}
	// A silent track reports a -Inf peak, which would make its headroom +Inf and
	// vanish from the minimum; skipping it keeps the finite tracks in charge.
	if got := AlbumGain(-14, album, []Loudness{silent, loud}, true); math.Abs(got-1) > 1e-9 {
		t.Errorf("cap gain beside a silent track = %v, want 1", got)
	}
	// An album of nothing but silence has no peak to clamp against and no loudness
	// to move: the gain must stay finite either way, because WaxFlow rejects a
	// non-finite GainDB and a working no-op would turn into an error.
	for _, clamp := range []bool{false, true} {
		if got := AlbumGain(-14, silent, []Loudness{silent, silent}, clamp); got != 0 {
			t.Errorf("silent album gain (clamp=%v) = %v, want 0", clamp, got)
		}
	}
}

func TestAlbumPeakShortfall(t *testing.T) {
	album := Loudness{IntegratedLUFS: -20, TruePeakDBTP: -2, LRA: 6}
	loud := Loudness{IntegratedLUFS: -12, TruePeakDBTP: -2}
	roomy := Loudness{IntegratedLUFS: -30, TruePeakDBTP: -20}
	silent := Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1)}

	// Wants +6, the clamp allows +1: 5 dB held back.
	if got := AlbumPeakShortfall(-14, album, []Loudness{roomy, loud}); math.Abs(got-5) > 1e-9 {
		t.Errorf("shortfall = %v, want 5", got)
	}
	// Plenty of headroom: nothing held back.
	if got := AlbumPeakShortfall(-14, album, []Loudness{roomy}); got != 0 {
		t.Errorf("shortfall with headroom = %v, want 0", got)
	}
	// Attenuating: the clamp cannot bind.
	if got := AlbumPeakShortfall(-26, album, []Loudness{roomy, loud}); got != 0 {
		t.Errorf("attenuating shortfall = %v, want 0", got)
	}
	// Nothing measurable reports no shortfall rather than an infinite one.
	if got := AlbumPeakShortfall(-14, Loudness{IntegratedLUFS: math.Inf(-1)}, []Loudness{silent}); got != 0 {
		t.Errorf("silent album shortfall = %v, want 0", got)
	}
	// The gain and the shortfall must agree: applied + held back == asked for.
	gain := AlbumGain(-14, album, []Loudness{roomy, loud}, true)
	short := AlbumPeakShortfall(-14, album, []Loudness{roomy, loud})
	if want := RawGain(-14, album.IntegratedLUFS); math.Abs(gain+short-want) > 1e-9 {
		t.Errorf("gain %v + shortfall %v != requested %v", gain, short, want)
	}
}

// A gated-silent album has no loudness to move, so neither mode moves it. The
// clamp has to sit behind that guard rather than in front of it: a silent album
// whose tracks still carry a peak over the ceiling would otherwise be attenuated
// by a clamp protecting nothing, and cap and limit would disagree on an album
// neither can change.
func TestAlbumGainSilentAlbumWithHotTrack(t *testing.T) {
	silentAlbum := Loudness{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: -0.5, LRA: 0}
	perTrack := []Loudness{
		{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: -0.5}, // silent body, hot transient
		{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: math.Inf(-1)},
	}
	for _, clamp := range []bool{false, true} {
		if got := AlbumGain(-14, silentAlbum, perTrack, clamp); got != 0 {
			t.Errorf("clamp=%v: gain = %v, want 0", clamp, got)
		}
	}
}
