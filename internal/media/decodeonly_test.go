package media

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// TestDecoderRegistryParity pins the two codec tables to the engine's decoder
// registry: every codec WaxFlow decodes is either remuxable (codecToFormat) or
// decode-only (decodeOnlyCodecs), never both and never neither, PCM excepted
// (its packets take their layout from the container, so no copy survives and
// remuxDeclined has its own wording; TestPCMRemuxDeclined). A decoder
// registered upstream that lands nowhere fails here, so it gets classified on
// purpose (the lossy tables, the batch walk, the refusal wording) rather than
// declining by default.
func TestDecoderRegistryParity(t *testing.T) {
	seen := map[string]bool{}
	for _, id := range format.Decoders() {
		name := codecName(id)
		seen[name] = true
		_, remux := codecToFormat(id)
		_, decodeOnly := DecodeOnlyCodec(name)
		switch {
		case id == codec.PCM:
			if remux || decodeOnly {
				t.Errorf("pcm: remuxable=%v decode-only=%v; PCM is neither, by its own rule", remux, decodeOnly)
			}
		case remux == decodeOnly:
			t.Errorf("the engine decodes %q: remuxable=%v decode-only=%v; a decoder is exactly one of the two, decide and add it", name, remux, decodeOnly)
		}
	}
	for name := range decodeOnlyCodecs {
		if !seen[name] {
			t.Errorf("decodeOnlyCodecs names %q, which the engine no longer decodes; drop the entry", name)
		}
	}
}

// Every extension WaxFlow registers for a container it only reads is refused
// as an output name by every gate in this package, whatever the codec, rather
// than force-muxed: FLAC bytes under out.mpp would be a file every player
// reads as Musepack. The list itself is pinned by hand to WaxFlow's
// format/registry.go, which exports no extension list.
func TestDecodeOnlyOutputExtensionsRejected(t *testing.T) {
	exts := DecodeOnlyExts()
	if want := []string{"asf", "mp+", "mpc", "mpp", "wma"}; !slices.Equal(exts, want) {
		t.Fatalf("DecodeOnlyExts() = %v, want the engine's spellings %v", exts, want)
	}
	if name, ok := DecodeOnlyContainer(".MPC"); !ok || name != "Musepack" {
		t.Errorf("DecodeOnlyContainer(\".MPC\") = %q, %v; want Musepack, true (dotted, any case)", name, ok)
	}
	if _, ok := DecodeOnlyContainer("flac"); ok {
		t.Error("DecodeOnlyContainer(\"flac\") = true")
	}
	for _, ext := range exts {
		out := "out." + ext
		if needsForcedMuxer(out) {
			t.Errorf("needsForcedMuxer(%q) = true; the extension must constrain the output, not be muxed over", out)
		}
		for _, name := range []string{"flac", "aac", "opus", "pcm", "wma", "musepack"} {
			if ContainerAccepts(ext, name) {
				t.Errorf("ContainerAccepts(%q, %q) = true, want false: nothing WaxTap writes may carry the name", ext, name)
			}
		}
		for _, c := range []Codec{CodecFLAC, CodecMP3, CodecOpus, CodecWAV} {
			err := CheckOutputContainer(c, out)
			if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") {
				t.Errorf("CheckOutputContainer(%v, %q) = %v, want ErrIncompatibleSpec naming the read-only container", c, out, err)
			}
		}
	}
}

// mpcFixture writes the tagged Musepack fixture into dir and returns its path;
// chapteredMPCFixture and wmaFixture do the same for the other two decode-only
// fixtures, each under its own name so one dir can hold all three.
func mpcFixture(t *testing.T, dir string) string {
	return writeFixture(t, dir, "in.mpc", mediatest.TaggedMPC())
}

func chapteredMPCFixture(t *testing.T, dir string) string {
	return writeFixture(t, dir, "chapters.mpc", mediatest.ChapteredMPC())
}

func wmaFixture(t *testing.T, dir string) string {
	return writeFixture(t, dir, "in.wma", mediatest.ChapteredWMA())
}

