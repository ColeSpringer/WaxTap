package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// warningDetail returns the detail of the first warning carrying code, or "".
func warningDetail(res *Result, code WarningCode) string {
	for _, w := range res.Warnings {
		if w.Code == code {
			return w.Detail
		}
	}
	return ""
}

// damagedFixture writes a fixture in the given format and returns the intact
// path and a truncated copy of it.
func damagedFixture(t *testing.T, dir, name string, format TranscodeFormat) (intact, truncated string) {
	t.Helper()
	ctx := context.Background()
	wav := filepath.Join(dir, name+"-src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	intact = filepath.Join(dir, name)
	if _, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input:       wav,
		ProcessSpec: ProcessSpec{Output: ToFile(intact), Transcode: &TranscodeSpec{Format: format}},
	}); err != nil {
		t.Fatalf("fixture transcode: %v", err)
	}
	whole, err := os.ReadFile(intact)
	if err != nil {
		t.Fatal(err)
	}
	truncated = filepath.Join(dir, "cut-"+name)
	if err := os.WriteFile(truncated, whole[:len(whole)*6/10], 0o644); err != nil {
		t.Fatal(err)
	}
	return intact, truncated
}

// A damaged local input is transcoded from the part that reads, which is the
// right thing to do and the wrong thing to do silently: the run stays exit 0
// and reports the damage as a warning.
func TestProcessWarnsInputDamage(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		format TranscodeFormat
		ext    string
		detail string // wording each container uses for the damage it tolerated
	}{
		{"wav", FormatWAV, ".wav", "clamped"},
		{"flac", FormatFLAC, ".flac", "declares"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			intact, truncated := damagedFixture(t, dir, "in"+tc.ext, tc.format)

			clean, err := newOfflineClient(t).Process(ctx, ProcessRequest{
				Input:       intact,
				ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "clean.wav")), Transcode: &TranscodeSpec{Format: FormatWAV}},
			})
			if err != nil {
				t.Fatalf("Process on the intact fixture: %v", err)
			}
			if hasWarning(clean, WarnInputDamage) {
				t.Errorf("an undamaged input warned: %+v", clean.Warnings)
			}

			res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
				Input:       truncated,
				ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "out.wav")), Transcode: &TranscodeSpec{Format: FormatWAV}},
			})
			if err != nil {
				t.Fatalf("Process on the damaged fixture: %v, want the readable part delivered", err)
			}
			detail := warningDetail(res, WarnInputDamage)
			if detail == "" {
				t.Fatalf("no %s warning on a truncated input; warnings = %+v", WarnInputDamage, res.Warnings)
			}
			if !strings.Contains(detail, tc.detail) {
				t.Errorf("detail = %q, want it to carry the parser's own note (%q)", detail, tc.detail)
			}
		})
	}
}

// A cut resolves against the length the probe clamped to, not the length the
// header claims: inside it the cut runs, past it the spec is rejected before
// the engine reads a sample, exactly as an undamaged file behaves.
func TestCutOnTruncatedSourcePostClamp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, truncated := damagedFixture(t, dir, "in.flac", FormatFLAC)

	if _, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: truncated,
		ProcessSpec: ProcessSpec{
			Output: ToFile(filepath.Join(dir, "in-range.flac")),
			Cut:    &CutSpec{Ranges: []TimeRange{{Start: 200 * time.Millisecond, End: 500 * time.Millisecond}}},
		},
	}); err != nil {
		t.Fatalf("cut inside the readable part: %v, want it to run", err)
	}

	_, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: truncated,
		ProcessSpec: ProcessSpec{
			Output: ToFile(filepath.Join(dir, "past.flac")),
			Cut:    &CutSpec{Ranges: []TimeRange{{Start: 2500 * time.Millisecond, End: 2900 * time.Millisecond}}},
		},
	})
	if err == nil {
		t.Fatal("a cut past the readable length succeeded, want it rejected against the clamped duration")
	}
	if !strings.Contains(err.Error(), "do not intersect") {
		t.Errorf("err = %v, want the pre-engine rejection naming a non-intersecting range", err)
	}
	// The rejection is where the surprising duration needs explaining: the user
	// asked for a cut that exists in the file the header describes, so the
	// error carries the probe's damage note alongside the clamped duration.
	if !strings.Contains(err.Error(), "declares") {
		t.Errorf("err = %v, want the probe's damage note explaining the short duration", err)
	}
}

