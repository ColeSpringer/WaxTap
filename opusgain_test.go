package waxtap

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// toLoudness rebuilds the package's measurement from the public figures, so a
// test can ask GainFor what the pipeline asked it.
func toLoudness(l LoudnessInfo) loudness.Loudness {
	return loudness.Loudness{IntegratedLUFS: l.IntegratedLUFS, TruePeakDBTP: l.TruePeakDBTP, LRA: l.LRA, SamplePeakDB: l.SamplePeakDB}
}

// opusFixture encodes a synthetic tone to an Opus file at name and returns it.
func opusFixture(t *testing.T, c *Client, dir, name string) string {
	t.Helper()
	wav := filepath.Join(dir, name+".wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name+".opus")
	if _, err := c.Process(context.Background(), ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
	}}); err != nil {
		t.Fatalf("opus fixture: %v", err)
	}
	return out
}

// An Opus source that stays Opus under cap carries its gain in the OpusHead:
// the packets are copied, the header says the gain, and WaxFlow's decoder
// applies it, so the delivered measurement reads the normalized loudness.
func TestNormalizeOpusWritesHeaderGain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	src := opusFixture(t, c, dir, "src")

	out := filepath.Join(dir, "norm.opus")
	res, err := c.Process(ctx, ProcessRequest{Input: src, ProcessSpec: ProcessSpec{
		Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
		Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Transcoded || !res.LoudnessApplied || !res.Loudness.HeaderGain {
		t.Fatalf("Transcoded=%v LoudnessApplied=%v HeaderGain=%v, want a header-gain remux", res.Transcoded, res.LoudnessApplied, res.Loudness.HeaderGain)
	}
	want := loudness.GainFor(-20, toLoudness(*res.Loudness.Input))
	if math.Abs(res.Loudness.GainDB-want) > 0.004 {
		t.Errorf("GainDB = %.4f, want %.4f within a Q7.8 step", res.Loudness.GainDB, want)
	}
	q, err := media.OpusHeaderGain(ctx, out)
	if err != nil || q != media.OpusGainQ78(want) {
		t.Errorf("header gain %d (%v), want %d", q, err, media.OpusGainQ78(want))
	}
	if got := res.Loudness.Output.IntegratedLUFS; math.Abs(got-(res.Loudness.Input.IntegratedLUFS+res.Loudness.GainDB)) > 0.3 {
		t.Errorf("delivered %.2f LUFS, want input %.2f + gain %.2f", got, res.Loudness.Input.IntegratedLUFS, res.Loudness.GainDB)
	}
	if res.Loudness.Output.IntegratedLUFS > -19.5 || res.Loudness.Output.IntegratedLUFS < -20.5 {
		t.Errorf("delivered %.2f LUFS, want the -20 target", res.Loudness.Output.IntegratedLUFS)
	}
}

// Everything that reshapes samples re-encodes as before: the limiter, an
// explicit bitrate, and a cut.
func TestNormalizeOpusReEncodesWhenTheGainCannotRide(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	src := opusFixture(t, c, dir, "src")

	for _, tc := range []struct {
		name string
		spec ProcessSpec
	}{
		{"limit", ProcessSpec{Transcode: &TranscodeSpec{Format: FormatOpus}, Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20, PeakMode: PeakLimit}}},
		{"bitrate", ProcessSpec{Transcode: &TranscodeSpec{Format: FormatOpus, Bitrate: 96000}, Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20}}},
		{"cut", ProcessSpec{Transcode: &TranscodeSpec{Format: FormatOpus}, Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20}, Cut: &CutSpec{Ranges: []TimeRange{{Start: 0, End: 500 * 1e6}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			spec.Output = ToFile(filepath.Join(dir, tc.name+".opus"))
			res, err := c.Process(ctx, ProcessRequest{Input: src, ProcessSpec: spec})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Transcoded || res.Loudness.HeaderGain {
				t.Errorf("Transcoded=%v HeaderGain=%v, want a re-encode", res.Transcoded, res.Loudness.HeaderGain)
			}
		})
	}
}

// The head rides in every container that carries one, not only Ogg.
func TestNormalizeOpusHeaderGainInMatroskaContainers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	src := opusFixture(t, c, dir, "src")

	for _, name := range []string{"norm.webm", "norm.mka"} {
		out := filepath.Join(dir, name)
		res, err := c.Process(ctx, ProcessRequest{Input: src, ProcessSpec: ProcessSpec{
			Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
			Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20},
		}})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Transcoded || !res.Loudness.HeaderGain {
			t.Errorf("%s: Transcoded=%v HeaderGain=%v, want a header-gain remux", name, res.Transcoded, res.Loudness.HeaderGain)
		}
		q, err := media.OpusHeaderGain(ctx, out)
		if err != nil || q != media.OpusGainQ78(res.Loudness.GainDB) {
			t.Errorf("%s: header gain %d (%v), want %d", name, q, err, media.OpusGainQ78(res.Loudness.GainDB))
		}
	}
}