func writeFixture(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	in := filepath.Join(dir, name)
	if err := os.WriteFile(in, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return in
}

// A Musepack source rides the ordinary decode path: the probe names it, its
// APEv2 tag reaches the probe, a lossless encode delivers every sample, and
// the two requests that would need a Musepack writer (a remux, and a copy-cut
// with no same-family encoder to fall back to) decline in WaxTap's own words
// before the engine is asked for a format it does not have.
func TestMusepackSourceDecodesAndDeclinesCopy(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	ctx := context.Background()
	dir := t.TempDir()
	in := mpcFixture(t, dir)

	pr := mustProbe(t, r, in)
	a, _ := pr.AudioStream()
	if a.CodecName != "musepack" || pr.Format.Container != "musepack" {
		t.Errorf("probe = codec %q in %q, want musepack in musepack", a.CodecName, pr.Format.Container)
	}
	if a.Samples != mediatest.TaggedMPCSamples || a.SampleRate != mediatest.TaggedMPCRate || a.Channels != 2 {
		t.Errorf("stream = %+v, want %d samples at %d Hz stereo", a, mediatest.TaggedMPCSamples, mediatest.TaggedMPCRate)
	}
	for key, want := range mediatest.TaggedMPCTags {
		if got := tagValue(pr, key); got != want {
			t.Errorf("probe tag %s = %q, want %q", key, got, want)
		}
	}

	out := filepath.Join(dir, "out.flac")
	if _, err := r.Transcode(ctx, in, out, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	if o, _ := mustProbe(t, r, out).AudioStream(); o.CodecName != "flac" || o.Samples != mediatest.TaggedMPCSamples {
		t.Errorf("output = %+v, want flac holding every source sample", o)
	}

	_, err := r.Transcode(ctx, in, filepath.Join(dir, "copy.out"), Spec{Codec: CodecCopy})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "cannot remux musepack audio") || !strings.Contains(err.Error(), "pass --format") {
		t.Errorf("remux = %v, want ErrIncompatibleSpec naming musepack, the reason, and the escape", err)
	}

	cut := CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: 100 * time.Millisecond}},
		Total:   mediatest.TaggedMPCDuration,
		CopyCut: true,
		Encode:  Spec{Codec: CodecCopy},
	}
	_, err = r.Render(ctx, in, filepath.Join(dir, "cut.mka"), cut)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "pass an explicit format") {
		t.Errorf("copy-cut = %v, want ErrIncompatibleSpec naming the --format escape", err)
	}
	cut.Encode = Spec{Codec: CodecFLAC}
	res, err := r.Render(ctx, in, filepath.Join(dir, "cut.flac"), cut)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if res.Mode != ModeAccurate {
		t.Errorf("cut mode = %v, want ModeAccurate (Musepack has no packet cut)", res.Mode)
	}
}

// A WMA source rides the ordinary decode path, the shape
// TestMusepackSourceDecodesAndDeclinesCopy pins for the other decode-only
// codec: the probe names it, its tags reach the probe, a lossless encode
// delivers every sample, and the two requests that would need a WMA writer (a
// remux, and a copy-cut with no same-family encoder to fall back to) decline
// in WaxTap's own words before the engine is asked for a format it does not
// have.
func TestWMASourceDecodesAndDeclinesCopy(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	ctx := context.Background()
	dir := t.TempDir()
	in := wmaFixture(t, dir)

	pr := mustProbe(t, r, in)
	a, _ := pr.AudioStream()
	if a.CodecName != "wma" || pr.Format.Container != "wma" {
		t.Errorf("probe = codec %q in %q, want wma in wma", a.CodecName, pr.Format.Container)
	}
	if a.Samples != mediatest.ChapteredWMASamples || a.SampleRate != mediatest.ChapteredWMARate || a.Channels != 1 {
		t.Errorf("stream = %+v, want %d samples at %d Hz mono", a, mediatest.ChapteredWMASamples, mediatest.ChapteredWMARate)
	}
	for key, want := range mediatest.ChapteredWMATags {
		if got := tagValue(pr, key); got != want {
			t.Errorf("probe tag %s = %q, want %q", key, got, want)
		}
	}

	out := filepath.Join(dir, "out.flac")
	if _, err := r.Transcode(ctx, in, out, Spec{Codec: CodecFLAC}); err != nil {
		t.Fatalf("transcode: %v", err)
	}
	// WMA carries no padding count and ASF's play duration is a millisecond
	// figure, so the engine delivers the decode's whole last frame rather
	// than trimming to a length it cannot trust (WaxFlow's codec/wma Drain):
	// the output holds every source sample and at most a frame past them.
	o, _ := mustProbe(t, r, out).AudioStream()
	if o.CodecName != "flac" || o.Samples < mediatest.ChapteredWMASamples || o.Samples > mediatest.ChapteredWMASamples+mediatest.ChapteredWMAFrame {
		t.Errorf("output = %+v, want flac holding every source sample and at most a %d-sample frame past them", o, mediatest.ChapteredWMAFrame)
	}

	_, err := r.Transcode(ctx, in, filepath.Join(dir, "copy.out"), Spec{Codec: CodecCopy})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "cannot remux wma audio") || !strings.Contains(err.Error(), "pass --format") {
		t.Errorf("remux = %v, want ErrIncompatibleSpec naming wma, the reason, and the escape", err)
	}

	cut := CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: 500 * time.Millisecond}},
		Total:   mediatest.ChapteredWMADuration,
		CopyCut: true,
		Encode:  Spec{Codec: CodecCopy},
	}
	_, err = r.Render(ctx, in, filepath.Join(dir, "cut.mka"), cut)
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), "pass an explicit format") {
		t.Errorf("copy-cut = %v, want ErrIncompatibleSpec naming the --format escape", err)
	}
	cut.Encode = Spec{Codec: CodecFLAC}
	res, err := r.Render(ctx, in, filepath.Join(dir, "cut.flac"), cut)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if res.Mode != ModeAccurate {
		t.Errorf("cut mode = %v, want ModeAccurate (WMA has no packet cut)", res.Mode)
	}
}

