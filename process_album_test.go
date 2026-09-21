package waxtap

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// TestProcessAlbumValidation covers checks that run before the engine is needed.
func TestProcessAlbumValidation(t *testing.T) {
	c, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("no inputs", func(t *testing.T) {
		if _, err := c.ProcessAlbum(ctx, nil, -14, TranscodeSpec{Format: FormatFLAC}); err == nil {
			t.Error("expected error for empty album")
		}
	})

	t.Run("copy rejected", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "a.flac", Output: "b.flac"}}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatCopy})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("copy album = %v, want ErrIncompatibleSpec", err)
		}
	})

	t.Run("same input/output rejected", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "same.flac", Output: "same.flac"}}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("same-path album = %v, want ErrIncompatibleSpec", err)
		}
	})

	t.Run("missing output path", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "a.flac", Output: ""}}
		if _, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC}); err == nil {
			t.Error("expected error for missing output path")
		}
	})

	t.Run("two tracks share an output", func(t *testing.T) {
		tracks := []AlbumTrack{
			{Input: "a.flac", Output: "out.flac"},
			{Input: "b.flac", Output: "out.flac"},
		}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("shared output = %v, want ErrIncompatibleSpec", err)
		}
	})

	t.Run("output overwrites another track's input", func(t *testing.T) {
		tracks := []AlbumTrack{
			{Input: "a.flac", Output: "b.flac"}, // would clobber track 2's source
			{Input: "b.flac", Output: "c.flac"},
		}
		_, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC})
		if !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("cross-clobber = %v, want ErrIncompatibleSpec", err)
		}
	})

	t.Run("out-of-range or non-finite target rejected", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "a.flac", Output: "out/a.flac"}}
		for _, target := range []float64{-100, 0, math.NaN(), math.Inf(1)} {
			if _, err := c.ProcessAlbum(ctx, tracks, target, TranscodeSpec{Format: FormatFLAC}); !errors.Is(err, ErrIncompatibleSpec) {
				t.Errorf("target %v = %v, want ErrIncompatibleSpec", target, err)
			}
		}
	})

	t.Run("negative bitrate rejected", func(t *testing.T) {
		tracks := []AlbumTrack{{Input: "a.flac", Output: "out/a.flac"}}
		if _, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatMP3, Bitrate: -1}); !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("negative bitrate = %v, want ErrIncompatibleSpec", err)
		}
	})
}

