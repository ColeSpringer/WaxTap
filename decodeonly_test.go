package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// The decode-only inputs (Musepack, WMA) through the facade: each decodes to
// the requested format, its metadata rides the ordinary carry pass into the
// output, and neither can be written.

// A Musepack input is a local source like any other lossy one: it decodes to
// the requested format, its APEv2 metadata rides the ordinary carry pass into
// the output, and nothing about the promotion warns (the source was lossy
// already). What it cannot be is written: a copy and an output carrying its
// extension both fail as incompatible specs, exit 2 at the CLI, before any
// engine call.
func TestProcessMusepackSource(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mpc")
	if err := os.WriteFile(in, mediatest.TaggedMPC(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out.flac")
	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.SourceFormat.Codec != "musepack" || res.SourceFormat.Extension != "mpc" {
		t.Errorf("source format = %+v, want musepack/mpc", res.SourceFormat)
	}
	if res.OutputFormat.Codec != "flac" || !res.Transcoded {
		t.Errorf("output format = %+v transcoded %v, want a flac encode", res.OutputFormat, res.Transcoded)
	}
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	for key, want := range mediatest.TaggedMPCTags {
		k, err := tag.ParseKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := doc.Get(k); !ok || got[0] != want {
			t.Errorf("carried %s = %v (ok %v), want %q", key, got, ok, want)
		}
	}

	// The promotion a cut into a foreign container makes is the shape that
	// warns for a lossless source (TestProcessWarnsImplicitLossyPromotion); a
	// source that was lossy already, which is what lossySource says of
	// musepack, must take the same Opus re-encode without the warning.
	cutOut := filepath.Join(dir, "cut.mka")
	res, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(cutOut), Cut: &CutSpec{Ranges: []TimeRange{{Start: 0, End: 50 * time.Millisecond}}}},
	})
	if err != nil {
		t.Fatalf("cut into .mka: %v", err)
	}
	if res.OutputFormat.Codec != "opus" || !res.Transcoded {
		t.Errorf("cut into .mka = %+v transcoded %v, want the container's Opus promotion", res.OutputFormat, res.Transcoded)
	}
	for _, w := range res.Warnings {
		if w.Code == WarnImplicitLossy {
			t.Errorf("warnings = %v; a source that was lossy already must not warn implicit-lossy", res.Warnings)
		}
	}

	// Keeping the packets under the source's own name is the natural copy
	// request, and the one Matroska's allowlist would not have caught: it is
	// refused because nothing writes Musepack, and the message says so.
	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "copy.mpc")), Transcode: &TranscodeSpec{Format: FormatCopy}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") || !strings.Contains(err.Error(), "--format") {
		t.Errorf("copy = %v, want ErrIncompatibleSpec naming the decode-only source and the --format escape", err)
	}
	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "out.mpc")), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("flac under .mpc = %v, want ErrIncompatibleSpec (the extension is refused, not force-muxed)", err)
	}
}

// A chaptered Musepack source carries its SV8 chapter packets, which nothing
// but the reference editor writes and WaxLabel reads since v1.6.2, onto an
// output that stores chapters, and a cut remaps them like any other carried
// chapter set: a chapter whose content the cut removed is dropped, the rest
// shift by the audio removed before them, and the start-only form (End zero,
// the untitled last chapter included) survives the remap. The items a
// chapter tag carries beside its title (Middle has an artist and a track
// number) are the chapter's metadata, not the file's, and must not surface
// as output tags.
func TestProcessCarriesMusepackChapters(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mpc")
	if err := os.WriteFile(in, mediatest.ChapteredMPC(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out.flac")
	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a whole chapter carry warned: %+v", res.Warnings)
	}
	doc := parseOutput(t, out)
	assertChapters(t, "transcode", doc.Chapters(), mediatest.ChapteredMPCChapters())
	if n := doc.Tags().Len(); n != 0 {
		t.Errorf("output carries %d tags, want none: a chapter tag's items are the chapter's, not the file's", n)
	}

	// Removing the first 200 ms erases Intro (0 to 181 ms) entirely and
	// shifts the rest by 200 ms, Middle clamped to the new start.
	cutOut := filepath.Join(dir, "cut.flac")
	res, err = c.Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(cutOut),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 0, End: 200 * time.Millisecond}}},
		},
	})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a remapped chapter carry warned: %+v", res.Warnings)
	}
	src := mediatest.ChapteredMPCChapters()
	assertChapters(t, "cut", parseOutput(t, cutOut).Chapters(), []waxlabel.Chapter{
		{Start: 0, Title: "Middle"},
		{Start: src[2].Start - 200*time.Millisecond, Title: "Coda"},
		{Start: src[3].Start - 200*time.Millisecond, Title: ""},
	})
}