// The read-side half of the invariant TestWaxLabelReadsEveryEngineOutput pins:
// carryTags parses a source through WaxLabel alone, so the decode-only inputs
// must be ones WaxLabel identifies too. Each fixture is pinned with the
// metadata its carry rests on: the APEv2 tag, the SV8 chapter packets, the
// ASF title and Marker Object.
func TestWaxLabelReadsDecodeOnlyInputs(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name     string
		path     string
		format   waxlabel.Format
		title    string
		chapters int
	}{
		{"tagged mpc", mpcFixture(t, dir), waxlabel.FormatMusepack, mediatest.TaggedMPCTags["TITLE"], 0},
		{"chaptered mpc", chapteredMPCFixture(t, dir), waxlabel.FormatMusepack, "", len(mediatest.ChapteredMPCChapters())},
		{"chaptered wma", wmaFixture(t, dir), waxlabel.FormatWMA, mediatest.ChapteredWMATags["TITLE"], len(mediatest.ChapteredWMAChapters())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := waxlabel.ParseFile(context.Background(), tc.path)
			if err != nil {
				t.Fatalf("WaxLabel cannot parse the %s fixture the engine decodes: %v", tc.format, err)
			}
			if doc.Format() != tc.format {
				t.Errorf("format = %v, want %v", doc.Format(), tc.format)
			}
			if got, _ := doc.Get(tag.Title); strings.Join(got, "\x00") != tc.title {
				t.Errorf("TITLE = %q, want %q", got, tc.title)
			}
			if n := len(doc.Chapters()); n != tc.chapters {
				t.Errorf("chapters = %d, want %d", n, tc.chapters)
			}
		})
	}
}