// An album folds damage into one warning that names the first damaged file,
// following the aggregation every other ProcessAlbum warning uses: one damaged
// track is identifiable without opening every output, and a batch of damaged
// rips does not bury the summary under one warning per file.
func TestProcessAlbumWarnsInputDamageFolded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	intact, truncated := damagedFixture(t, dir, "track.flac", FormatFLAC)

	albumDamage := func(t *testing.T, tracks []AlbumTrack) []Warning {
		t.Helper()
		res, err := newOfflineClient(t).ProcessAlbum(ctx, tracks, -14, TranscodeSpec{Format: FormatFLAC})
		if err != nil {
			t.Fatalf("ProcessAlbum: %v", err)
		}
		var damage []Warning
		for _, w := range res.Warnings {
			if w.Code == WarnInputDamage {
				damage = append(damage, w)
			}
		}
		return damage
	}

	one := albumDamage(t, []AlbumTrack{
		{Input: intact, Output: filepath.Join(dir, "one.flac")},
		{Input: truncated, Output: filepath.Join(dir, "two.flac")},
	})
	if len(one) != 1 {
		t.Fatalf("got %d input-damage warnings, want exactly 1: %+v", len(one), one)
	}
	if !strings.Contains(one[0].Detail, filepath.Base(truncated)) {
		t.Errorf("detail = %q, want it to name %q", one[0].Detail, filepath.Base(truncated))
	}

	_, truncated2 := damagedFixture(t, dir, "more.flac", FormatFLAC)
	two := albumDamage(t, []AlbumTrack{
		{Input: truncated, Output: filepath.Join(dir, "three.flac")},
		{Input: truncated2, Output: filepath.Join(dir, "four.flac")},
	})
	if len(two) != 1 {
		t.Fatalf("two damaged tracks produced %d warnings, want 1 folded: %+v", len(two), two)
	}
	if !strings.Contains(two[0].Detail, "and 1 more track") {
		t.Errorf("detail = %q, want the fold to count the second track", two[0].Detail)
	}
}

// corruptedFixture returns a FLAC whose middle bytes were overwritten in place:
// the byte count and headers stay intact, so the probe passes it clean, and the
// damage only surfaces when a decode actually reaches it.
func corruptedFixture(t *testing.T, dir string) string {
	t.Helper()
	intact, _ := damagedFixture(t, dir, "in.flac", FormatFLAC)
	whole, err := os.ReadFile(intact)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "corrupt.flac")
	off := len(whole) * 6 / 10
	for i := range 512 {
		whole[off+i] = 0xA5
	}
	if err := os.WriteFile(corrupt, whole, 0o644); err != nil {
		t.Fatal(err)
	}
	return corrupt
}

// Mid-file corruption is invisible to the probe (same byte count, clean
// headers, full declared length), so the decode ending early is the only
// evidence there is. A transcode that silently delivered 60% of the audio at
// exit 0 was the bug; it now succeeds with the damage reported.
func TestProcessWarnsShortDecode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corrupt := corruptedFixture(t, dir)

	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input:       corrupt,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "out.wav")), Transcode: &TranscodeSpec{Format: FormatWAV}},
	})
	if err != nil {
		t.Fatalf("Process: %v, want the readable part delivered", err)
	}
	detail := warningDetail(res, WarnInputDamage)
	if detail == "" {
		t.Fatalf("no %s warning on a corrupt-middle input; warnings = %+v", WarnInputDamage, res.Warnings)
	}
	if !strings.Contains(detail, "did not read") {
		t.Errorf("detail = %q, want it to report the decode ending early", detail)
	}

	clean, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input:       filepath.Join(dir, "in.flac"),
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "clean.wav")), Transcode: &TranscodeSpec{Format: FormatWAV}},
	})
	if err != nil {
		t.Fatalf("Process on the intact fixture: %v", err)
	}
	if hasWarning(clean, WarnInputDamage) {
		t.Errorf("an undamaged input warned: %+v", clean.Warnings)
	}
}

// The measure-only path has no output to compare, but the meter reports how
// much audio it read, which is the same evidence: a measurement over 60% of a
// track is not a measurement of the track.
func TestMeasureOnlyWarnsShortDecode(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corrupt := corruptedFixture(t, dir)

	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input:       corrupt,
		ProcessSpec: ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly, Target: -14}},
	})
	if err != nil {
		t.Fatalf("measure-only Process: %v", err)
	}
	detail := warningDetail(res, WarnInputDamage)
	if detail == "" {
		t.Fatalf("no %s warning on a corrupt-middle measure; warnings = %+v", WarnInputDamage, res.Warnings)
	}
	if !strings.Contains(detail, "measurement covered") {
		t.Errorf("detail = %q, want the measurement wording", detail)
	}
}

// An album refuses a corrupt-middle track rather than warning past it: every
// track shares one gain, and a gain computed over the 60% that decodes would
// normalize the whole record against a number that describes nothing. The
// refusal must name the file, not make the user count timeline members.
func TestProcessAlbumRefusesShortDecodeNamingTrack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	corrupt := corruptedFixture(t, dir)

	_, err := newOfflineClient(t).ProcessAlbum(ctx, []AlbumTrack{
		{Input: filepath.Join(dir, "in.flac"), Output: filepath.Join(dir, "one.flac")},
		{Input: corrupt, Output: filepath.Join(dir, "two.flac")},
	}, -14, TranscodeSpec{Format: FormatFLAC})
	if err == nil {
		t.Fatal("ProcessAlbum succeeded on a track that does not decode to its declared length")
	}
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Errorf("err = %v, want ErrUnsupportedInput", err)
	}
	if !strings.Contains(err.Error(), "corrupt.flac") {
		t.Errorf("err = %q, want it to name corrupt.flac", err)
	}
	if !strings.Contains(err.Error(), "delivered") {
		t.Errorf("err = %q, want the delivered-vs-declared cause kept", err)
	}
}