// A WMA input is a local source like any other lossy one: it decodes to the
// requested format, and its tags and Marker Object chapters (which WaxLabel
// reads since v1.6.2) ride the ordinary carry pass into the output. The
// muxer's encoder stamp is excluded by policy rather than dropped, so nothing
// warns and the output carries exactly the title. A cut remaps the markers
// like any other chapter set. What a WMA file cannot be is written: a copy
// and an output carrying its extension both fail as incompatible specs, exit
// 2 at the CLI, before any engine call.
func TestProcessWMASource(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wma")
	if err := os.WriteFile(in, mediatest.ChapteredWMA(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "out.flac")
	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if res.SourceFormat.Codec != "wma" || res.SourceFormat.Extension != "wma" {
		t.Errorf("source format = %+v, want wma/wma", res.SourceFormat)
	}
	if res.OutputFormat.Codec != "flac" || !res.Transcoded {
		t.Errorf("output format = %+v transcoded %v, want a flac encode", res.OutputFormat, res.Transcoded)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a whole carry warned: %+v", res.Warnings)
	}
	doc := parseOutput(t, out)
	assertTags(t, doc, mediatest.ChapteredWMATags)
	assertChapters(t, "transcode", doc.Chapters(), mediatest.ChapteredWMAChapters())

	// Removing [250 ms, 750 ms) cuts into Intro (0 to 500 ms), lands Mïddle
	// (500 ms to 1.25 s) on the join, and shifts Coda by the 500 ms removed.
	cutOut := filepath.Join(dir, "cut.flac")
	res, err = c.Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(cutOut),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 250 * time.Millisecond, End: 750 * time.Millisecond}}},
		},
	})
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	if hasWarning(res, WarnTagCarry) {
		t.Errorf("a remapped marker carry warned: %+v", res.Warnings)
	}
	assertChapters(t, "cut", parseOutput(t, cutOut).Chapters(), []waxlabel.Chapter{
		{Start: 0, Title: "Intro"},
		{Start: 250 * time.Millisecond, Title: "Mïddle"},
		{Start: 750 * time.Millisecond, Title: "Coda"},
	})

	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "copy.wma")), Transcode: &TranscodeSpec{Format: FormatCopy}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "does not write it") || !strings.Contains(err.Error(), "--format") {
		t.Errorf("copy = %v, want ErrIncompatibleSpec naming the decode-only source and the --format escape", err)
	}
	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "out.wma")), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("flac under .wma = %v, want ErrIncompatibleSpec (the extension is refused, not force-muxed)", err)
	}
}