// albumFixtures writes two FLAC tracks whose loudness is far apart and whose true
// peaks differ, so the album-wide clamp has something to bind on and the spacing
// has something to preserve.
func albumFixtures(t *testing.T, dir string) []AlbumTrack {
	t.Helper()
	quiet := filepath.Join(dir, "quiet.wav") // ~-41 LUFS, 0 dBTP
	if err := os.WriteFile(quiet, mediatest.QuietWithTransientWAV(3, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	loudFLAC := synthSine(t, dir, "loud.flac", 3, "flac") // ~-9 LUFS, ~-6 dBTP
	return []AlbumTrack{
		{Input: quiet, Output: filepath.Join(dir, "out", "quiet.flac")},
		{Input: loudFLAC, Output: filepath.Join(dir, "out", "loud.flac")},
	}
}

func albumWarning(res *AlbumProcessResult, code WarningCode) (Warning, bool) {
	for _, w := range res.Warnings {
		if w.Code == code {
			return w, true
		}
	}
	return Warning{}, false
}

// TestProcessAlbumWarnsOutputClipping: clipping tracks aggregate into one
// warning where the worst track speaks for the album (the way albumFold folds
// the downmix observation), the clean track stays out of the count, and cap
// mode, which clamps the album under the true-peak ceiling, must not warn at
// all.
func TestProcessAlbumWarnsOutputClipping(t *testing.T) {
	dir := t.TempDir()
	hot := filepath.Join(dir, "hot.wav")
	if err := os.WriteFile(hot, mediatest.HotFloatWAV(1, 2, 13), 0o644); err != nil {
		t.Fatal(err)
	}
	warm := filepath.Join(dir, "warm.wav")
	if err := os.WriteFile(warm, mediatest.HotFloatWAV(1, 2, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	clean := synthSine(t, dir, "clean.flac", 1, "flac")
	c := newOfflineClient(t)
	ctx := context.Background()

	// Limit mode aims straight at the target; the resulting few-dB attenuation
	// leaves the ~+15.6 dB overs past full scale, so the encodes must clamp them.
	tracks := []AlbumTrack{
		{Input: warm, Output: filepath.Join(dir, "limit", "warm.flac")},
		{Input: hot, Output: filepath.Join(dir, "limit", "hot.flac")},
		{Input: clean, Output: filepath.Join(dir, "limit", "clean.flac")},
	}
	res, err := c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC}, WithAlbumPeakMode(PeakLimit))
	if err != nil {
		t.Fatal(err)
	}
	var clips []Warning
	for _, w := range res.Warnings {
		if w.Code == WarnOutputClipping {
			clips = append(clips, w)
		}
	}
	if len(clips) != 1 {
		t.Fatalf("output-clipping warnings = %+v, want exactly one covering the album", clips)
	}
	// The worst track (26 clipped samples, not warm's 10) speaks for the album,
	// the second clipping track is counted, and the remedy points at the peak
	// mode, the knob an already-normalizing run has left.
	for _, want := range []string{tracks[1].Output, "clipping: 26 of ", "and 1 more track", "--peak-mode cap"} {
		if !strings.Contains(clips[0].Detail, want) {
			t.Errorf("detail = %q, want it to carry %q", clips[0].Detail, want)
		}
	}
	if strings.Contains(clips[0].Detail, "normalize") {
		t.Errorf("detail = %q suggests normalizing to a run that just did", clips[0].Detail)
	}

	// Cap mode binds on the hot track's true peak and holds the whole album
	// under the ceiling, so the same overs land below full scale.
	capTracks := []AlbumTrack{
		{Input: hot, Output: filepath.Join(dir, "cap", "hot.flac")},
		{Input: clean, Output: filepath.Join(dir, "cap", "clean.flac")},
	}
	res, err = c.ProcessAlbum(ctx, capTracks, -14, TranscodeSpec{Format: FormatFLAC}, WithAlbumPeakMode(PeakCap))
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := albumWarning(res, WarnOutputClipping); ok {
		t.Errorf("cap mode warned %q; the clamp should keep every sample under the ceiling", w.Detail)
	}
}

// TestProcessAlbumCapPreservesSpacing is F3: the mandatory limiter pulled louder
// tracks down harder, so inputs an exact distance apart came out closer together
// with no way to opt out. The album-wide clamp leaves the limiter idle, so the
// delivered spacing is the input spacing.
func TestProcessAlbumCapPreservesSpacing(t *testing.T) {
	dir := t.TempDir()
	tracks := albumFixtures(t, dir)
	c := newOfflineClient(t)
	ctx := context.Background()

	// A boosting target, so limit mode would have something for the limiter to give
	// back and cap mode has a clamp to bind.
	const target = -6.0
	res, err := c.ProcessAlbum(ctx, tracks, target, TranscodeSpec{Format: FormatFLAC}, WithAlbumPeakMode(PeakCap))
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}

	inSpacing := res.PerTrack[1].IntegratedLUFS - res.PerTrack[0].IntegratedLUFS
	var out [2]LoudnessInfo
	for i, p := range res.Outputs {
		if out[i], err = c.Measure(ctx, p); err != nil {
			t.Fatalf("Measure %s: %v", p, err)
		}
	}
	outSpacing := out[1].IntegratedLUFS - out[0].IntegratedLUFS
	if math.Abs(outSpacing-inSpacing) > 0.1 {
		t.Errorf("spacing in = %.3f LU, out = %.3f LU: a uniform gain must reproduce it", inSpacing, outSpacing)
	}
	// Every track moved by exactly the reported gain, which is what "uniform" means.
	for i := range out {
		if got, want := out[i].IntegratedLUFS, res.PerTrack[i].IntegratedLUFS+res.GainDB; math.Abs(got-want) > 0.2 {
			t.Errorf("track %d delivered %.3f LUFS, want %.3f (input %+.3f dB)", i, got, want, res.GainDB)
		}
	}
}

// The clamp costs loudness, and that cost used to be silent: ProcessAlbum never
// warned at all, so a miss past the project's own 1 LU threshold went unreported.
func TestProcessAlbumCapWarnsOnShortfall(t *testing.T) {
	dir := t.TempDir()
	tracks := albumFixtures(t, dir)
	res, err := newOfflineClient(t).ProcessAlbum(context.Background(), tracks, -6, TranscodeSpec{Format: FormatFLAC}, WithAlbumPeakMode(PeakCap))
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	w, ok := albumWarning(res, WarnLoudnessTargetMissed)
	if !ok {
		t.Fatalf("cap mode missed the target without a warning: %+v (gain %+.2f dB)", res.Warnings, res.GainDB)
	}
	if !strings.Contains(w.Detail, "true-peak") || !strings.Contains(w.Detail, "delivered") {
		t.Errorf("detail = %q, want the cause and the delivered loudness", w.Detail)
	}
	if res.Delivered == nil {
		t.Fatal("Delivered must be populated in cap mode too; an absent field would trip JSON consumers")
	}
	// Derived, not measured: the limiter is idle, so it is exactly album + gain.
	if got, want := res.Delivered.IntegratedLUFS, res.Album.IntegratedLUFS+res.GainDB; math.Abs(got-want) > 1e-9 {
		t.Errorf("Delivered = %v, want the derived %v", got, want)
	}
}

// An attenuating gain never engages the limiter, so both modes land on the target
// analytically and neither needs a second pass over the outputs to find out.
func TestProcessAlbumAttenuatingDerivesDelivered(t *testing.T) {
	dir := t.TempDir()
	tracks := albumFixtures(t, dir)
	c := newOfflineClient(t)

	for _, mode := range []struct {
		name string
		mode PeakMode
	}{{"cap", PeakCap}, {"limit", PeakLimit}} {
		t.Run(mode.name, func(t *testing.T) {
			for i := range tracks {
				tracks[i].Output = filepath.Join(dir, mode.name, filepath.Base(tracks[i].Input)+".flac")
			}
			res, err := c.ProcessAlbum(context.Background(), tracks, -35, TranscodeSpec{Format: FormatFLAC}, WithAlbumPeakMode(mode.mode))
			if err != nil {
				t.Fatalf("ProcessAlbum: %v", err)
			}
			if res.GainDB >= 0 {
				t.Fatalf("gain = %+.2f dB, want an attenuating one for this target", res.GainDB)
			}
			if res.Delivered == nil {
				t.Fatal("Delivered must be populated")
			}
			if got, want := res.Delivered.IntegratedLUFS, res.Album.IntegratedLUFS+res.GainDB; math.Abs(got-want) > 1e-9 {
				t.Errorf("Delivered = %v, want the derived %v (no measurement should have run)", got, want)
			}
			if _, ok := albumWarning(res, WarnLoudnessTargetMissed); ok {
				t.Errorf("an attenuating album lands on target; it must not warn: %+v", res.Warnings)
			}
		})
	}
}

// A limiting album that boosts is the one case measurement can answer and
// arithmetic cannot, and it is where the report's silent 1.16 LU miss came from.
//
// It uses the crest fixture rather than albumFixtures: the limiter engaging is
// not the same as the integrated loudness moving. A half-millisecond transient
// gets shaved without shifting the gated measurement at all, so a fixture built
// around one would report a miss of zero and prove nothing. CrestWAV carries four
// transients a second, which the gate does count.
func TestProcessAlbumLimitMeasuresAndWarns(t *testing.T) {
	dir := t.TempDir()
	var tracks []AlbumTrack
	for _, n := range []string{"a", "b"} {
		in := filepath.Join(dir, n+".wav")
		if err := os.WriteFile(in, mediatest.CrestWAV(3, 1), 0o644); err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, AlbumTrack{Input: in, Output: filepath.Join(dir, "out", n+".flac")})
	}

	const target = -6.0
	res, err := newOfflineClient(t).ProcessAlbum(context.Background(), tracks, target, TranscodeSpec{Format: FormatFLAC}, WithAlbumPeakMode(PeakLimit))
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	if res.GainDB <= 0 {
		t.Fatalf("gain = %+.2f dB, want a boosting one for this target", res.GainDB)
	}
	if res.Delivered == nil {
		t.Fatal("Delivered must be measured for a boosting limit")
	}
	// Measured, not derived: the limiter only ever gives gain back, so a measured
	// delivery cannot land above the arithmetic album + gain, and on this fixture it
	// lands visibly below it.
	derived := res.Album.IntegratedLUFS + res.GainDB
	if res.Delivered.IntegratedLUFS > derived {
		t.Errorf("Delivered = %.3f, above the arithmetic %.3f: the limiter cannot add loudness", res.Delivered.IntegratedLUFS, derived)
	}
	if derived-res.Delivered.IntegratedLUFS < 1e-9 {
		t.Errorf("Delivered = %.6f equals the arithmetic %.6f; the measurement did not run", res.Delivered.IntegratedLUFS, derived)
	}

	// The invariant, not a fixed LU figure: how far the limiter can be driven is a
	// property of the limiter, so pinning a number would turn an upstream
	// improvement into a failure. Between the threshold and zero nothing is
	// asserted, for the reason mapping.go documents.
	miss := math.Abs(target - res.Delivered.IntegratedLUFS)
	w, warned := albumWarning(res, WarnLoudnessTargetMissed)
	switch {
	case miss > loudnessMissWarnDB && !warned:
		t.Errorf("delivered %.3f LUFS misses %g by %.3f LU and said nothing: %+v", res.Delivered.IntegratedLUFS, target, miss, res.Warnings)
	case miss <= loudnessMissWarnDB && warned:
		t.Errorf("delivered %.3f LUFS is inside the threshold but warned: %+v", res.Delivered.IntegratedLUFS, res.Warnings)
	case warned && !strings.Contains(w.Detail, "single uniform-gain pass"):
		t.Errorf("detail = %q, want it to say why album mode cannot iterate onto the target", w.Detail)
	}
}

