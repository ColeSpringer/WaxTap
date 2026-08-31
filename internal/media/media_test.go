package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"
	"github.com/colespringer/waxlabel"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// wavFixture writes a pure-Go WAV sine of the given length/channels and returns
// its path.
func wavFixture(t *testing.T, seconds, channels int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.wav")
	if err := os.WriteFile(p, mediatest.SineWAV(seconds, channels), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// encodeFixture transcodes a WAV to codec at name (in dir) and returns its path.
func encodeFixture(t *testing.T, r *Runner, dir, name string, c Codec) string {
	t.Helper()
	src := wavFixture(t, 3, 2)
	out := filepath.Join(dir, name)
	if _, err := r.Transcode(context.Background(), src, out, Spec{Codec: c}); err != nil {
		t.Fatalf("encode %s: %v", name, err)
	}
	return out
}

// hotFixture writes a float WAV carrying overs samples per channel planted
// past full scale and returns its path; encoding it to an integer output must
// clip exactly 2*overs samples (stereo).
func hotFixture(t *testing.T, overs int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hot.wav")
	if err := os.WriteFile(p, mediatest.HotFloatWAV(1, 2, overs), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTranscodeReportsLevels(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()

	out := filepath.Join(dir, "hot.flac")
	res, err := r.Transcode(context.Background(), hotFixture(t, 13), out, Spec{Codec: CodecFLAC})
	if err != nil {
		t.Fatal(err)
	}
	// 13 overs per channel, stereo: the quantizer clamps all 26.
	l := res.Levels
	if l.ClippedSamples != 26 || l.Samples != 44100 || l.Channels != 2 || !l.Quantized {
		t.Errorf("Levels = %+v, want 26 clipped of 44100x2 quantized", l)
	}
	if want := "clipping: 26 of 88200"; !strings.Contains(l.Note(), want) {
		t.Errorf("Note() = %q, want it to carry %q", l.Note(), want)
	}

	// A clean encode warrants no note...
	clean := filepath.Join(dir, "clean.flac")
	cres, err := r.Transcode(context.Background(), wavFixture(t, 1, 2), clean, Spec{Codec: CodecFLAC})
	if err != nil {
		t.Fatal(err)
	}
	if cres.Levels.ClippedSamples != 0 || cres.Levels.Note() != "" {
		t.Errorf("clean encode Levels = %+v (note %q), want no clipping and no note", cres.Levels, cres.Levels.Note())
	}

	// ...and a container copy never earns one, whatever the source held.
	copied := filepath.Join(dir, "hot.mka")
	rres, err := r.Transcode(context.Background(), out, copied, Spec{Codec: CodecCopy})
	if err != nil {
		t.Fatal(err)
	}
	if rres.Levels != (Levels{}) {
		t.Errorf("remux Levels = %+v, want zero (a packet copy re-derives no samples)", rres.Levels)
	}
}

// TestTranscodeReportsTruePeakOnly covers the other half of the level report: a
// source whose stored samples stay under full scale while the waveform between
// them crosses it. The quantizer clamps nothing, so only the output true-peak
// meter can say playback will clip.
func TestTranscodeReportsTruePeakOnly(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := filepath.Join(dir, "isp.wav")
	if err := os.WriteFile(in, mediatest.IntersampleHotWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := r.Transcode(context.Background(), in, filepath.Join(dir, "isp.flac"), Spec{Codec: CodecFLAC})
	if err != nil {
		t.Fatal(err)
	}
	l := res.Levels
	if l.ClippedSamples != 0 || !l.Quantized || l.TruePeak < 1.1 || l.TruePeak > 1.3 {
		t.Errorf("Levels = %+v, want no clipped samples and a ~1.2 true peak", l)
	}
	if !strings.Contains(l.Note(), "true peak:") {
		t.Errorf("Note() = %q, want the true-peak wording", l.Note())
	}
}

func TestCodecStringExtensionLossless(t *testing.T) {
	cases := []struct {
		c        Codec
		str, ext string
		lossless bool
	}{
		{CodecCopy, "copy", "", true},
		{CodecFLAC, "flac", "flac", true},
		{CodecALAC, "alac", "m4a", true},
		{CodecWAV, "wav", "wav", true},
		{CodecMP3, "mp3", "mp3", false},
		{CodecAAC, "aac", "m4a", false},
		{CodecOpus, "opus", "opus", false},
		{CodecVorbis, "vorbis", "ogg", false},
		{CodecAIFF, "aiff", "aiff", true},
		{CodecHEAAC, "he-aac", "m4a", false},
		{CodecWavPack, "wavpack", "wv", true},
		{CodecAPE, "ape", "ape", true},
	}
	for _, tc := range cases {
		if got := tc.c.String(); got != tc.str {
			t.Errorf("%v String() = %q, want %q", tc.c, got, tc.str)
		}
		if got := tc.c.Extension(); got != tc.ext {
			t.Errorf("%v Extension() = %q, want %q", tc.c, got, tc.ext)
		}
		if got := tc.c.IsLossless(); got != tc.lossless {
			t.Errorf("%v IsLossless() = %v, want %v", tc.c, got, tc.lossless)
		}
	}
}

func TestEncodeOptionsBitrateDefaults(t *testing.T) {
	if o := encodeOptions(Spec{Codec: CodecMP3}); o.Format != "mp3" || o.MP3Bitrate != defaultMP3Bitrate || o.MP3VBR {
		t.Errorf("MP3 default = %+v, want CBR %d", o, defaultMP3Bitrate)
	}
	if o := encodeOptions(Spec{Codec: CodecMP3, Bitrate: 128000}); o.MP3Bitrate != 128000 {
		t.Errorf("MP3 override = %d, want 128000", o.MP3Bitrate)
	}
	if o := encodeOptions(Spec{Codec: CodecAAC}); o.AACBitrate != defaultAACBitrate {
		t.Errorf("AAC default = %d, want %d", o.AACBitrate, defaultAACBitrate)
	}
	if o := encodeOptions(Spec{Codec: CodecOpus}); o.OpusBitrate != defaultOpusBitrate {
		t.Errorf("Opus default = %d, want %d", o.OpusBitrate, defaultOpusBitrate)
	}
	if o := encodeOptions(Spec{Codec: CodecVorbis, Bitrate: 200000}); o.VorbisQuality != defaultVorbisQuality || o.VorbisBitrate != 0 {
		t.Errorf("Vorbis = %+v, want quality %v and no bitrate (bitrate ignored)", o, defaultVorbisQuality)
	}
	if o := encodeOptions(Spec{Codec: CodecFLAC, Channels: 2, GainDB: -3}); o.Channels != 2 || o.GainDB != -3 || o.BitDepth != 0 {
		t.Errorf("FLAC opts = %+v, want channels 2, gain -3, keep depth", o)
	}
	// HE-AAC rides the AAC bitrate anchor with its own low-rate default; WaxTap
	// encodes v1 only, so the v2 selector must stay off.
	if o := encodeOptions(Spec{Codec: CodecHEAAC}); o.Format != "he-aac" || o.AACBitrate != defaultHEAACBitrate || o.HEAACv2 {
		t.Errorf("HE-AAC default = %+v, want format he-aac at %d, v1", o, defaultHEAACBitrate)
	}
	if o := encodeOptions(Spec{Codec: CodecHEAAC, Bitrate: 48000}); o.AACBitrate != 48000 {
		t.Errorf("HE-AAC override = %d, want 48000", o.AACBitrate)
	}
	// The lossless WavPack/APE rows take no bitrate and keep the level defaults.
	if o := encodeOptions(Spec{Codec: CodecWavPack}); o.Format != "wavpack" || o.WavPackLevel != 0 {
		t.Errorf("WavPack = %+v, want format wavpack at the default level", o)
	}
	if o := encodeOptions(Spec{Codec: CodecAPE}); o.Format != "ape" || o.APELevel != 0 {
		t.Errorf("APE = %+v, want format ape at the default level", o)
	}
}

func TestCodecNameBoundary(t *testing.T) {
	// Every codec.ID WaxTap handles must map to a name ContainerAccepts understands.
	cases := map[codec.ID]string{
		codec.Opus: "opus", codec.AACLC: "aac", codec.FLAC: "flac",
		codec.ALAC: "alac", codec.MP3: "mp3", codec.Vorbis: "vorbis", codec.PCM: "pcm",
		codec.HEAAC: "he-aac", codec.WavPack: "wavpack", codec.APE: "ape", codec.WMA: "wma",
	}
	for id, want := range cases {
		if got := codecName(id); got != want {
			t.Errorf("codecName(%v) = %q, want %q", id, got, want)
		}
	}
	// codecToFormat is the write-direction inverse for the remuxable codecs. PCM is
	// excluded because its wire layout belongs to the container, so no packet copy
	// survives (see TestPCMRemuxDeclined).
	for _, id := range []codec.ID{codec.Opus, codec.AACLC, codec.HEAAC, codec.FLAC, codec.ALAC, codec.MP3, codec.Vorbis, codec.WavPack, codec.APE} {
		if _, ok := codecToFormat(id); !ok {
			t.Errorf("codecToFormat(%v) not ok", id)
		}
	}
	if _, ok := codecToFormat(codec.PCM); ok {
		t.Error("codecToFormat(pcm) ok = true; PCM must decline so the caller gets ErrIncompatibleSpec, not an engine error")
	}
	// WMA is decode-only upstream: no output row exists, so a remux must decline
	// with WaxTap's own wording rather than reach the engine.
	if _, ok := codecToFormat(codec.WMA); ok {
		t.Error("codecToFormat(wma) ok = true; WMA has no WaxFlow output row and must decline")
	}
}

func TestProbeReportsSourceFacts(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	in := wavFixture(t, 2, 2)
	pr, err := r.Probe(context.Background(), in)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	a, ok := pr.AudioStream()
	if !ok {
		t.Fatal("no audio stream")
	}
	if a.CodecName != "pcm" || a.SampleRate != 44100 || a.Channels != 2 {
		t.Errorf("stream = %+v, want pcm/44100/2ch", a)
	}
	if d := pr.Format.Duration; d < 1900*time.Millisecond || d > 2100*time.Millisecond {
		t.Errorf("duration = %v, want ~2s", d)
	}
	if pr.Format.Size <= 0 || pr.Format.Container == "" {
		t.Errorf("format = %+v, want size>0 and a container name", pr.Format)
	}
}

func TestProbeRejectsNonAudio(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	junk := filepath.Join(t.TempDir(), "x.bin")
	os.WriteFile(junk, []byte("not audio at all"), 0o644)
	if _, err := r.Probe(context.Background(), junk); err == nil {
		t.Error("probe of junk should error")
	}
}

func TestTranscodeRoundTripsCodecs(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		c    Codec
		want string
	}{
		{"out.flac", CodecFLAC, "flac"},
		{"out.mp3", CodecMP3, "mp3"},
		{"out.opus", CodecOpus, "opus"},
		{"out.ogg", CodecVorbis, "vorbis"},
		{"out.m4a", CodecAAC, "aac"},
		{"out_he.m4a", CodecHEAAC, "he-aac"},
		{"out.wv", CodecWavPack, "wavpack"},
		{"out.ape", CodecAPE, "ape"},
	} {
		out := encodeFixture(t, r, dir, tc.name, tc.c)
		pr, err := r.Probe(context.Background(), out)
		if err != nil {
			t.Fatalf("probe %s: %v", tc.name, err)
		}
		if a, _ := pr.AudioStream(); a.CodecName != tc.want {
			t.Errorf("%s codec = %q, want %q", tc.name, a.CodecName, tc.want)
		}
	}
}

func TestTranscodeCopyRemuxChangesContainer(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	// Encode a native FLAC, then copy-remux it into Matroska: codec stays flac.
	flac := encodeFixture(t, r, dir, "a.flac", CodecFLAC)
	out := filepath.Join(dir, "a.mka")
	if _, err := r.Transcode(context.Background(), flac, out, Spec{Codec: CodecCopy}); err != nil {
		t.Fatalf("remux: %v", err)
	}
	pr, err := r.Probe(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := pr.AudioStream(); a.CodecName != "flac" {
		t.Errorf("remux changed codec to %q, want flac", a.CodecName)
	}
	if pr.Format.Container != "mka" {
		t.Errorf("container = %q, want mka", pr.Format.Container)
	}
}

func TestTranscodeCopyRejectsProcessing(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	in := wavFixture(t, 1, 2)
	out := filepath.Join(t.TempDir(), "o.wav")
	if _, err := r.Transcode(context.Background(), in, out, Spec{Codec: CodecCopy, Channels: 1}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("copy + channel change err = %v, want ErrIncompatibleSpec", err)
	}
	if _, err := r.Transcode(context.Background(), in, out, Spec{Codec: CodecCopy, GainDB: 3}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("copy + gain err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestRenderCutRemuxOpusIsLossless(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "in.opus", CodecOpus) // 3s
	out := filepath.Join(dir, "cut.opus")
	res, err := r.Render(context.Background(), in, out, CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: time.Second}},
		Total:   3 * time.Second,
		CopyCut: true,
		Encode:  Spec{Codec: CodecOpus},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if res.Mode != ModeCopy {
		t.Errorf("Opus cut mode = %v, want ModeCopy (cut-remux, no re-encode)", res.Mode)
	}
	if !res.Applied {
		t.Error("cut not applied")
	}
	if res.Levels != (Levels{}) {
		t.Errorf("cut-remux Levels = %+v, want zero (a packet copy re-derives no samples)", res.Levels)
	}
}

func TestRenderCutReportsLevels(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	out := filepath.Join(t.TempDir(), "cut.flac")
	// The fixture's overs all land in the first ~13ms, inside the kept span, so
	// the cut re-encode must clamp and report all 26 of them.
	res, err := r.Render(context.Background(), hotFixture(t, 13), out, CutSpec{
		Keeps:  []cutrange.Range{{Start: 0, End: 500 * time.Millisecond}},
		Total:  time.Second,
		Encode: Spec{Codec: CodecFLAC},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Levels.ClippedSamples != 26 {
		t.Errorf("Levels = %+v, want 26 clipped samples", res.Levels)
	}
	if want := "clipping: 26 of "; !strings.Contains(res.Levels.Note(), want) {
		t.Errorf("Note() = %q, want it to carry %q", res.Levels.Note(), want)
	}
}

func TestRenderCutFlacFallsBackToReencode(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "in.flac", CodecFLAC) // 3s
	out := filepath.Join(dir, "cut.flac")
	// FLAC is off the cut-remux allowlist, so a copy cut re-encodes losslessly.
	res, err := r.Render(context.Background(), in, out, CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: time.Second}},
		Total:   3 * time.Second,
		CopyCut: true,
		Encode:  Spec{Codec: CodecFLAC},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if res.Mode != ModeAccurate {
		t.Errorf("FLAC cut mode = %v, want ModeAccurate (re-encode)", res.Mode)
	}
	if a, _ := mustProbe(t, r, out).AudioStream(); a.CodecName != "flac" {
		t.Errorf("re-encoded codec = %q, want flac", a.CodecName)
	}
}

func TestRenderRequireCopyRejectsFlac(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "in.flac", CodecFLAC)
	out := filepath.Join(dir, "cut.flac")
	_, err := r.Render(context.Background(), in, out, CutSpec{
		Keeps:             []cutrange.Range{{Start: 0, End: time.Second}},
		Total:             3 * time.Second,
		CopyCut:           true,
		RequireCopyFormat: true, // explicit copy: no re-encode fallback allowed
		Encode:            Spec{Codec: CodecFLAC},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("explicit-copy FLAC cut err = %v, want ErrIncompatibleSpec", err)
	}
	if fileExists(out) {
		t.Error("rejected cut wrote output")
	}
}

func TestRenderMultiRangeReencode(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "in.flac", CodecFLAC) // 3s
	out := filepath.Join(dir, "cut.flac")
	// Two kept ranges force a Concat re-encode. Kept 0-1 and 2-3 = ~2s.
	res, err := r.Render(context.Background(), in, out, CutSpec{
		Keeps:  []cutrange.Range{{Start: 0, End: time.Second}, {Start: 2 * time.Second, End: 3 * time.Second}},
		Total:  3 * time.Second,
		Encode: Spec{Codec: CodecFLAC},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !res.Applied {
		t.Error("cut not applied")
	}
	if d := mustProbe(t, r, out).Format.Duration; d < 1700*time.Millisecond || d > 2300*time.Millisecond {
		t.Errorf("multi-range cut duration = %v, want ~2s", d)
	}
}

func TestValidateCrossfade(t *testing.T) {
	keeps := []cutrange.Range{{Start: 0, End: time.Second}, {Start: 2 * time.Second, End: 3 * time.Second}}
	if err := ValidateCrossfade(keeps, 0); err != nil {
		t.Errorf("zero crossfade should pass: %v", err)
	}
	if err := ValidateCrossfade(keeps, 500*time.Millisecond); err != nil {
		t.Errorf("fitting crossfade should pass: %v", err)
	}
	if err := ValidateCrossfade(keeps, 2*time.Second); err == nil {
		t.Error("crossfade longer than a span should be rejected")
	}
}

func TestContainerAcceptsTable(t *testing.T) {
	cases := []struct {
		ext, codec string
		want       bool
	}{
		{"flac", "flac", true}, {"flac", "aac", false},
		{"m4a", "aac", true}, {"m4a", "alac", true}, {"m4a", "opus", false},
		{"ogg", "opus", true}, {"ogg", "vorbis", true}, {"ogg", "aac", false},
		{"wav", "pcm", true}, {"opus", "opus", true},
		{"webm", "opus", true}, {"webm", "aac", false},
		{"mka", "aac", true}, {"aac", "aac", true}, {"aac", "alac", false},
		{"", "aac", true}, // unknown container: permissive
		// PCM's two names must not cross: .aiff cannot hold RIFF and .wav cannot hold
		// AIFF, though both hold a probed "pcm" source.
		{"aiff", "aiff", true}, {"aif", "aiff", true}, {"aiff", "pcm_s16le", true},
		{"aiff", "wav", false}, {"aiff", "flac", false}, {"aiff", "aac", false},
		{"wav", "aiff", false},
		// All four registered spellings answer identically; when .aifc was missing it
		// collected RIFF bytes.
		{"aifc", "aiff", true}, {"afc", "aiff", true},
		{"aifc", "pcm_s16le", true}, {"afc", "pcm_s16le", true},
		{"aifc", "wav", false}, {"afc", "flac", false},
		// Matroska takes PCM through the wav row. The aiff row has no alternate
		// container, so aiff into .mka has to be rejected before the encode.
		{"mka", "aiff", false}, {"mka", "pcm_s16le", true},
		// HE-AAC rides everywhere the AAC family does, and nowhere else.
		{"m4a", "he-aac", true}, {"mp4", "he-aac", true}, {"m4b", "he-aac", true},
		{"aac", "he-aac", true}, {"mka", "he-aac", true},
		{"webm", "he-aac", false}, {"ogg", "he-aac", false},
		// WavPack and APE fit only their own containers; WaxFlow writes neither
		// into Matroska.
		{"wv", "wavpack", true}, {"wv", "flac", false}, {"wv", "ape", false},
		{"ape", "ape", true}, {"ape", "wavpack", false},
		{"mka", "wavpack", false}, {"mka", "ape", false},
	}
	for _, c := range cases {
		if got := ContainerAccepts(c.ext, c.codec); got != c.want {
			t.Errorf("ContainerAccepts(%q,%q) = %v, want %v", c.ext, c.codec, got, c.want)
		}
	}
}

func TestAnalyzeFileCancellationNotBadInput(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	in := wavFixture(t, 2, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.AnalyzeFile(ctx, in, 0)
	if err == nil {
		t.Skip("engine completed before observing cancellation")
	}
	if errors.Is(err, waxerr.ErrUnsupportedInput) {
		t.Errorf("canceled analyze classified as bad input: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled analyze err = %v, want context.Canceled", err)
	}
}

func TestContainerForFormatAware(t *testing.T) {
	cases := []struct {
		format, ext, want string
	}{
		{"aac", "m4a", "progressive"}, // else fragmented CMAF (Apple-hostile)
		{"alac", "m4a", "progressive"},
		// Keyed on the format, not the extension: AAC and ALAC are MP4 either way.
		{"alac", "alac", "progressive"},
		{"aac", "", "progressive"},    // extensionless output
		{"aac", "xyz", "progressive"}, // unrelated extension
		{"flac", "ogg", "ogg"},        // else a bare FLAC stream in a .ogg file
		{"opus", "ogg", ""},           // Opus is Ogg natively
		{"vorbis", "ogg", ""},
		{"opus", "mka", "mka"},
		{"opus", "webm", "webm"},
		{"aac", "aac", "adts"},
		{"flac", "flac", ""},
		{"mp3", "mp3", ""},
		// HE-AAC is MP4 like the rest of its family, and ADTS on a .aac path.
		{"he-aac", "m4a", "progressive"},
		{"he-aac", "", "progressive"},
		{"he-aac", "aac", "adts"},
		{"he-aac", "mka", "mka"},
		// WavPack and APE have no alternate containers; any override would error.
		{"wavpack", "wv", ""},
		{"ape", "ape", ""},
		// The aiff row has no alternate container, so any override would error.
		{"aiff", "aiff", ""},
		{"aiff", "aif", ""},
		{"aiff", "", ""},
	}
	for _, c := range cases {
		if got := containerFor(c.format, c.ext); got != c.want {
			t.Errorf("containerFor(%q,%q) = %q, want %q", c.format, c.ext, got, c.want)
		}
	}
}

func TestContainerTablesRejectUnmuxable(t *testing.T) {
	// The tables must not advertise .mka for mp3/alac, which WaxFlow cannot mux.
	if ContainerAccepts("mka", "mp3") || ContainerAccepts("mka", "alac") {
		t.Error("mka must not accept mp3 or alac")
	}
	if err := CheckOutputContainer(CodecMP3, "out.mka"); err == nil {
		t.Error("mp3 into .mka should be rejected before the engine")
	}
	if err := CheckOutputContainer(CodecALAC, "out.mka"); err == nil {
		t.Error("alac into .mka should be rejected before the engine")
	}
}

func TestRenderCutRemuxAACProgressive(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "in.m4a", CodecAAC) // 3s AAC-LC, progressive
	out := filepath.Join(dir, "cut.m4a")
	res, err := r.Render(context.Background(), in, out, CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: time.Second}},
		Total:   3 * time.Second,
		CopyCut: true,
		Encode:  Spec{Codec: CodecAAC},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if res.Mode != ModeCopy {
		t.Errorf("AAC cut mode = %v, want ModeCopy (cut-remux)", res.Mode)
	}
	// The cut output must be a progressive (tag-friendly) MP4, not fragmented CMAF.
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if bytes.Contains(b, []byte("moof")) {
		t.Error("AAC cut produced a fragmented MP4 (moof); want progressive")
	}
}

// TestEncodeMP4ProgressiveOffM4APath covers AAC and ALAC written to paths that do
// not name MP4. They are MP4 regardless, so they need the progressive override;
// a fragmented file cannot be tagged and Apple players reject it.
func TestEncodeMP4ProgressiveOffM4APath(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	cases := []struct {
		name string
		c    Codec
	}{
		{"out.alac", CodecALAC}, // codec-named extension
		{"out", CodecALAC},      // extensionless
		{"aac_out", CodecAAC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := encodeFixture(t, r, t.TempDir(), tc.name, tc.c)
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte("moof")) {
				t.Errorf("%s encode to %q produced a fragmented MP4 (moof); want progressive", tc.c, tc.name)
			}
		})
	}
}

// magicOf returns a file's first four bytes, the container's magic.
func magicOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 4 {
		t.Fatalf("%s is %d bytes", path, len(b))
	}
	return string(b[:4])
}

// TestAIFFWritesFORMNotRIFF covers the encode side of the PCM split. An .aiff
// output must carry AIFF's FORM magic, not the RIFF that PCM's default wav row
// would write.
func TestAIFFWritesFORMNotRIFF(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()

	enc := encodeFixture(t, r, dir, "encoded.aiff", CodecAIFF)
	if got := magicOf(t, enc); got != "FORM" {
		t.Errorf("AIFF encode magic = %q, want %q", got, "FORM")
	}
	if c := mustProbe(t, r, enc).Format.Container; c != "aiff" {
		t.Errorf("AIFF encode container = %q, want %q", c, "aiff")
	}
	// The mirror, confirming the choice follows the codec and is not blanket.
	if got := magicOf(t, encodeFixture(t, r, dir, "encoded.wav", CodecWAV)); got != "RIFF" {
		t.Errorf("WAV encode magic = %q, want %q", got, "RIFF")
	}
}

// TestPCMRemuxDeclined covers why PCM is missing from codecToFormat and what
// depends on that. PCM packets are raw samples whose layout belongs to the
// container, so WaxFlow declines every PCM remux; declining in WaxTap instead
// gives the caller ErrIncompatibleSpec at exit 2 rather than an engine string at
// exit 1.
//
// A WaxFlow bump that lifts the decline fails the second half. PCM would then
// need a row here plus extension-directed selection between wav and aiff, or a
// copy into .aiff writes RIFF bytes.
func TestPCMRemuxDeclined(t *testing.T) {
	if f, ok := codecToFormat(codec.PCM); ok {
		t.Errorf("codecToFormat(PCM) = %q,true; want a decline (no PCM remux survives)", f)
	}

	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	src := encodeFixture(t, r, dir, "src.aiff", CodecAIFF)

	for _, out := range []string{"copy.aiff", "copy.wav"} {
		dst := filepath.Join(dir, out)
		_, err := r.Transcode(context.Background(), src, dst, Spec{Codec: CodecCopy})
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("copy PCM -> %s: err = %v, want ErrIncompatibleSpec (exit 2)", out, err)
		}
		if fileExists(dst) {
			t.Errorf("copy PCM -> %s left a partial output behind", out)
		}
	}

	// Confirm through WaxFlow that the engine still declines, so the check above is
	// doing real work rather than duplicating one the engine would make.
	if survives := codecSurvivesPCMProbe(t, r, src); survives {
		t.Error("WaxFlow now remuxes PCM; codecToFormat needs a PCM row with extension-directed wav/aiff selection")
	}
}

// codecSurvivesPCMProbe asks WaxFlow whether a PCM track can be packet-copied,
// bypassing codecToFormat's decline.
func codecSurvivesPCMProbe(t *testing.T, r *Runner, path string) bool {
	t.Helper()
	src, closeSrc, err := openSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSrc()
	_, info, err := format.OpenDemuxer(src, hintFor(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.engine.PlanRemux(info.Default(), waxflow.TranscodeOptions{Format: "aiff"})
	return err == nil && plan != nil
}

// TestIsAIFFExt covers the single list behind AIFF's four spellings. Every table
// that chooses between PCM's two containers reads it, so a missing spelling means
// that extension gets RIFF bytes.
func TestIsAIFFExt(t *testing.T) {
	for _, ext := range []string{"aiff", "aif", "aifc", "afc"} {
		if !IsAIFFExt(ext) {
			t.Errorf("IsAIFFExt(%q) = false, want true", ext)
		}
		// Each spelling is container-checked rather than force-muxed, so a mismatched
		// format is rejected before the encode instead of writing the wrong bytes.
		if needsForcedMuxer("out." + ext) {
			t.Errorf("needsForcedMuxer(out.%s) = true; an AIFF spelling must be container-checked", ext)
		}
		if err := CheckOutputContainer(CodecAIFF, "out."+ext); err != nil {
			t.Errorf("CheckOutputContainer(aiff, out.%s) = %v, want nil", ext, err)
		}
		if err := CheckOutputContainer(CodecFLAC, "out."+ext); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("CheckOutputContainer(flac, out.%s) = %v, want ErrIncompatibleSpec", ext, err)
		}
		if err := CheckOutputContainer(CodecWAV, "out."+ext); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("CheckOutputContainer(wav, out.%s) = %v, want ErrIncompatibleSpec", ext, err)
		}
	}
	for _, ext := range []string{"wav", "flac", "m4a", "aiffx", "af", ""} {
		if IsAIFFExt(ext) {
			t.Errorf("IsAIFFExt(%q) = true, want false", ext)
		}
	}
}

func TestCheckOutputContainerAndInfer(t *testing.T) {
	if err := CheckOutputContainer(CodecFLAC, "out.flac"); err != nil {
		t.Errorf("flac into .flac should pass: %v", err)
	}
	if err := CheckOutputContainer(CodecFLAC, "out.opus"); err == nil {
		t.Error("flac into .opus should be rejected")
	}
	if err := CheckOutputContainer(CodecCopy, "out.flac"); err != nil {
		t.Errorf("copy is never constrained: %v", err)
	}
	if needsForcedMuxer("x.flac") || !needsForcedMuxer("x.alac") || !needsForcedMuxer("x") {
		t.Error("needsForcedMuxer should be false for .flac, true for codec-name/extensionless paths")
	}
	// A force-muxed path takes its container from the format, so its extension does
	// not constrain the codec.
	for _, out := range []string{"x.alac", "x"} {
		if err := CheckOutputContainer(CodecFLAC, out); err != nil {
			t.Errorf("CheckOutputContainer(flac, %q) = %v, want nil", out, err)
		}
	}
}

// formTypeOf returns an IFF file's form type, the 4 bytes after the FORM header
// and length. It is what separates AIFF from AIFF-C; the leading magic is "FORM"
// for both.
func formTypeOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 12 {
		t.Fatalf("%s is %d bytes, too short for an IFF header", path, len(b))
	}
	return string(b[8:12])
}

// probeSampleFormat reports the written file's sample type and depth. ProbeStream
// carries neither, so this asks WaxFlow directly, the same way ffprobe's
// sample_fmt/bits_per_raw_sample rows do in the manual sweep.
func probeSampleFormat(t *testing.T, r *Runner, path string) audio.Format {
	t.Helper()
	src, closeSrc, err := openSource(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer closeSrc()
	info, err := r.Engine().Probe(src, hintFor(path), nil)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return info.Default().Fmt
}

// TestBitDepthForcesIntegerOutput is F8's knob: decoding runs in float, so a
// lossy source writes float WAV and 24-bit FLAC. BitDepth forces integer output
// on the four formats that hold integer PCM, and the lossy rows drop it.
func TestBitDepthForcesIntegerOutput(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	// An Opus round trip puts a genuine float stream in front of the encoders.
	lossy := encodeFixture(t, r, dir, "lossy.opus", CodecOpus)

	encode := func(name string, spec Spec) string {
		t.Helper()
		out := filepath.Join(dir, name)
		if _, err := r.Transcode(context.Background(), lossy, out, spec); err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		return out
	}

	// The default: follow the decoded stream, which is float.
	if f := probeSampleFormat(t, r, encode("f32.wav", Spec{Codec: CodecWAV})); f.Type != audio.Float {
		t.Errorf("default WAV from a lossy source = %v/%d-bit, want float", f.Type, f.BitDepth)
	}
	if f := probeSampleFormat(t, r, encode("d24.flac", Spec{Codec: CodecFLAC})); f.Type != audio.Int || f.BitDepth != 24 {
		t.Errorf("default FLAC from a lossy source = %v/%d-bit, want 24-bit int", f.Type, f.BitDepth)
	}

	for _, tc := range []struct {
		name  string
		codec Codec
		depth int
	}{
		{"i16.wav", CodecWAV, 16},
		{"i24.wav", CodecWAV, 24},
		{"i16.flac", CodecFLAC, 16},
		{"i16.aiff", CodecAIFF, 16},
		{"i24.aiff", CodecAIFF, 24},
		{"i16.m4a", CodecALAC, 16},
	} {
		f := probeSampleFormat(t, r, encode(tc.name, Spec{Codec: tc.codec, BitDepth: tc.depth}))
		if f.Type != audio.Int || f.BitDepth != tc.depth {
			t.Errorf("%s = %v/%d-bit, want %d-bit int", tc.name, f.Type, f.BitDepth, tc.depth)
		}
	}

	// A plain AIFF rather than AIFF-C float is what --bit-depth buys on the aiff
	// row. The magic cannot show it: both variants open "FORM". The form type at
	// offset 8 is the discriminator, AIFC for the float variant.
	if got := formTypeOf(t, encode("f32b.aiff", Spec{Codec: CodecAIFF})); got != "AIFC" {
		t.Errorf("default AIFF from a float source = %q, want AIFC (the float variant)", got)
	}
	if got := formTypeOf(t, encode("i16b.aiff", Spec{Codec: CodecAIFF, BitDepth: 16})); got != "AIFF" {
		t.Errorf("16-bit AIFF form type = %q, want plain AIFF", got)
	}

	// The lossy rows zero the depth in their adjust hooks, so the request reaches
	// the encoder and is dropped rather than failing.
	for _, c := range []Codec{CodecMP3, CodecAAC, CodecOpus, CodecVorbis} {
		if o := encodeOptions(Spec{Codec: c, BitDepth: 16}); o.BitDepth != 16 {
			t.Errorf("%v encodeOptions dropped BitDepth before WaxFlow saw it: %+v", c, o)
		}
	}
}

// TestBitDepthMatchingSourceIsBitExact: asking for the depth a 16-bit source
// already has must be a clean no-op. Neither the dither nor the widen branch
// fires, so a WAV round trip has to return the same samples.
func TestBitDepthMatchingSourceIsBitExact(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := wavFixture(t, 2, 2)

	mid := filepath.Join(dir, "mid.flac")
	if _, err := r.Transcode(context.Background(), in, mid, Spec{Codec: CodecFLAC, BitDepth: 16}); err != nil {
		t.Fatalf("encode flac: %v", err)
	}
	out := filepath.Join(dir, "out.wav")
	if _, err := r.Transcode(context.Background(), mid, out, Spec{Codec: CodecWAV, BitDepth: 16}); err != nil {
		t.Fatalf("encode wav: %v", err)
	}

	src, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// Compare samples, not headers: WaxFlow's RIFF chunk layout need not match the
	// fixture generator's canonical 44-byte one.
	samples := src[44:]
	if len(dst) < len(samples) || !bytes.Equal(samples, dst[len(dst)-len(samples):]) {
		t.Error("--bit-depth 16 on an already-16-bit source was not bit-exact")
	}
}

func mustProbe(t *testing.T, r *Runner, path string) ProbeResult {
	t.Helper()
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	return pr
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// wvTagFixture encodes a stereo sine to WavPack and tags it through WaxLabel
// (an APEv2 block, the file's only tag form), including an own-audio value.
func wvTagFixture(t *testing.T, r *Runner, dir string) string {
	t.Helper()
	out := encodeFixture(t, r, dir, "tagged.wv", CodecWavPack)
	if err := mediatest.TagFile(context.Background(), out,
		"TITLE", "Tagged Sine",
		"ARTIST", "WaxTap Test",
		"REPLAYGAIN_TRACK_GAIN", "-3.00 dB"); err != nil {
		t.Fatalf("tag fixture: %v", err)
	}
	return out
}

func tagValue(pr ProbeResult, key string) string {
	if vs := pr.Tags[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// WaxLabel writes the APEv2 block on a finished .wv and the probe's demuxer
// reads it back, the read parity the client's post-pass tagging rests on. A
// whole-file remux does not carry it: WaxFlow rewrites carry no tags, which is
// what lets the client's carry pass own every output without a double write.
func TestWavPackWaxLabelTagsProbeReadAndRemuxStrips(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := wvTagFixture(t, r, dir)

	pr := mustProbe(t, r, in)
	if got := tagValue(pr, "TITLE"); got != "Tagged Sine" {
		t.Fatalf("encoded TITLE = %q, want %q (tags = %v)", got, "Tagged Sine", pr.Tags)
	}

	out := filepath.Join(dir, "copy.wv")
	if _, err := r.Transcode(context.Background(), in, out, Spec{Codec: CodecCopy}); err != nil {
		t.Fatalf("remux: %v", err)
	}
	pr = mustProbe(t, r, out)
	if a, _ := pr.AudioStream(); a.CodecName != "wavpack" {
		t.Errorf("remux codec = %q, want wavpack", a.CodecName)
	}
	if got := tagValue(pr, "TITLE"); got != "" {
		t.Errorf("remuxed TITLE = %q, want stripped (the client's carry pass restores metadata)", got)
	}
}

// A copy cut of a WavPack source falls back to a re-encode: WavPack is not on
// WaxFlow's cut allowlist (lossless, so the re-encode costs CPU and zero
// generation loss, the ALAC rule).
func TestCutWavPackFallsBackToReencode(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := wvTagFixture(t, r, dir)
	out := filepath.Join(dir, "cut.wv")
	res, err := r.Render(context.Background(), in, out, CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: time.Second}},
		Total:   3 * time.Second,
		CopyCut: true,
		Encode:  Spec{Codec: CodecWavPack},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if res.Mode != ModeAccurate {
		t.Errorf("wavpack cut mode = %v, want ModeAccurate (not on the cut allowlist)", res.Mode)
	}
	pr := mustProbe(t, r, out)
	if a, _ := pr.AudioStream(); a.CodecName != "wavpack" {
		t.Errorf("cut codec = %q, want wavpack (fallback keeps the family)", a.CodecName)
	}
}

// The facade's tag carry parses sources with WaxLabel alone (no probe
// fallback), resting on the cross-library invariant that WaxLabel identifies
// every format the engine handles. This pins it for every format the engine
// can write; WMA, the one decode-only input, cannot be synthesized here and
// keeps its read side pinned upstream.
func TestWaxLabelReadsEveryEngineOutput(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	for _, c := range []Codec{
		CodecFLAC, CodecALAC, CodecWAV, CodecMP3, CodecAAC, CodecOpus,
		CodecVorbis, CodecAIFF, CodecHEAAC, CodecWavPack, CodecAPE,
	} {
		out := encodeFixture(t, r, dir, "out_"+c.String()+"."+c.Extension(), c)
		if _, err := waxlabel.ParseFile(context.Background(), out); err != nil {
			t.Errorf("%s: WaxLabel cannot parse the engine's own output: %v", c, err)
		}
	}
}

// The APE container takes the same WaxLabel-written APEv2 block, and the
// probe's demuxer reads it back.
func TestAPEWaxLabelTagsProbeRead(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	out := encodeFixture(t, r, dir, "tagged.ape", CodecAPE)
	if err := mediatest.TagFile(context.Background(), out, "ALBUM", "Test Album"); err != nil {
		t.Fatalf("tag fixture: %v", err)
	}
	pr := mustProbe(t, r, out)
	if a, _ := pr.AudioStream(); a.CodecName != "ape" {
		t.Errorf("codec = %q, want ape", a.CodecName)
	}
	if got := tagValue(pr, "ALBUM"); got != "Test Album" {
		t.Errorf("ALBUM = %q, want %q (tags = %v)", got, "Test Album", pr.Tags)
	}
}

// A copy of an HE-AAC source keeps its identity: the probe reports he-aac and
// the remux carries the packets rather than declining (pre-bump the codec ID
// did not exist; a decline here would break --format copy on such files).
func TestHEAACRemuxKeepsIdentity(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "he.m4a", CodecHEAAC)
	pr := mustProbe(t, r, in)
	if a, _ := pr.AudioStream(); a.CodecName != "he-aac" {
		t.Fatalf("encoded codec = %q, want he-aac", a.CodecName)
	}
	out := filepath.Join(dir, "copy.m4a")
	if _, err := r.Transcode(context.Background(), in, out, Spec{Codec: CodecCopy}); err != nil {
		t.Fatalf("remux: %v", err)
	}
	pr = mustProbe(t, r, out)
	if a, _ := pr.AudioStream(); a.CodecName != "he-aac" {
		t.Errorf("remuxed codec = %q, want he-aac (identity preserved)", a.CodecName)
	}
}

// HE-AAC's cut-remux is positional in WaxFlow's allowlist: a cut keeping the
// stream head packet-copies, one starting later declines and re-encodes. Both
// halves are pinned so an upstream allowlist change surfaces here instead of
// silently changing what a cut costs.
func TestRenderCutHEAACHeadOnly(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "he.m4a", CodecHEAAC)

	head := filepath.Join(dir, "head.m4a")
	res, err := r.Render(context.Background(), in, head, CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: time.Second}},
		Total:   3 * time.Second,
		CopyCut: true,
		Encode:  Spec{Codec: CodecHEAAC},
	})
	if err != nil {
		t.Fatalf("head cut: %v", err)
	}
	if res.Mode != ModeCopy {
		t.Errorf("head-keeping HE-AAC cut mode = %v, want ModeCopy", res.Mode)
	}

	mid := filepath.Join(dir, "mid.m4a")
	res, err = r.Render(context.Background(), in, mid, CutSpec{
		Keeps:   []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
		Total:   3 * time.Second,
		CopyCut: true,
		Encode:  Spec{Codec: CodecHEAAC},
	})
	if err != nil {
		t.Fatalf("mid cut: %v", err)
	}
	if res.Mode != ModeAccurate {
		t.Errorf("mid-stream HE-AAC cut mode = %v, want ModeAccurate (WaxFlow cuts HE-AAC only from the stream head)", res.Mode)
	}
}