// An album takes the members whose headers state their length only
// approximately, which the engine's timeline refuses at plan time until they
// are measured: ASF states a rounded duration (and WMA carries no padding
// count, so a decode delivers whole frames past it), and a WAV carrying MP3
// frames counts everything they decode to. The per-track measurement reads
// every file to its end and hands the count to the timeline, so the album
// runs, every output holds its audio and its metadata, and the mixed rates
// (8, 22.05 and 44.1 kHz) conform to the envelope. The MP3 member also
// carries the one engine remark a fixture here can trigger, which the album
// reports under its own code, apart from damage.
func TestProcessAlbumTakesAdvisoryLengths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	wma := filepath.Join(dir, "in.wma")
	frames := filepath.Join(dir, "frames.wav")
	wav := filepath.Join(dir, "in.wav")
	for path, data := range map[string][]byte{wma: mediatest.ChapteredWMA(), frames: mediatest.MP3WAV(), wav: mediatest.SineWAV(2, 2)} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := newOfflineClient(t).ProcessAlbum(ctx, []AlbumTrack{
		{Input: wma, Output: filepath.Join(dir, "one.flac")},
		{Input: frames, Output: filepath.Join(dir, "two.flac")},
		{Input: wav, Output: filepath.Join(dir, "three.flac")},
	}, -18, TranscodeSpec{Format: FormatFLAC})
	if err != nil {
		t.Fatalf("album with advisory-length members: %v, want it to run on the measured lengths", err)
	}
	if !loudness.Gainable(res.Album.IntegratedLUFS) || !res.LoudnessApplied {
		t.Errorf("album loudness = %+v applied %v, want a measurable album with its gain applied", res.Album, res.LoudnessApplied)
	}
	doc := parseOutput(t, res.Outputs[0])
	assertTags(t, doc, mediatest.ChapteredWMATags)
	assertChapters(t, "album", doc.Chapters(), mediatest.ChapteredWMAChapters())
	for i, out := range res.Outputs {
		if d := probeDuration(t, out); d < time.Second {
			t.Errorf("output %d (%s) holds %s of audio, want the whole member", i, filepath.Base(out), d)
		}
	}
	var note string
	for _, w := range res.Warnings {
		switch w.Code {
		case WarnInputDamage:
			t.Errorf("an undamaged album warned of damage: %q", w.Detail)
		case WarnInputNote:
			note = w.Detail
		}
	}
	if !strings.HasPrefix(note, "frames.wav: ") || !strings.Contains(note, "nCodecDelay") {
		t.Errorf("input-note = %q, want the MP3 member named with the engine's nCodecDelay remark", note)
	}
}