// Album mode writes through runner.Transcode rather than the pipeline, so it does
// not reach warnImplicitDownmix, and the engine's own log line is demoted. Without
// its own detection an --album --format mp3 run on a surround master exits 0 with
// an empty warnings array and half the channels gone.
func TestProcessAlbumWarnsImplicitDownmix(t *testing.T) {
	dir := t.TempDir()
	var tracks []AlbumTrack
	for _, n := range []string{"a", "b"} {
		in := filepath.Join(dir, n+".wav")
		if err := os.WriteFile(in, mediatest.SineWAV(1, 6), 0o644); err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, AlbumTrack{Input: in, Output: filepath.Join(dir, "out", n+".mp3")})
	}
	res, err := newOfflineClient(t).ProcessAlbum(t.Context(), tracks, -14, TranscodeSpec{Format: FormatMP3})
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	w, ok := albumWarning(res, WarnImplicitDownmix)
	if !ok {
		t.Fatalf("a 6-channel album folded to stereo without a warning: %+v", res.Warnings)
	}
	if !strings.Contains(w.Detail, "6 channels") {
		t.Errorf("detail = %q, want the source channel count", w.Detail)
	}
	// One warning for the album, not one per track.
	n := 0
	for _, warn := range res.Warnings {
		if warn.Code == WarnImplicitDownmix {
			n++
		}
	}
	if n != 1 {
		t.Errorf("got %d implicit-downmix warnings, want 1 for the album", n)
	}

	// A lossless album that keeps the layout says nothing.
	for i := range tracks {
		tracks[i].Output = filepath.Join(dir, "keep", filepath.Base(tracks[i].Input)+".flac")
	}
	kept, err := newOfflineClient(t).ProcessAlbum(t.Context(), tracks, -14, TranscodeSpec{Format: FormatFLAC})
	if err != nil {
		t.Fatalf("ProcessAlbum (flac): %v", err)
	}
	if _, ok := albumWarning(kept, WarnImplicitDownmix); ok {
		t.Errorf("a format that holds 6 channels must not warn: %+v", kept.Warnings)
	}
}