// The decode-only codecs that arrive inside containers this package writes:
// the codec alone is read-only, and the refusal names it. A byte-linear
// payload counts its own length, so the G.711 decode delivers exactly what
// the probe declared; WMA Lossless sits in ASF, which states a rounded
// duration for a codec with no padding count, so its decode runs to the frame
// boundary past the declared count.
func TestDecodeOnlyCodecsInsideWritableContainers(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	ctx := context.Background()
	dir := t.TempDir()
	for _, tc := range []struct {
		name, codec, display string
		data                 []byte
		rate, channels       int
		samples, tail        int64
	}{
		{"alaw.wav", "alaw", "G.711 A-law", mediatest.ALawWAV(), mediatest.ALawWAVRate, 1, mediatest.ALawWAVSamples, 0},
		{"lossless.wma", "wmalossless", "WMA Lossless", mediatest.LosslessWMA(), mediatest.LosslessWMARate, mediatest.LosslessWMAChannels, mediatest.LosslessWMASamples, mediatest.LosslessWMAFrame},
	} {
		t.Run(tc.codec, func(t *testing.T) {
			in := writeFixture(t, dir, tc.name, tc.data)
			pr, err := r.Probe(ctx, in)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			a, _ := pr.AudioStream()
			if a.CodecName != tc.codec || a.SampleRate != tc.rate || a.Channels != tc.channels || a.Samples != tc.samples {
				t.Errorf("probe = %+v, want %s %d Hz %d ch %d samples", a, tc.codec, tc.rate, tc.channels, tc.samples)
			}
			if len(pr.Warnings) != 0 || len(pr.Notes) != 0 {
				t.Errorf("probe warnings %v notes %v, want none on a clean fixture", pr.Warnings, pr.Notes)
			}
			if display, ok := DecodeOnlyCodec(a.CodecName); !ok || display != tc.display {
				t.Errorf("DecodeOnlyCodec(%q) = %q, %v; want %q, true", a.CodecName, display, ok, tc.display)
			}
			_, err = r.Transcode(ctx, in, filepath.Join(dir, tc.codec+"-copy.mka"), Spec{Codec: CodecCopy})
			if !errors.Is(err, waxerr.ErrIncompatibleSpec) || !strings.Contains(err.Error(), tc.display) || !strings.Contains(err.Error(), "--format") {
				t.Errorf("copy = %v, want ErrIncompatibleSpec naming %s and the --format escape", err, tc.display)
			}
			// -decoded, not tc.codec+".wav": that collides with the alaw fixture's
			// own name, and a transcode onto its open source is refused outright on
			// Windows (and by Process everywhere).
			res, err := r.Transcode(ctx, in, filepath.Join(dir, tc.codec+"-decoded.wav"), Spec{Codec: CodecWAV})
			if err != nil {
				t.Fatalf("transcode: %v", err)
			}
			if got := res.Levels.Samples; got < tc.samples || got > tc.samples+tc.tail {
				t.Errorf("decode delivered %d frames, want %d to %d", got, tc.samples, tc.samples+tc.tail)
			}
			if len(res.InputWarnings) != 0 {
				t.Errorf("decode found damage %v on a clean fixture", res.InputWarnings)
			}
		})
	}
}

// An MP3 carried in a WAV is the one decode-only-in-a-writable-container
// case that copies: the frames move into a bare .mp3 packet for packet, where
// the LAME tag's own trims then apply (the WAV applied none, and says so in a
// note that is not damage). The WAV's own length is advisory, the fact
// chunk's count of everything the frames decode to.
func TestMP3InWAVCopiesOutToBareMP3(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	ctx := context.Background()
	dir := t.TempDir()
	in := writeFixture(t, dir, "frames.wav", mediatest.MP3WAV())
	pr, err := r.Probe(ctx, in)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	a, _ := pr.AudioStream()
	if pr.Format.Container != "wav" || a.CodecName != "mp3" || a.SampleRate != mediatest.MP3WAVRate || a.Samples != mediatest.MP3WAVSamples {
		t.Errorf("probe = container %s stream %+v, want mp3 in wav at %d Hz declaring %d samples", pr.Format.Container, a, mediatest.MP3WAVRate, mediatest.MP3WAVSamples)
	}
	if len(pr.Warnings) != 0 || len(pr.Notes) != 1 || !strings.Contains(pr.Notes[0], "nCodecDelay") {
		t.Errorf("probe warnings %v notes %v, want the nCodecDelay remark as the one note and no damage", pr.Warnings, pr.Notes)
	}
	if _, decodeOnly := DecodeOnlyCodec(a.CodecName); decodeOnly {
		t.Error("mp3 classified decode-only; its frames copy and re-encode")
	}

	out := filepath.Join(dir, "frames.mp3")
	if _, err := r.Transcode(ctx, in, out, Spec{Codec: CodecCopy}); err != nil {
		t.Fatalf("copy out: %v", err)
	}
	op, err := r.Probe(ctx, out)
	if err != nil {
		t.Fatalf("probe copy: %v", err)
	}
	oa, _ := op.AudioStream()
	if op.Format.Container != "mp3" || oa.CodecName != "mp3" || oa.Samples != mediatest.MP3WAVFrameSamples {
		t.Errorf("copied-out file = container %s stream %+v, want a bare mp3 declaring %d samples (the LAME trims applied)", op.Format.Container, oa, mediatest.MP3WAVFrameSamples)
	}

	res, err := r.Transcode(ctx, in, filepath.Join(dir, "frames-decoded.wav"), Spec{Codec: CodecWAV})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Levels.Samples != mediatest.MP3WAVSamples {
		t.Errorf("decode of the WAV delivered %d frames, want the untrimmed %d", res.Levels.Samples, mediatest.MP3WAVSamples)
	}
}