// probeDuration reports the audio length an output declares.
func probeDuration(t *testing.T, path string) time.Duration {
	t.Helper()
	pr, err := media.NewRunner(media.RunnerConfig{}).Probe(t.Context(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", filepath.Base(path), err)
	}
	return pr.Format.Duration
}

// The codecs the engine only decodes that arrive inside containers WaxTap
// writes: the file's name says nothing, so the refusal names the codec. A
// G.711 WAV re-encodes to anything and copies to nothing; an MP3 carried in a
// WAV copies out to a bare .mp3 without a re-encode, and not back into a WAV,
// which WaxTap does not write it into; WMA Lossless decodes as the lossless
// source it is.
func TestProcessDecodeOnlyCodecsInWritableContainers(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	alaw := filepath.Join(dir, "alaw.wav")
	frames := filepath.Join(dir, "frames.wav")
	lossless := filepath.Join(dir, "lossless.wma")
	for path, data := range map[string][]byte{alaw: mediatest.ALawWAV(), frames: mediatest.MP3WAV(), lossless: mediatest.LosslessWMA()} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := c.Process(ctx, ProcessRequest{Input: alaw, ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "alaw.opus")), Transcode: &TranscodeSpec{Format: FormatOpus}}})
	if err != nil {
		t.Fatalf("alaw to opus: %v", err)
	}
	if res.SourceFormat.Codec != "alaw" || res.SourceFormat.Extension != "wav" || hasWarning(res, WarnImplicitLossy) {
		t.Errorf("alaw source = %+v (implicit-lossy warned: %v), want alaw/wav and no implicit-lossy warning on a source that was lossy already", res.SourceFormat, hasWarning(res, WarnImplicitLossy))
	}
	_, err = c.Process(ctx, ProcessRequest{Input: alaw, ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "alaw-copy.wav")), Transcode: &TranscodeSpec{Format: FormatCopy}}})
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "G.711 A-law") || !strings.Contains(err.Error(), "--format") {
		t.Errorf("alaw copy = %v, want ErrIncompatibleSpec naming G.711 A-law and the --format escape", err)
	}

	// A copy names the container by the output's extension, and the CLI
	// turns a --format naming the family the source is already in into this
	// same copy; the library re-encodes a named format as asked.
	res, err = c.Process(ctx, ProcessRequest{Input: frames, ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "frames.mp3")), Transcode: &TranscodeSpec{Format: FormatCopy}}})
	if err != nil {
		t.Fatalf("mp3-in-wav copied to .mp3: %v", err)
	}
	if res.SourceFormat.Codec != "mp3" || res.SourceFormat.Extension != "wav" || res.Transcoded || res.OutputFormat.Codec != "mp3" || res.OutputFormat.Extension != "mp3" {
		t.Errorf("mp3-in-wav copied to .mp3 = source %+v, transcoded %v, output %+v; want the frames copied out unencoded", res.SourceFormat, res.Transcoded, res.OutputFormat)
	}
	if d := probeDuration(t, filepath.Join(dir, "frames.mp3")); d < time.Second {
		t.Errorf("copied-out mp3 holds %s, want the whole second", d)
	}
	if head, err := os.ReadFile(filepath.Join(dir, "frames.mp3")); err != nil || len(head) < 4 || string(head[:4]) == "RIFF" {
		t.Errorf("copied-out mp3 starts %q (err %v), want a bare MP3, not a WAV", head[:min(4, len(head))], err)
	}
	if note := warningDetail(res, WarnInputNote); !strings.Contains(note, "nCodecDelay") || hasWarning(res, WarnInputDamage) {
		t.Errorf("input-note = %q (damage warned: %v), want the engine's nCodecDelay remark as a note and no damage", note, hasWarning(res, WarnInputDamage))
	}
	_, err = c.Process(ctx, ProcessRequest{Input: frames, ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "frames-copy.wav")), Transcode: &TranscodeSpec{Format: FormatCopy}}})
	if !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("mp3-in-wav copied back into a .wav = %v, want ErrIncompatibleSpec: WaxTap writes no MP3 into a WAV", err)
	}

	res, err = c.Process(ctx, ProcessRequest{Input: lossless, ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "lossless.flac")), Transcode: &TranscodeSpec{Format: FormatFLAC}}})
	if err != nil {
		t.Fatalf("wma lossless to flac: %v", err)
	}
	if res.SourceFormat.Codec != "wmalossless" || res.SourceFormat.Extension != "wma" || !res.Transcoded {
		t.Errorf("wma lossless source = %+v transcoded %v, want wmalossless/wma re-encoded", res.SourceFormat, res.Transcoded)
	}
	_, err = c.Process(ctx, ProcessRequest{Input: lossless, ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "lossless-copy.mka")), Transcode: &TranscodeSpec{Format: FormatCopy}}})
	if !errors.Is(err, ErrIncompatibleSpec) || !strings.Contains(err.Error(), "WMA Lossless") {
		t.Errorf("wma lossless copy = %v, want ErrIncompatibleSpec naming WMA Lossless", err)
	}
}

// parseOutput reads the metadata a process left on path.
func parseOutput(t *testing.T, path string) *waxlabel.Document {
	t.Helper()
	doc, err := waxlabel.ParseFile(t.Context(), path)
	if err != nil {
		t.Fatalf("parse %s: %v", filepath.Base(path), err)
	}
	return doc
}

// assertTags compares the tags a process left on doc against want, exactly:
// every key with its one value, and nothing else.
func assertTags(t *testing.T, doc *waxlabel.Document, want map[string]string) {
	t.Helper()
	if n := doc.Tags().Len(); n != len(want) {
		t.Errorf("output carries %d tags, want %d: %v", n, len(want), want)
	}
	for key, val := range want {
		k, err := tag.ParseKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := doc.Get(k); len(got) != 1 || got[0] != val {
			t.Errorf("carried %s = %q, want the one value %q", key, got, val)
		}
	}
}

// assertChapters compares a carried chapter set against the source's. The
// output's chapter form keeps milliseconds (VorbisComment CHAPTERxxx), so a
// start within that of the source's counts as carried; every other field,
// End included, must come back as the source had it.
func assertChapters(t *testing.T, step string, got, want []waxlabel.Chapter) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: chapters = %+v, want %d: %+v", step, got, len(want), want)
	}
	for i, w := range want {
		g := got[i]
		if (g.Start - w.Start).Abs() < time.Millisecond {
			g.Start = w.Start
		}
		if g != w {
			t.Errorf("%s: chapter %d = %+v, want %+v", step, i, got[i], w)
		}
	}
}