// A second normalization to the same target moves the head by what is left,
// which is nothing: the measurement of the source already heard the gain the
// head states, so the run adds to it rather than replacing it.
func TestNormalizeOpusAddsToTheSourcesOwnHeaderGain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	src := opusFixture(t, c, dir, "src")

	first := filepath.Join(dir, "first.opus")
	one, err := c.Process(ctx, ProcessRequest{Input: src, ProcessSpec: ProcessSpec{
		Output: ToFile(first), Transcode: &TranscodeSpec{Format: FormatOpus},
		Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20},
	}})
	if err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "second.opus")
	two, err := c.Process(ctx, ProcessRequest{Input: first, ProcessSpec: ProcessSpec{
		Output: ToFile(second), Transcode: &TranscodeSpec{Format: FormatOpus},
		Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(two.Loudness.GainDB) > 0.05 {
		t.Errorf("a second pass moved the gain by %.3f dB, want about 0", two.Loudness.GainDB)
	}
	q1, err := media.OpusHeaderGain(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := media.OpusHeaderGain(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if diff := q2 - q1; diff > 13 || diff < -13 {
		t.Errorf("head moved from %d to %d, want it held within a rounding step of the first pass (%.3f dB)", q1, q2, one.Loudness.GainDB)
	}
}

// An unmeasurable source derives no gain, so it is delivered as a plain copy
// with nothing written to the head.
func TestNormalizeOpusUnmeasurableStaysACopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	wav := filepath.Join(dir, "silence.wav")
	if err := os.WriteFile(wav, mediatest.SilenceWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "silence.opus")
	if _, err := c.Process(ctx, ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(src), Transcode: &TranscodeSpec{Format: FormatOpus},
	}}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "norm.opus")
	res, err := c.Process(ctx, ProcessRequest{Input: src, ProcessSpec: ProcessSpec{
		Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
		Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.LoudnessApplied || res.Loudness.HeaderGain || res.Loudness.GainDB != 0 {
		t.Errorf("LoudnessApplied=%v HeaderGain=%v GainDB=%v, want an untouched copy", res.LoudnessApplied, res.Loudness.HeaderGain, res.Loudness.GainDB)
	}
	if q, err := media.OpusHeaderGain(ctx, out); err != nil || q != 0 {
		t.Errorf("header gain %d (%v), want 0", q, err)
	}
}

// The packets did not change, so the tags that describe them are kept; the
// gain tags are not, since the head now carries a gain they would restate
// against a loudness that no longer exists.
func TestNormalizeOpusCarriesOwnAudioTagsButNotTheGains(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	src := opusFixture(t, c, dir, "src")
	if err := mediatest.TagFile(ctx, src, "ENCODER", "x", "REPLAYGAIN_TRACK_GAIN", "-5.00 dB"); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "norm.opus")
	if _, err := c.Process(ctx, ProcessRequest{Input: src, ProcessSpec: ProcessSpec{
		Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
		Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -20},
	}}); err != nil {
		t.Fatal(err)
	}
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := doc.Get(tag.Encoder); !ok || len(v) == 0 || v[0] != "x" {
		t.Errorf("the encoder stamp = %q (%v), want it carried", v, ok)
	}
	if v, ok := doc.Get(tag.ReplayGainTrackGain); ok {
		t.Errorf("REPLAYGAIN_TRACK_GAIN = %q, want it dropped: the head now carries the gain", v)
	}
}

// A source already on target quantizes to a zero step, so nothing is written
// to the head and the file is a plain copy. Its own ReplayGain tags still
// describe it exactly, so they stay: claiming a header gain would drop them
// for a file nothing changed.
func TestNormalizeOpusZeroGainKeepsTheGainTags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	src := opusFixture(t, c, dir, "src")

	// Normalize once to learn where the source sits, then aim at that.
	measured, err := c.Measure(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := mediatest.TagFile(ctx, src, "REPLAYGAIN_TRACK_GAIN", "-5.00 dB"); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "same.opus")
	res, err := c.Process(ctx, ProcessRequest{Input: src, ProcessSpec: ProcessSpec{
		Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
		Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: measured.IntegratedLUFS},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Loudness.HeaderGain || res.Loudness.GainDB != 0 {
		t.Fatalf("HeaderGain=%v GainDB=%v, want no header gain for a source already on target", res.Loudness.HeaderGain, res.Loudness.GainDB)
	}
	if q, qerr := media.OpusHeaderGain(ctx, out); qerr != nil || q != 0 {
		t.Errorf("header gain %d (%v), want it untouched", q, qerr)
	}
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := doc.Get(tag.ReplayGainTrackGain); !ok || len(v) == 0 {
		t.Errorf("REPLAYGAIN_TRACK_GAIN = %v, want it kept: the audio and its loudness are unchanged", v)
	}
}
