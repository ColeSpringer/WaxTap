package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"

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
}

func TestCodecNameBoundary(t *testing.T) {
	// Every codec.ID WaxTap handles must map to a name ContainerAccepts understands.
	cases := map[codec.ID]string{
		codec.Opus: "opus", codec.AACLC: "aac", codec.FLAC: "flac",
		codec.ALAC: "alac", codec.MP3: "mp3", codec.Vorbis: "vorbis", codec.PCM: "pcm",
	}
	for id, want := range cases {
		if got := codecName(id); got != want {
			t.Errorf("codecName(%v) = %q, want %q", id, got, want)
		}
	}
	// codecToFormat is the write-direction inverse for the remuxable codecs. PCM is
	// excluded because its wire layout belongs to the container, so no packet copy
	// survives (see TestPCMRemuxDeclined).
	for _, id := range []codec.ID{codec.Opus, codec.AACLC, codec.FLAC, codec.ALAC, codec.MP3, codec.Vorbis} {
		if _, ok := codecToFormat(id); !ok {
			t.Errorf("codecToFormat(%v) not ok", id)
		}
	}
	if _, ok := codecToFormat(codec.PCM); ok {
		t.Error("codecToFormat(pcm) ok = true; PCM must decline so the caller gets ErrIncompatibleSpec, not an engine error")
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
		Keeps:       []cutrange.Range{{Start: 0, End: time.Second}},
		Total:       3 * time.Second,
		CopyCut:     true,
		RequireCopy: true, // explicit copy: no re-encode fallback allowed
		Encode:      Spec{Codec: CodecFLAC},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("RequireCopy FLAC cut err = %v, want ErrIncompatibleSpec", err)
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