// The probe keeps the engine's damage and its remarks on a well-formed file
// apart, and a result's damage list drops the member index a timeline
// prefixes, once per finding.
func TestProbeAndResultWarningShapes(t *testing.T) {
	pr := mapProbe(&format.Info{Container: "wav", Warnings: []string{"data chunk clamped"}, Notes: []string{"no ftyp box"}}, 0)
	if !slices.Equal(pr.Warnings, []string{"data chunk clamped"}) || !slices.Equal(pr.Notes, []string{"no ftyp box"}) {
		t.Errorf("mapProbe warnings %v notes %v, want the two lists kept apart", pr.Warnings, pr.Notes)
	}
	got := sourceWarnings([]string{"member 0: truncated final frame dropped (offset 9)", "member 1: truncated final frame dropped (offset 9)", "member 12: bad crc", "plain"})
	if want := []string{"truncated final frame dropped (offset 9)", "bad crc", "plain"}; !slices.Equal(got, want) {
		t.Errorf("sourceWarnings = %v, want %v", got, want)
	}
	if sourceWarnings(nil) != nil {
		t.Error("sourceWarnings(nil) is not nil")
	}
}

// A measurement reads the whole file, so it finds the damage a lazy walker
// leaves for the read: a truncated ADTS stream declares no length and probes
// clean, and only the read that reaches the torn frame can say so.
func TestAnalyzeFileReportsReadDamage(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	ctx := context.Background()
	dir := t.TempDir()
	wav := writeFixture(t, dir, "src.wav", mediatest.SineWAV(3, 2))
	whole := filepath.Join(dir, "whole.aac")
	if _, err := r.Transcode(ctx, wav, whole, Spec{Codec: CodecAAC}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(whole)
	if err != nil {
		t.Fatal(err)
	}
	torn := writeFixture(t, dir, "torn.aac", data[:len(data)*6/10])
	if pr, err := r.Probe(ctx, torn); err != nil || len(pr.Warnings) != 0 {
		t.Fatalf("probe of the torn stream: warnings %v, err %v; want a clean probe, the damage lies past the headers", pr.Warnings, err)
	}
	res, err := r.AnalyzeFile(ctx, torn, 0)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if found := res.InputWarnings; len(found) != 1 || !strings.Contains(found[0], "truncated") {
		t.Errorf("AnalyzeFile damage = %v, want the torn final frame reported once", found)
	} else if strings.HasPrefix(found[0], "member ") {
		t.Errorf("the damage %q still carries a member prefix", found[0])
	}
	if clean, err := r.AnalyzeFile(ctx, wav, 0); err != nil || len(clean.InputWarnings) != 0 {
		t.Errorf("AnalyzeFile on a clean file: damage %v, err %v", clean.InputWarnings, err)
	}
}

// A member whose headers only estimate its length is measured to its end,
// with no declaration to hold it to: the group measurement decodes each
// member itself. The WMA fixture is the advisory case (ASF has no walk, so
// MeasureLength decodes it), and the group's count for it is what that decode
// delivers.
func TestAnalyzeGroupMeasuresAnAdvisoryMember(t *testing.T) {
	r := NewRunner(RunnerConfig{MaxProcs: 1})
	ctx := context.Background()
	dir := t.TempDir()
	wma := writeFixture(t, dir, "in.wma", mediatest.LosslessWMA())
	wav := writeFixture(t, dir, "in.wav", mediatest.SineWAV(1, 2))

	length, err := r.MeasureLength(ctx, wma)
	if err != nil {
		t.Fatalf("measure the member: %v", err)
	}
	group, members, err := r.AnalyzeGroup(ctx, []string{wma, wav}, nil)
	if err != nil {
		t.Fatalf("AnalyzeGroup: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	if members[0].Samples != length.Samples {
		t.Errorf("the advisory member measured %d frames, want the %d its own decode delivers", members[0].Samples, length.Samples)
	}
	if math.IsNaN(group.IntegratedLUFS) {
		t.Errorf("group loudness = %v, want a figure", group.IntegratedLUFS)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := r.AnalyzeGroup(canceled, []string{wma, wav}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("AnalyzeGroup under a canceled context = %v, want context.Canceled", err)
	}
}
