package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
)

// warningDetail returns the detail of the first warning carrying code, or "".
func warningDetail(res *Result, code WarningCode) string {
	return warningIn(res.Warnings, code)
}

// warningIn returns the detail of the first warning in ws carrying code, or
// "", for a result type that is not a *Result.
func warningIn(ws []Warning, code WarningCode) string {
	for _, w := range ws {
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
		// The frame-indexed payloads walk lazily: their probe reads the headers
		// clean, and the truncated frame is found by the read that reaches it,
		// which the write reports and the warning carries. Both word it the same
		// way now that ADTS drops a final frame whose declared span runs past
		// the data end instead of indexing it, which is what MP3's walker always
		// did.
		{"mp3", FormatMP3, ".mp3", "truncated final frame"},
		{"aac", FormatAAC, ".aac", "truncated final frame"},
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
			if !strings.HasPrefix(detail, "the input is damaged: ") {
				t.Errorf("detail = %q, want the verdict to lead now that the list holds damage alone", detail)
			}
			if hasWarning(res, WarnInputNote) {
				t.Errorf("a truncated input raised the not-damage note: %+v", res.Warnings)
			}
		})
	}
}

// A measure-only run reads the whole input too, so the damage a lazy walker
// leaves for the read reaches the warning with no output written: a truncated
// ADTS stream declares no length, so nothing else would say it was short.
func TestMeasureOnlyWarnsReadDamage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, truncated := damagedFixture(t, dir, "in.aac", FormatAAC)
	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input:       truncated,
		ProcessSpec: ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly}},
	})
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	detail := warningDetail(res, WarnInputDamage)
	if !strings.HasPrefix(detail, "the input is damaged: ") || !strings.Contains(detail, "truncated") {
		t.Errorf("measure-only damage = %q, want the verdict and the torn frame the read found", detail)
	}
}

// Measure is Process with a measure-only spec, and it reports what that
// read found: a truncated payload the probe reads clean reaches the caller
// as input-damage, and a whole file reports nothing.
func TestMeasureReportsInputDamage(t *testing.T) {
	dir := t.TempDir()
	// "m.aac", not "m": damagedFixture writes the file under the name
	// verbatim, and an extensionless AAC lands in flat MP4, which declares
	// its length and does not have the probes-clean, read-finds-damage
	// shape ADTS has (TestMeasureOnlyWarnsReadDamage uses "in.aac").
	whole, truncated := damagedFixture(t, dir, "m.aac", FormatAAC)
	c := newOfflineClient(t)
	res, err := c.Measure(context.Background(), truncated)
	if err != nil {
		t.Fatal(err)
	}
	if res.Loudness.IntegratedLUFS == 0 {
		t.Errorf("no measurement came back: %+v", res.Loudness)
	}
	if d := warningIn(res.Warnings, WarnInputDamage); !strings.HasPrefix(d, "the input is damaged: ") {
		t.Errorf("input-damage detail = %q, want the damage the read found", d)
	}
	clean, err := c.Measure(context.Background(), whole)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean.Warnings) != 0 {
		t.Errorf("a whole file reports %v, want nothing", clean.Warnings)
	}
}

// The dropped trim reaches the caller as gapless-dropped, once, naming the
// container and the samples; a copy into a container that carries the trim
// raises nothing.
func TestProcessWarnsGaplessDroppedOnACopyIntoADTS(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	// An AAC in progressive MP4, the way damagedFixture writes its formats
	// through the offline client; its edit list states the encoder delay.
	src := filepath.Join(dir, "src.m4a")
	if _, err := c.Process(ctx, ProcessRequest{Input: wav,
		ProcessSpec: ProcessSpec{Output: ToFile(src), Transcode: &TranscodeSpec{Format: FormatAAC}}}); err != nil {
		t.Fatal(err)
	}
	copyTo := func(out string) *Result {
		t.Helper()
		res, err := c.Process(ctx, ProcessRequest{Input: src,
			ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, out)), Transcode: &TranscodeSpec{Format: FormatCopy}}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if d := warningDetail(copyTo("out.aac"), WarnGaplessDropped); !strings.Contains(d, ".aac") || !strings.Contains(d, "encoder delay") {
		t.Errorf("gapless-dropped detail = %q, want the container and the samples named", d)
	}
	if d := warningDetail(copyTo("out.m4a"), WarnGaplessDropped); d != "" {
		t.Errorf("a copy into .m4a warns %q, want nothing", d)
	}
}

