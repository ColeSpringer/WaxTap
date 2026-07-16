package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxflow/codec"

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
	// codecToFormat is the write-direction inverse for the remuxable codecs.
	for _, id := range []codec.ID{codec.Opus, codec.AACLC, codec.FLAC, codec.ALAC, codec.MP3, codec.Vorbis, codec.PCM} {
		if _, ok := codecToFormat(id); !ok {
			t.Errorf("codecToFormat(%v) not ok", id)
		}
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
		{"flac", "ogg", "ogg"}, // else a bare FLAC stream in a .ogg file
		{"opus", "ogg", ""},    // Opus is Ogg natively
		{"vorbis", "ogg", ""},
		{"opus", "mka", "mka"},
		{"opus", "webm", "webm"},
		{"aac", "aac", "adts"},
		{"flac", "flac", ""},
		{"mp3", "mp3", ""},
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
	if !CanInferContainer("x.flac") || CanInferContainer("x.alac") || CanInferContainer("x") {
		t.Error("CanInferContainer should accept .flac, reject codec-name/extensionless paths")
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
