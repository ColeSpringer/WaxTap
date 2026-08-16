package waxtap

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