// A cut of several spans reads the one source through a timeline, which names
// each span's findings after its member; the warning carries the source's own
// words once, with no member index, whatever the engine's prefix looks like.
// The damage is junk spliced into the middle of an MP3: the frame walk is
// lazy, so the probe passes the file clean and the resync is found by the
// read, and every frame is still there, so no span outruns the file.
func TestCutWarningsCarryNoMemberPrefix(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	intact, _ := damagedFixture(t, dir, "in.mp3", FormatMP3)
	whole, err := os.ReadFile(intact)
	if err != nil {
		t.Fatal(err)
	}
	mid := len(whole) / 2
	spliced := filepath.Join(dir, "spliced.mp3")
	if err := os.WriteFile(spliced, slices.Concat(whole[:mid], make([]byte, 300), whole[mid:]), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := newOfflineClient(t).Process(ctx, ProcessRequest{
		Input: spliced,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(filepath.Join(dir, "cut.wav")),
			Transcode: &TranscodeSpec{Format: FormatWAV},
			Cut: &CutSpec{Ranges: []TimeRange{
				{Start: 100 * time.Millisecond, End: 200 * time.Millisecond},
				{Start: 350 * time.Millisecond, End: 450 * time.Millisecond},
			}},
		},
	})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	detail := warningDetail(res, WarnInputDamage)
	if detail == "" {
		t.Fatalf("no %s warning on a spliced input; warnings = %+v", WarnInputDamage, res.Warnings)
	}
	if !strings.HasPrefix(detail, "the input is damaged: ") || strings.Contains(detail, "member ") {
		t.Errorf("detail = %q, want the verdict then the engine's finding, with no member index", detail)
	}
	finding := strings.TrimPrefix(detail, "the input is damaged: ")
	if first, _, _ := strings.Cut(finding, ";"); strings.Count(finding, first) != 1 {
		t.Errorf("detail = %q, want each finding once across the spans", detail)
	}
}

// A clipped sample on a source whose decode stays in range is the gain's
// doing, so it warns; on a transform codec the decoder's own overshoot makes
// the count meaningless, so it does not.
func TestDecodeOvershootsGatesClipping(t *testing.T) {
	for codec, want := range map[string]bool{
		"opus": true, "mp3": true, "aac": true, "wma": true, "wmapro": true, "musepack": true,
		"alaw": false, "mulaw": false, "ima-adpcm": false, "ms-adpcm": false, "flac": false, "wmalossless": false, "pcm": false,
	} {
		if got := decodeOvershoots(codec); got != want {
			t.Errorf("decodeOvershoots(%q) = %v, want %v", codec, got, want)
		}
	}
	levels := media.Levels{ClippedSamples: 3, Samples: 8000, Channels: 1, Quantized: true}
	for codec, warns := range map[string]bool{"alaw": true, "flac": true, "opus": false} {
		em := newEmitter(nil, "")
		warnOutputClipping(em, nil, pipeline.Result{SourceCodec: codec, Levels: levels})
		if got := len(em.collected()) == 1; got != warns {
			t.Errorf("%s source with clipped samples warned = %v, want %v", codec, got, warns)
		}
	}
}