// TestMeasureAlbumNamesUnreadableTrack pins the file name on a per-track
// failure: an album has many inputs, and "unsupported or unreadable input"
// alone sends the user through all of them to find the bad one.
func TestMeasureAlbumNamesUnreadableTrack(t *testing.T) {
	dir := t.TempDir()
	good := synthSine(t, dir, "good.flac", 1, "flac")
	bad := filepath.Join(dir, "bad.flac")
	if err := os.WriteFile(bad, []byte("not audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := newOfflineClient(t).MeasureAlbum(context.Background(), []string{good, bad})
	if !errors.Is(err, ErrUnsupportedInput) || !strings.Contains(err.Error(), "track bad.flac") {
		t.Errorf("MeasureAlbum = %v, want ErrUnsupportedInput naming track bad.flac", err)
	}
}

// An album into a lossy format is measured at the width the encoder delivers,
// the same fold a single file gets: without it every track's figure, and the
// album gain derived from them, describes audio the encoder never meters, and
// a surround album lands a fold's worth off target.
func TestProcessAlbumMeasuresAtTheEncodersWidth(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	var tracks []AlbumTrack
	for i, n := range []string{"a", "b"} {
		in := filepath.Join(dir, n+".wav")
		// One fronts-only member and one with the tone in every channel: a
		// coherent fold reads back at the source loudness, a fronts-only one
		// loses the fold's normalization, so the album needs both to show the
		// per-track figure is the folded one.
		body := mediatest.FrontsOnlyWAV(2, 6)
		if i == 1 {
			body = mediatest.SineWAV(2, 6)
		}
		if err := os.WriteFile(in, body, 0o644); err != nil {
			t.Fatal(err)
		}
		tracks = append(tracks, AlbumTrack{Input: in, Output: filepath.Join(dir, "out", n+".opus")})
	}

	const target = -18.0
	res, err := c.ProcessAlbum(ctx, tracks, target, TranscodeSpec{Format: FormatOpus}, WithAlbumPeakMode(PeakCap))
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	for i, tr := range tracks {
		single, err := c.Process(ctx, ProcessRequest{Input: tr.Input, ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "single", filepath.Base(tr.Output))),
			Transcode: &TranscodeSpec{Format: FormatOpus},
			Loudness:  &LoudnessSpec{Mode: LoudnessMeasureOnly},
			Channels:  LayoutStereo,
			Downmix:   true,
		}})
		if err != nil {
			t.Fatalf("Process track %d: %v", i, err)
		}
		if got, want := res.PerTrack[i].IntegratedLUFS, single.Loudness.Input.IntegratedLUFS; math.Abs(got-want) > 0.05 {
			t.Errorf("track %d measured %.2f LUFS, the same fold alone measures %.2f", i, got, want)
		}
	}
	// The written files, not the arithmetic: a cap-mode Delivered is derived
	// from the album figure, so it lands on the target whatever the encoder
	// then did to the samples.
	written, err := c.MeasureAlbum(ctx, res.Outputs)
	if err != nil {
		t.Fatalf("MeasureAlbum of the outputs: %v", err)
	}
	if got := written.Album.IntegratedLUFS; math.Abs(got-target) > 0.5 {
		t.Errorf("the written album measures %.2f LUFS, want %g within 0.5", got, target)
	}
	if res.Delivered == nil {
		t.Fatal("no delivered measurement")
	}
	if math.Abs(res.Delivered.IntegratedLUFS-written.Album.IntegratedLUFS) > 0.5 {
		t.Errorf("Delivered says %.2f LUFS, the files measure %.2f", res.Delivered.IntegratedLUFS, written.Album.IntegratedLUFS)
	}
}