// The explicit-copy refusal for a positionally-declined HE-AAC cut names the
// constraint rather than implying the file is not AAC.
func TestRenderRequireCopyHEAACMidStream(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	dir := t.TempDir()
	in := encodeFixture(t, r, dir, "he.m4a", CodecHEAAC)
	_, err := r.Render(context.Background(), in, filepath.Join(dir, "cut.m4a"), CutSpec{
		Keeps:              []cutrange.Range{{Start: time.Second, End: 2 * time.Second}},
		Total:              3 * time.Second,
		CopyCut:            true,
		RequireCopyCutMode: true,
		Encode:             Spec{Codec: CodecHEAAC},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
	if !strings.Contains(err.Error(), "stream start") {
		t.Errorf("err = %v, want it to name HE-AAC's head-only constraint", err)
	}
}

// A copy-cut source with no same-family encoder (WMA) fails with WaxTap's own
// wording instead of reaching the engine with an empty format.
func TestRenderCutCopyFallbackNeedsEncoder(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	in := filepath.Join(t.TempDir(), "in.wav")
	if err := os.WriteFile(in, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	// PCM declines the cut-remux like WMA does, and a CodecCopy fallback is the
	// same impossible spec either way.
	_, err := r.Render(context.Background(), in, filepath.Join(t.TempDir(), "out.wav"), CutSpec{
		Keeps:   []cutrange.Range{{Start: 0, End: time.Second}},
		Total:   3 * time.Second,
		CopyCut: true,
		Encode:  Spec{Codec: CodecCopy},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
	if !strings.Contains(err.Error(), "pass an explicit format") {
		t.Errorf("err = %v, want the --format escape named", err)
	}
}

// encodeOptions is the one funnel every encode passes through, and it hands
// the muxer no tags for any codec: the finished file gets the WaxLabel
// post-pass, and a mux-time set beside it would be two conflicting writers.
func TestEncodeOptionsNeverPassesTags(t *testing.T) {
	for _, c := range []Codec{CodecFLAC, CodecAPE, CodecWavPack, CodecMP3} {
		if o := encodeOptions(Spec{Codec: c}); len(o.Tags) != 0 {
			t.Errorf("%v opts.Tags = %v, want none (the post-pass owns metadata)", c, o.Tags)
		}
	}
}

// A tolerant parser that works around damage must say so: the probe carries
// WaxFlow's warnings through, so a caller can tell a short file from a short
// recording. The clamped duration is the other half: the damaged file must not
// keep claiming the length its header declares.
func TestProbeSurfacesDamageWarnings(t *testing.T) {
	r := NewRunner(RunnerConfig{})
	ctx := context.Background()

	intact := wavFixture(t, 2, 2)
	full, err := r.Probe(ctx, intact)
	if err != nil {
		t.Fatalf("probe intact: %v", err)
	}
	if len(full.Warnings) != 0 {
		t.Errorf("Warnings = %v on an undamaged file, want none", full.Warnings)
	}

	truncated := filepath.Join(t.TempDir(), "short.wav")
	whole, err := os.ReadFile(intact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truncated, whole[:len(whole)/2], 0o644); err != nil {
		t.Fatal(err)
	}

	short, err := r.Probe(ctx, truncated)
	if err != nil {
		t.Fatalf("probe truncated: %v", err)
	}
	if len(short.Warnings) == 0 {
		t.Fatal("Warnings is empty on a truncated file, want the tolerated damage reported")
	}
	if short.Format.Duration >= full.Format.Duration {
		t.Errorf("duration = %v on half a file, want less than the intact %v", short.Format.Duration, full.Format.Duration)
	}
}

// TestShortDecodeNotes pins the thresholds: a real mid-file failure trips, the
// frame-level drift ordinary encoders produce does not, and neither does a
// proportionally tiny shortfall on very long audio.
func TestShortDecodeNotes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		got      time.Duration
		declared time.Duration
		want     bool
	}{
		{"corrupt frame stops the decode", 1800 * time.Millisecond, 3 * time.Second, true},
		{"encoder padding drift", 2950 * time.Millisecond, 3 * time.Second, false},
		{"seconds lost on long audio", 10*time.Minute - 5*time.Second, 10 * time.Minute, true},
		{"framing drift on long audio", 10*time.Hour - 200*time.Millisecond, 10 * time.Hour, false},
		{"most of a short file gone", 400 * time.Millisecond, time.Second, true},
		{"unknown declared", time.Second, 0, false},
		{"unknown got", 0, time.Second, false},
		{"delivered long", 3 * time.Second, 2 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := ShortDecodeNote(tc.got, tc.declared)
			if got := note != ""; got != tc.want {
				t.Fatalf("ShortDecodeNote(%v, %v) = %q, want note=%v", tc.got, tc.declared, note, tc.want)
			}
			if tc.want && !strings.Contains(note, "did not read") {
				t.Errorf("note = %q, want it to say the remainder did not read", note)
			}
			if m := ShortMeasureNote(tc.got, tc.declared); (m != "") != tc.want {
				t.Errorf("ShortMeasureNote(%v, %v) = %q, want note=%v", tc.got, tc.declared, m, tc.want)
			}
		})
	}
}