// The engine's damage and its remarks on a well-formed input go out under
// different codes: the damage with its verdict, the remarks verbatim.
func TestInputRemarksSplitDamageFromNotes(t *testing.T) {
	em := newEmitter(nil, "")
	warnInputDamage(em, pipeline.Result{
		SourceWarnings: []string{"data chunk clamped to the file"},
		SourceNotes:    []string{"no ftyp box", "ignoring a video track"},
	})
	got := em.collected()
	if len(got) != 2 {
		t.Fatalf("warnings = %+v, want one damage and one note", got)
	}
	if got[0].Code != WarnInputDamage || got[0].Detail != "the input is damaged: data chunk clamped to the file" {
		t.Errorf("damage = %+v, want the verdict then the note", got[0])
	}
	if got[1].Code != WarnInputNote || got[1].Detail != "no ftyp box; ignoring a video track" {
		t.Errorf("note = %+v, want the remarks verbatim under input-note", got[1])
	}
	if em := newEmitter(nil, ""); len(func() []Warning { warnInputDamage(em, pipeline.Result{}); return em.collected() }()) != 0 {
		t.Error("a clean input warned")
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
// refusal must name the file, not make the user count members, and it must
// keep the covered-vs-declared cause.
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
	if !strings.Contains(err.Error(), "covered") || !strings.Contains(err.Error(), "declared") {
		t.Errorf("err = %q, want the covered-vs-declared cause kept", err)
	}
}

// A cut of a lazily walked payload resolves against the length a decode
// delivers, not the header's claim: inside it the cut runs, past it the spec
// is rejected before the engine reads a sample, as a truncated FLAC's clamped
// probe already behaves. Before this the engine refused the run as "the source
// ended N samples into a span that declared M" and the CLI called it an I/O
// failure.
func TestCutOnTruncatedLazyPayloadMeasuresFirst(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		format TranscodeFormat
		ext    string
		// damage is the wording the container's own walker uses for the
		// shortfall it found. MP3 has a stale Xing count to contradict as
		// well as a frame to drop; ADTS declared no length to begin with.
		damage string
	}{{"mp3", FormatMP3, ".mp3", "declares"}, {"adts", FormatAAC, ".aac", "truncated final frame"}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, truncated := damagedFixture(t, dir, "in"+tc.ext, tc.format) // 3 s declared, ~1.8 s real
			c := newOfflineClient(t)

			// Remove the head: the kept span runs to the real end.
			res, err := c.Process(ctx, ProcessRequest{Input: truncated, ProcessSpec: ProcessSpec{
				Output:    ToFile(filepath.Join(dir, "tail.flac")),
				Transcode: &TranscodeSpec{Format: FormatFLAC},
				Cut:       &CutSpec{Ranges: []TimeRange{{Start: 0, End: 500 * time.Millisecond}}},
			}})
			if err != nil {
				t.Fatalf("cut to the real end: %v", err)
			}
			if d := res.OutputFormat.Duration; d <= 0 || d > 1800*time.Millisecond {
				t.Errorf("output duration %v, want the readable remainder (about 1.3 s)", d)
			}
			got := warningDetail(res, WarnInputDamage)
			if got == "" {
				t.Error("no input-damage warning for the truncated source")
			}
			if !strings.Contains(got, tc.damage) {
				t.Errorf("input-damage detail = %q, want it to carry %q", got, tc.damage)
			}

			// A span past the real end is rejected up front, naming the damage.
			_, err = c.Process(ctx, ProcessRequest{Input: truncated, ProcessSpec: ProcessSpec{
				Output:    ToFile(filepath.Join(dir, "past.flac")),
				Transcode: &TranscodeSpec{Format: FormatFLAC},
				Cut:       &CutSpec{Ranges: []TimeRange{{Start: 2500 * time.Millisecond, End: 2900 * time.Millisecond}}},
			}})
			if err == nil || !strings.Contains(err.Error(), "do not intersect") || !errors.Is(err, ErrIncompatibleSpec) {
				t.Fatalf("err = %v, want the pre-engine rejection", err)
			}

			// An interior cut composes several spans, whose seam holds each
			// member to the measured count. It runs for both codecs now that
			// the walk of a truncated ADTS no longer counts a frame it cannot
			// read. A normalizing cut takes the same path and no longer flips
			// to exit 2.
			if _, err := c.Process(ctx, ProcessRequest{Input: truncated, ProcessSpec: ProcessSpec{
				Output:    ToFile(filepath.Join(dir, "norm.flac")),
				Transcode: &TranscodeSpec{Format: FormatFLAC},
				Loudness:  &LoudnessSpec{Mode: LoudnessApply, Target: -16},
				Cut:       &CutSpec{Ranges: []TimeRange{{Start: 200 * time.Millisecond, End: 400 * time.Millisecond}}},
			}}); err != nil {
				t.Fatalf("normalizing cut: %v", err)
			}
		})
	}
}

// The numbers the run reports describe the file it read, not the length the
// header claimed: a truncated source's declared total would put a 3 s source
// duration and a 500 ms removal beside a 1.3 s output.
func TestCutOnTruncatedLazyPayloadReportsMeasuredNumbers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, truncated := damagedFixture(t, dir, "in.mp3", FormatMP3)

	pres, err := pipeline.Run(ctx, media.NewRunner(media.RunnerConfig{}), truncated, filepath.Join(dir, "tail.flac"), pipeline.Spec{
		Remove: []cutrange.Range{{Start: 0, End: 500 * time.Millisecond}},
		Codec:  media.CodecFLAC,
	}, nil)
	if err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	if pres.SourceDuration <= 0 || pres.SourceDuration > 2*time.Second {
		t.Errorf("SourceDuration = %v, want the measured length (about 1.8 s), not the declared 3 s", pres.SourceDuration)
	}
	kept := pres.SourceDuration - pres.Removed
	out, err := media.NewRunner(media.RunnerConfig{}).Probe(ctx, pres.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if d := (kept - out.Format.Duration).Abs(); d > 60*time.Millisecond {
		t.Errorf("source %v less removed %v = %v, want the delivered %v", pres.SourceDuration, pres.Removed, kept, out.Format.Duration)
	}
}