// An album mixing a surround member with a stereo one is measured as a group:
// the timeline places the narrower member into the widest layout with its
// missing positions silent, which is loudness-neutral, so the group figure is
// what every member contributes at its own width. What is still refused is a
// member whose positions have no home in that layout, and that refusal names
// the track.
func TestAlbumMeasuresMixedWidthsAsAGroup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	wide := filepath.Join(dir, "wide.wav")
	narrow := filepath.Join(dir, "narrow.wav")
	if err := os.WriteFile(wide, mediatest.FrontsOnlyWAV(2, 6), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(narrow, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newOfflineClient(t)

	res, err := c.MeasureAlbum(ctx, []string{wide, narrow})
	if err != nil {
		t.Fatalf("MeasureAlbum: %v", err)
	}
	if math.IsInf(res.Album.IntegratedLUFS, 0) || math.IsNaN(res.Album.IntegratedLUFS) {
		t.Errorf("group figure = %v, want a finite measurement", res.Album.IntegratedLUFS)
	}
	// The narrower member contributes what it is, not what widening made of it.
	solo, err := c.MeasureAlbum(ctx, []string{narrow})
	if err != nil {
		t.Fatalf("MeasureAlbum(narrow): %v", err)
	}
	if d := math.Abs(res.PerTrack[1].IntegratedLUFS - solo.PerTrack[0].IntegratedLUFS); d > 0.01 {
		t.Errorf("stereo member measured %.3f in the mixed album, %.3f alone: off by %.3f LU",
			res.PerTrack[1].IntegratedLUFS, solo.PerTrack[0].IntegratedLUFS, d)
	}

	out := filepath.Join(dir, "out")
	pres, err := c.ProcessAlbum(ctx, []AlbumTrack{
		{Input: wide, Output: filepath.Join(out, "a.flac")},
		{Input: narrow, Output: filepath.Join(out, "b.flac")},
	}, -14, TranscodeSpec{Format: FormatFLAC})
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	if math.IsInf(pres.Album.IntegratedLUFS, 0) || math.IsNaN(pres.Album.IntegratedLUFS) {
		t.Errorf("ProcessAlbum group figure = %v, want a finite measurement", pres.Album.IntegratedLUFS)
	}
	for _, n := range []string{"a.flac", "b.flac"} {
		if _, serr := os.Stat(filepath.Join(out, n)); serr != nil {
			t.Errorf("%s was not written: %v", n, serr)
		}
	}
}

// A member whose channel positions differ from its neighbours' is measured as
// it is. Two 6-channel files, one with its rear pair at the back and one at
// the sides, used to be refused: the group was a concatenation, a timeline is
// built at one width and one layout, and the second file's positions had no
// home in the first's. Each member is now decoded at its own width, so the
// set has no envelope to be refused by and each track's encode stands alone.
func TestAlbumMeasuresAMemberWhosePositionsDiffer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// FL|FR|FC|LFE|BL|BR, which is audio.DefaultLayout(6).
	back := filepath.Join(dir, "back.wav")
	// FL|FR|FC|LFE|SL|SR: the same count, a pair the back layout cannot place.
	side := filepath.Join(dir, "side.wav")
	if err := os.WriteFile(back, mediatest.MaskedWAV(2, 6, 0x3F), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(side, mediatest.MaskedWAV(2, 6, 0x60F), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newOfflineClient(t)

	res, err := c.MeasureAlbum(ctx, []string{back, side})
	if err != nil {
		t.Fatalf("MeasureAlbum: %v", err)
	}
	if math.IsInf(res.Album.IntegratedLUFS, 0) || math.IsNaN(res.Album.IntegratedLUFS) {
		t.Errorf("album figure = %v, want a finite measurement", res.Album.IntegratedLUFS)
	}
	for i, l := range res.PerTrack {
		if math.IsInf(l.IntegratedLUFS, 0) || math.IsNaN(l.IntegratedLUFS) {
			t.Errorf("track %d = %v, want a finite measurement", i, l.IntegratedLUFS)
		}
	}

	// The write path agrees: both tracks are written. The target is WAV
	// because FLAC refuses this layout at its own encoder.
	out := filepath.Join(dir, "out")
	if _, perr := c.ProcessAlbum(ctx, []AlbumTrack{
		{Input: back, Output: filepath.Join(out, "a.wav")},
		{Input: side, Output: filepath.Join(out, "b.wav")},
	}, -14, TranscodeSpec{Format: FormatWAV}); perr != nil {
		t.Fatalf("ProcessAlbum: %v", perr)
	}
	for _, n := range []string{"a.wav", "b.wav"} {
		if _, serr := os.Stat(filepath.Join(out, n)); serr != nil {
			t.Errorf("%s was not written: %v", n, serr)
		}
	}
}

// albumTrackError names the track behind every shape WaxFlow words a member
// refusal in: the plan-time "timeline member N", the seam refusal, and the
// run-time "member N: " prefix waxerr.Annotate adds, which arrives after a
// classifier has already prepended its own text.
func TestAlbumTrackErrorNamesEveryMemberShape(t *testing.T) {
	inputs := []string{"/tmp/first.flac", "/tmp/second.flac"}
	for _, tc := range []struct {
		name string
		text string
		want string
	}{
		{"plan time", "waxflow: timeline member 1 cannot be mixed into 6 channels", "second.flac"},
		{"seam", "waxflow: timeline member 0 could not be positioned", "first.flac"},
		{"run time", "waxtap: unsupported or unreadable input: member 1: short read", "second.flac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := albumTrackError(errors.New(tc.text), inputs)
			if !strings.Contains(got.Error(), tc.want) {
				t.Errorf("albumTrackError(%q) = %v, want it to name %s", tc.text, got, tc.want)
			}
		})
	}
	// A message with no member index of its own passes through untouched, and
	// "member" inside another word is not one: relabelling an unrelated failure
	// with a track name is worse than leaving it unnamed.
	for _, text := range []string{
		"waxflow: no output format requested",
		"open /music/remember 1.flac: no such file or directory",
		"open /music/dismember 0.flac: permission denied",
	} {
		plain := errors.New(text)
		if got := albumTrackError(plain, inputs); got != plain {
			t.Errorf("albumTrackError(%q) = %v, want it unchanged", text, got)
		}
	}
}

// An all-Opus album under cap takes the header path too: every track is a
// packet copy whose head states the one album gain.
func TestProcessAlbumWritesHeaderGain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	opus := func(name string) string {
		wav := filepath.Join(dir, name+".wav")
		if err := os.WriteFile(wav, mediatest.SineWAV(2, 2), 0o644); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, name+".opus")
		if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
			Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
		}}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	tracks := []AlbumTrack{
		{Input: opus("a"), Output: filepath.Join(dir, "out", "a.opus")},
		{Input: opus("b"), Output: filepath.Join(dir, "out", "b.opus")},
	}

	res, err := c.ProcessAlbum(ctx, tracks, -20, TranscodeSpec{Format: FormatOpus}, WithAlbumPeakMode(PeakCap))
	if err != nil {
		t.Fatalf("ProcessAlbum: %v", err)
	}
	if !res.HeaderGain || !res.LoudnessApplied {
		t.Fatalf("HeaderGain=%v LoudnessApplied=%v, want the header path", res.HeaderGain, res.LoudnessApplied)
	}
	for _, out := range res.Outputs {
		q, qerr := media.OpusHeaderGain(ctx, out)
		if qerr != nil || q != media.OpusGainQ78(res.GainDB) {
			t.Errorf("%s: header gain %d (%v), want the album's %d", filepath.Base(out), q, qerr, media.OpusGainQ78(res.GainDB))
		}
	}
	if res.Delivered == nil || math.Abs(res.Delivered.IntegratedLUFS-(res.Album.IntegratedLUFS+res.GainDB)) > 1e-9 {
		t.Errorf("Delivered = %+v, want the derived album + gain", res.Delivered)
	}

	// Anything that needs an encode takes the encode path, as a single file does.
	for _, tc := range []struct {
		name string
		run  func() (*AlbumProcessResult, error)
	}{
		{"limit", func() (*AlbumProcessResult, error) {
			return c.ProcessAlbum(ctx, retarget(tracks, filepath.Join(dir, "limit")), -20, TranscodeSpec{Format: FormatOpus}, WithAlbumPeakMode(PeakLimit))
		}},
		{"bitrate", func() (*AlbumProcessResult, error) {
			return c.ProcessAlbum(ctx, retarget(tracks, filepath.Join(dir, "bitrate")), -20, TranscodeSpec{Format: FormatOpus, Bitrate: 96000}, WithAlbumPeakMode(PeakCap))
		}},
		{"flac member", func() (*AlbumProcessResult, error) {
			mixed := retarget(tracks, filepath.Join(dir, "mixed"))
			mixed[1].Input = synthSine(t, dir, "member.flac", 2, "flac")
			return c.ProcessAlbum(ctx, mixed, -20, TranscodeSpec{Format: FormatOpus}, WithAlbumPeakMode(PeakCap))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.run()
			if err != nil {
				t.Fatal(err)
			}
			if got.HeaderGain {
				t.Error("HeaderGain set on a run that has to re-encode")
			}
		})
	}
}

// retarget copies tracks with their outputs moved into dir.
func retarget(tracks []AlbumTrack, dir string) []AlbumTrack {
	out := make([]AlbumTrack, len(tracks))
	for i, t := range tracks {
		out[i] = AlbumTrack{Input: t.Input, Output: filepath.Join(dir, filepath.Base(t.Output))}
	}
	return out
}

// A measurement reports what it found in the inputs, the way a processing run
// does: a listener told an album measures -23 LUFS deserves to know one track
// stopped decoding halfway.
func TestMeasureAlbumReportsInputWarnings(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	intact, truncated := damagedFixture(t, dir, "track.flac", FormatFLAC)

	res, err := c.MeasureAlbum(ctx, []string{intact, truncated})
	if err != nil {
		t.Fatalf("MeasureAlbum: %v", err)
	}
	var damage []Warning
	for _, w := range res.Warnings {
		if w.Code == WarnInputDamage {
			damage = append(damage, w)
		}
	}
	if len(damage) != 1 {
		t.Fatalf("damage warnings = %+v, want one folded warning", res.Warnings)
	}
	if !strings.Contains(damage[0].Detail, filepath.Base(truncated)) {
		t.Errorf("detail = %q, want it to name %s", damage[0].Detail, filepath.Base(truncated))
	}

	// An album with nothing to measure says so rather than reporting -Inf bare.
	silent := make([]string, 2)
	for i := range silent {
		silent[i] = filepath.Join(dir, "silent"+string(rune('a'+i))+".wav")
		if err := os.WriteFile(silent[i], mediatest.SilenceWAV(2, 2), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	quiet, err := c.MeasureAlbum(ctx, silent)
	if err != nil {
		t.Fatalf("MeasureAlbum (silent): %v", err)
	}
	if !albumHasWarning(quiet.Warnings, WarnLoudnessUnmeasurable) {
		t.Errorf("warnings = %+v, want loudness-unmeasurable", quiet.Warnings)
	}
}

func albumHasWarning(ws []Warning, code WarningCode) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

// A mono member is measured as mono. The old group pass built one timeline
// at one width, and a mono member placed into a stereo envelope was
// duplicated onto both fronts and read 3 dB hot, so an album to a lossy
// target (where a wide member forces a stereo timeline) came out 0.7 LU
// away from the same album to a lossless one. The group is now the union of
// every member's own gated blocks, so the target codec cannot move the figure.
func TestAlbumMonoMemberIsNotCountedAsDualMono(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	stereo := filepath.Join(dir, "stereo.wav")
	mono := filepath.Join(dir, "mono.wav")
	wide := filepath.Join(dir, "wide.wav")
	for p, b := range map[string][]byte{stereo: mediatest.SineWAV(2, 2), mono: mediatest.SineWAV(2, 1), wide: mediatest.FrontsOnlyWAV(2, 6)} {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	measure := func(format TranscodeFormat, ext string) (*AlbumProcessResult, error) {
		var tracks []AlbumTrack
		for i, in := range []string{stereo, mono, wide} {
			tracks = append(tracks, AlbumTrack{Input: in, Output: filepath.Join(dir, ext, strconv.Itoa(i)+"."+ext)})
		}
		return c.ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: format}, WithAlbumPeakMode(PeakCap))
	}
	flac, err := measure(FormatFLAC, "flac")
	if err != nil {
		t.Fatal(err)
	}
	opus, err := measure(FormatOpus, "opus")
	if err != nil {
		t.Fatal(err)
	}
	solo, err := c.Measure(ctx, mono)
	if err != nil {
		t.Fatal(err)
	}
	for name, res := range map[string]*AlbumProcessResult{"flac": flac, "opus": opus} {
		if d := math.Abs(res.PerTrack[1].IntegratedLUFS - solo.IntegratedLUFS); d > 0.05 {
			t.Errorf("%s: mono member measured %.2f in the album, %.2f alone", name, res.PerTrack[1].IntegratedLUFS, solo.IntegratedLUFS)
		}
	}
	// The group figure itself: over a set with nothing to fold, it is the
	// energy-weighted mean of the members' own gated blocks, which the two
	// equal-length members make the plain mean of their energies. A mono
	// member widened onto a stereo pair reads 3 dB hot and pulls the group
	// most of the way up to the stereo member's figure, which is what the
	// concatenated group pass did.
	pair, err := c.ProcessAlbum(ctx, []AlbumTrack{
		{Input: stereo, Output: filepath.Join(dir, "pair", "0.flac")},
		{Input: mono, Output: filepath.Join(dir, "pair", "1.flac")},
	}, -14, TranscodeSpec{Format: FormatFLAC}, WithAlbumPeakMode(PeakCap))
	if err != nil {
		t.Fatal(err)
	}
	energy := func(lufs float64) float64 { return math.Pow(10, lufs/10) }
	want := 10 * math.Log10((energy(pair.PerTrack[0].IntegratedLUFS)+energy(pair.PerTrack[1].IntegratedLUFS))/2)
	if d := math.Abs(pair.Album.IntegratedLUFS - want); d > 0.1 {
		t.Errorf("album of a stereo and a mono member measured %.3f, want %.3f (the mean of their energies, %.3f LU off); the mono member was not measured as mono",
			pair.Album.IntegratedLUFS, want, d)
	}
}

// An album member the filesystem will not open is an I/O failure, exit 10,
// the same as the single-file path gives. It is not bad input: the file may
// be perfectly good audio the run cannot read.
func TestAlbumUnreadableMemberIsAnIOFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a 0000 file is merely read-only on Windows, and read-only files still open for reading")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	ctx := context.Background()
	dir := t.TempDir()
	good := filepath.Join(dir, "good.wav")
	if err := os.WriteFile(good, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(dir, "locked.wav")
	if err := os.WriteFile(locked, mediatest.SineWAV(1, 2), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o600) })

	c := newOfflineClient(t)
	_, err := c.MeasureAlbum(ctx, []string{good, locked})
	if err == nil {
		t.Fatal("MeasureAlbum on an unreadable member succeeded")
	}
	// The same class the single-file path reports, so one unreadable file
	// does not change meaning by being in an album.
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Errorf("err = %v (%T), want a *fs.PathError as the single-file path gives", err, err)
	}
	if errors.Is(err, ErrUnsupportedInput) {
		t.Errorf("err = %v, want an I/O failure rather than bad input", err)
	}
	if !strings.Contains(err.Error(), "locked.wav") {
		t.Errorf("err = %v, want it to name the track", err)
	}

	// The single-file path, for comparison.
	_, serr := c.Measure(ctx, locked)
	if !errors.As(serr, &pathErr) {
		t.Errorf("single-file err = %v (%T), want a *fs.PathError", serr, serr)
	}
}
