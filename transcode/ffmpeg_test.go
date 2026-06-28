package transcode

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/waxerr"
)

func TestBuildCommand_Copy(t *testing.T) {
	cmd, err := buildCommand("in.webm", "out.webm", Spec{Codec: CodecCopy})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	want := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", "in.webm", "-vn", "-map", "0:a:0",
		"-c:a", "copy", "out.webm",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args =\n  %v\nwant\n  %v", cmd.Args, want)
	}
}

func TestBuildCommand_Threads(t *testing.T) {
	cmd, err := buildCommand("in.webm", "out.flac", Spec{Codec: CodecFLAC, Threads: 2})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-threads", "2")
	// Zero threads emits no flag.
	cmd, _ = buildCommand("in.webm", "out.flac", Spec{Codec: CodecFLAC})
	if hasFlag(cmd.Args, "-threads") {
		t.Errorf("Threads=0 should emit no -threads flag: %v", cmd.Args)
	}
}

func TestBuildCommand_Lossless(t *testing.T) {
	cmd, err := buildCommand("in.webm", "out.flac", Spec{Codec: CodecFLAC})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	// No bitrate or quality args for a lossless encode.
	if hasFlag(cmd.Args, "-b:a") || hasFlag(cmd.Args, "-q:a") {
		t.Errorf("lossless command has rate/quality args: %v", cmd.Args)
	}
	assertSeq(t, cmd.Args, "-c:a", "flac")
	if cmd.Args[len(cmd.Args)-1] != "out.flac" {
		t.Errorf("output arg = %q, want out.flac", cmd.Args[len(cmd.Args)-1])
	}
}

func TestBuildCommand_ALACForcesIpodMuxer(t *testing.T) {
	// A bare ".alac" extension does not identify an ffmpeg muxer.
	cmd, err := buildCommand("in.flac", "out.alac", Spec{Codec: CodecALAC})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-c:a", "alac")
	assertSeq(t, cmd.Args, "-f", "ipod")
	if cmd.Args[len(cmd.Args)-1] != "out.alac" {
		t.Errorf("output arg = %q, want out.alac", cmd.Args[len(cmd.Args)-1])
	}
	// The muxer flag must precede the output filename.
	if fi := slices.Index(cmd.Args, "-f"); fi < 0 || fi >= len(cmd.Args)-1 {
		t.Errorf("-f muxer must come before output: %v", cmd.Args)
	}
}

func TestBuildCommand_NonALACHasNoForcedMuxer(t *testing.T) {
	cmd, err := buildCommand("in.webm", "out.flac", Spec{Codec: CodecFLAC})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if hasFlag(cmd.Args, "-f") {
		t.Errorf("non-ALAC command should not force a muxer: %v", cmd.Args)
	}
}

func TestBuildCommand_ALACValidContainerNotForced(t *testing.T) {
	for _, out := range []string{"out.m4a", "out.caf", "out.mp4", "out.M4A"} {
		cmd, err := buildCommand("in.flac", out, Spec{Codec: CodecALAC})
		if err != nil {
			t.Fatalf("buildCommand(%q): %v", out, err)
		}
		if hasFlag(cmd.Args, "-f") {
			t.Errorf("ALAC to %q should not force a muxer (ffmpeg infers it): %v", out, cmd.Args)
		}
	}
}

func TestBuildCommand_ALACExtensionlessForcesMuxer(t *testing.T) {
	cmd, err := buildCommand("in.flac", "out", Spec{Codec: CodecALAC})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-f", "ipod")
}

func TestRedactPath(t *testing.T) {
	staged := "/tmp/job/track-918273"
	final := "/home/u/track"
	base := &RunError{
		Binary:   "ffmpeg",
		ExitCode: 1,
		Stderr:   staged + ": Invalid data found when processing input",
		Err:      errors.New("exit status 1"),
	}
	if !strings.Contains(base.Error(), staged) {
		t.Fatalf("precondition: RunError message %q should contain the staged path", base.Error())
	}

	red := RedactPath(base, staged, final)
	if strings.Contains(red.Error(), staged) {
		t.Errorf("redacted message still contains the staged path: %q", red.Error())
	}
	if !strings.Contains(red.Error(), final) {
		t.Errorf("redacted message missing the final path: %q", red.Error())
	}
	// Redaction must preserve the error chain.
	if _, ok := errors.AsType[*RunError](red); !ok {
		t.Error("errors.AsType lost the *RunError after redaction")
	}

	// Inputs that cannot or need not be rewritten pass through unchanged.
	plain := errors.New("not a run error")
	if RedactPath(plain, staged, final) != plain {
		t.Error("RedactPath should pass a non-RunError through unchanged")
	}
	if got := RedactPath(base, "", final); got != error(base) {
		t.Error("empty from should be a no-op")
	}
	if got := RedactPath(base, final, final); got != error(base) {
		t.Error("from==to should be a no-op")
	}
}

func TestBuildCommand_ExtensionlessForcesCanonicalMuxer(t *testing.T) {
	// Without an extension, every encoding codec supplies its canonical muxer.
	cases := []struct {
		codec Codec
		muxer string
	}{
		{CodecFLAC, "flac"},
		{CodecMP3, "mp3"},
		{CodecWAV, "wav"},
		{CodecOpus, "opus"},
		{CodecAAC, "ipod"},
		{CodecVorbis, "ogg"},
		{CodecALAC, "ipod"},
	}
	for _, c := range cases {
		t.Run(c.codec.String(), func(t *testing.T) {
			cmd, err := buildCommand("in.webm", "out", Spec{Codec: c.codec})
			if err != nil {
				t.Fatalf("buildCommand: %v", err)
			}
			assertSeq(t, cmd.Args, "-f", c.muxer)
		})
	}
}

func TestBuildCommand_AACDotAACStaysADTS(t *testing.T) {
	// The .aac extension selects raw ADTS rather than the preset's M4A container.
	cmd, err := buildCommand("in.webm", "out.aac", Spec{Codec: CodecAAC})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if hasFlag(cmd.Args, "-f") {
		t.Errorf(".aac must stay raw ADTS (no forced muxer): %v", cmd.Args)
	}
}

func TestBuildCommand_VorbisDotVorbisForcesOgg(t *testing.T) {
	// .vorbis names a codec, so the output still needs the Ogg muxer.
	cmd, err := buildCommand("in.webm", "out.vorbis", Spec{Codec: CodecVorbis})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-f", "ogg")
}

func TestBuildCommand_ContainerExtensionsAuthoritative(t *testing.T) {
	// Real container extensions remain authoritative.
	cases := []struct {
		out   string
		codec Codec
	}{
		{"out.flac", CodecFLAC},
		{"out.mp3", CodecMP3},
		{"out.opus", CodecOpus},
		{"out.wav", CodecWAV},
		{"out.m4a", CodecAAC},
		{"out.ogg", CodecVorbis},
		{"out.m4a", CodecALAC},
	}
	for _, c := range cases {
		cmd, err := buildCommand("in.webm", c.out, Spec{Codec: c.codec})
		if err != nil {
			t.Fatalf("buildCommand(%q): %v", c.out, err)
		}
		if hasFlag(cmd.Args, "-f") {
			t.Errorf("%s to %q should infer the container (no -f): %v", c.codec, c.out, cmd.Args)
		}
	}
}

func TestBuildCommand_UnrecognizedExtensionForcesMuxer(t *testing.T) {
	// An extension that names no container, such as ".out", cannot be inferred, so
	// the selected preset's canonical muxer is forced.
	cmd, err := buildCommand("in.webm", "a.out", Spec{Codec: CodecFLAC})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-f", "flac")
}

func TestBuildCommand_MatroskaWebMContainersInferred(t *testing.T) {
	// WebM and Matroska are audio containers, and WebM holds YouTube's native Opus.
	// ffmpeg should infer those muxers from the extension instead of receiving the
	// preset's raw or default container.
	cases := []struct {
		out   string
		codec Codec
	}{
		{"track.webm", CodecOpus},
		{"track.mka", CodecFLAC},
		{"track.mka", CodecOpus},
		{"track.mkv", CodecAAC},
		// AIFF and Wave64 are PCM containers ffmpeg infers.
		{"track.aiff", CodecWAV},
		{"track.aif", CodecWAV},
		{"track.w64", CodecWAV},
	}
	for _, c := range cases {
		cmd, err := buildCommand("in.webm", c.out, Spec{Codec: c.codec})
		if err != nil {
			t.Fatalf("buildCommand(%q): %v", c.out, err)
		}
		if hasFlag(cmd.Args, "-f") {
			t.Errorf("%s to %q should infer the container (no -f): %v", c.codec, c.out, cmd.Args)
		}
	}
}

func TestBuildCommand_LossyDefaults(t *testing.T) {
	cases := []struct {
		name  string
		codec Codec
		want  []string // the codec + rate/quality args expected, in order
	}{
		{"mp3-v0", CodecMP3, []string{"-c:a", "libmp3lame", "-q:a", "0"}},
		{"aac-default", CodecAAC, []string{"-c:a", "aac", "-b:a", "256000"}},
		{"opus-default", CodecOpus, []string{"-c:a", "libopus", "-b:a", "192000"}},
		{"vorbis-q6", CodecVorbis, []string{"-c:a", "libvorbis", "-q:a", "6"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, err := buildCommand("in.m4a", "out", Spec{Codec: c.codec})
			if err != nil {
				t.Fatalf("buildCommand: %v", err)
			}
			assertSeq(t, cmd.Args, c.want...)
		})
	}
}

func TestBuildCommand_BitrateOverride(t *testing.T) {
	// An explicit bitrate forces CBR even for the VBR-default codecs.
	cmd, err := buildCommand("in.m4a", "out.mp3", Spec{Codec: CodecMP3, Bitrate: 320000})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-c:a", "libmp3lame", "-b:a", "320000")
	if hasFlag(cmd.Args, "-q:a") {
		t.Errorf("bitrate override should drop -q:a: %v", cmd.Args)
	}
}

func TestBuildCommand_BitrateIgnoredForLossless(t *testing.T) {
	cmd, err := buildCommand("in.webm", "out.flac", Spec{Codec: CodecFLAC, Bitrate: 320000})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if hasFlag(cmd.Args, "-b:a") {
		t.Errorf("lossless must ignore Bitrate: %v", cmd.Args)
	}
}

func TestBuildCommand_Filters(t *testing.T) {
	cmd, err := buildCommand("in.webm", "out.flac", Spec{
		Codec:   CodecFLAC,
		Filters: []string{"loudnorm=I=-14:linear=true", "aresample=48000"},
	})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-af", "loudnorm=I=-14:linear=true,aresample=48000")
}

func TestBuildCommand_CopyWithFiltersRejected(t *testing.T) {
	_, err := buildCommand("in.webm", "out.webm", Spec{
		Codec:   CodecCopy,
		Filters: []string{"loudnorm=I=-14"},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestBuildCommand_FilterComplex(t *testing.T) {
	graph := "[0:a:0]atrim=start=0.000000:end=5.000000,asetpts=PTS-STARTPTS[out]"
	cmd, err := buildCommand("in.webm", "out.flac", Spec{Codec: CodecFLAC, FilterComplex: graph})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	assertSeq(t, cmd.Args, "-filter_complex", graph, "-map", "[out]")
	assertSeq(t, cmd.Args, "-c:a", "flac")
	if hasFlag(cmd.Args, "-af") {
		t.Errorf("filter_complex path must not emit -af: %v", cmd.Args)
	}
	// The graph maps [out] explicitly, so the default -vn/-map 0:a:0 is dropped.
	if slices.Contains(cmd.Args, "0:a:0") {
		t.Errorf("filter_complex path should map [out] only, not 0:a:0: %v", cmd.Args)
	}
}

func TestBuildCommand_FilterComplexCopyRejected(t *testing.T) {
	_, err := buildCommand("in.webm", "out.webm", Spec{
		Codec:         CodecCopy,
		FilterComplex: "[0:a:0]anull[out]",
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestBuildCommand_FilterComplexAndFiltersRejected(t *testing.T) {
	// -af and -filter_complex on the same output are mutually exclusive.
	_, err := buildCommand("in.webm", "out.flac", Spec{
		Codec:         CodecFLAC,
		Filters:       []string{"loudnorm=I=-14"},
		FilterComplex: "[0:a:0]anull[out]",
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestBuildCommand_AlwaysAudioOnly(t *testing.T) {
	// Every command must drop video and pin the first audio stream so embedded
	// cover-art video streams can never be selected.
	for _, codec := range []Codec{CodecCopy, CodecFLAC, CodecMP3, CodecOpus} {
		cmd, err := buildCommand("in.mkv", "out", Spec{Codec: codec})
		if err != nil {
			t.Fatalf("buildCommand(%v): %v", codec, err)
		}
		if !hasFlag(cmd.Args, "-vn") {
			t.Errorf("%v: missing -vn: %v", codec, cmd.Args)
		}
		assertSeq(t, cmd.Args, "-map", "0:a:0")
	}
}

func TestBuildCommand_UnknownCodec(t *testing.T) {
	if _, err := buildCommand("in", "out", Spec{Codec: Codec(200)}); err == nil {
		t.Fatal("buildCommand(unknown codec) = nil error, want error")
	}
}

func TestCommandString(t *testing.T) {
	cmd := Command{Args: []string{"-i", "a.webm", "-c:a", "copy", "b.webm"}}
	if got := cmd.String(); got != "ffmpeg -i a.webm -c:a copy b.webm" {
		t.Errorf("String() = %q", got)
	}
}

func TestNewRunnerNotFound(t *testing.T) {
	_, err := NewRunner(RunnerConfig{
		FFmpegPath:  "/nonexistent/waxtap-ffmpeg",
		FFprobePath: "/nonexistent/waxtap-ffprobe",
	})
	if !errors.Is(err, waxerr.ErrFFmpegNotFound) {
		t.Fatalf("err = %v, want ErrFFmpegNotFound", err)
	}
}

func TestStartError(t *testing.T) {
	if err := startError("ffmpeg", exec.ErrNotFound); !errors.Is(err, waxerr.ErrFFmpegNotFound) {
		t.Errorf("not-found start error = %v, want ErrFFmpegNotFound", err)
	}
	other := startError("ffmpeg", errors.New("boom"))
	if errors.Is(other, waxerr.ErrFFmpegNotFound) {
		t.Errorf("generic start error wrongly classified as ErrFFmpegNotFound: %v", other)
	}
	if !strings.Contains(other.Error(), "start ffmpeg") {
		t.Errorf("generic start error = %q, want it to mention start ffmpeg", other)
	}
}

func TestClassifyRunPassthrough(t *testing.T) {
	// A non-exit error (e.g. the ctx error from the cancel path) passes through
	// unchanged rather than becoming a *RunError.
	got := classifyRun("ffmpeg", []string{"-i", "x"}, []byte("noise"), context.Canceled)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("got = %v, want context.Canceled", got)
	}
	if _, ok := errors.AsType[*RunError](got); ok {
		t.Fatalf("non-exit error wrongly wrapped as *RunError: %v", got)
	}
}

func TestRunError(t *testing.T) {
	base := errors.New("exit status 1")
	e := &RunError{
		Binary:   "ffmpeg",
		ExitCode: 1,
		Stderr:   "ignored line\nfatal: invalid argument\n",
		Err:      base,
	}
	msg := e.Error()
	if !strings.Contains(msg, "ffmpeg exited 1") {
		t.Errorf("Error() = %q, want it to mention exit code", msg)
	}
	if !strings.Contains(msg, "fatal: invalid argument") {
		t.Errorf("Error() = %q, want it to include the stderr tail", msg)
	}
	if !errors.Is(e, base) {
		t.Error("RunError should unwrap to its underlying error")
	}
}

func TestTailWriter(t *testing.T) {
	w := &tailWriter{max: 8}
	mustWrite(t, w, "abcd")
	mustWrite(t, w, "efgh")
	if got := string(w.bytes()); got != "abcdefgh" {
		t.Fatalf("after fill, bytes = %q, want abcdefgh", got)
	}
	// Exceeding max keeps only the last max bytes across writes.
	mustWrite(t, w, "ij")
	if got := string(w.bytes()); got != "cdefghij" {
		t.Fatalf("after overflow, bytes = %q, want cdefghij", got)
	}
	// A single write larger than max keeps only its own tail.
	mustWrite(t, w, "0123456789")
	if got := string(w.bytes()); got != "23456789" {
		t.Fatalf("after big write, bytes = %q, want 23456789", got)
	}
}

func TestTailWriterZeroMax(t *testing.T) {
	w := &tailWriter{max: 0}
	mustWrite(t, w, "anything")
	if len(w.bytes()) != 0 {
		t.Fatalf("zero-max tail retained %q", w.bytes())
	}
}

func mustWrite(t *testing.T, w *tailWriter, s string) {
	t.Helper()
	n, err := w.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", s, n, err, len(s))
	}
}

// hasFlag reports whether flag appears anywhere in args.
func hasFlag(args []string, flag string) bool {
	return slices.Contains(args, flag)
}

// assertSeq fails unless seq appears as a contiguous subsequence of args.
func assertSeq(t *testing.T, args []string, seq ...string) {
	t.Helper()
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return
		}
	}
	t.Errorf("args %v do not contain the sequence %v", args, seq)
}

func TestContainersForSubsetOfContainerAccepts(t *testing.T) {
	// Every container ContainersFor suggests for a codec must actually accept it,
	// so messages never recommend an incompatible extension. The probed PCM name
	// exercises the dual-name normalization.
	for _, codec := range []string{"flac", "wav", "pcm_s16le", "mp3", "aac", "alac", "opus", "vorbis"} {
		for _, ext := range ContainersFor(codec) {
			if !ContainerAccepts(ext, codec) {
				t.Errorf("ContainersFor(%q) suggests %q, but ContainerAccepts(%q,%q)=false", codec, ext, ext, codec)
			}
		}
	}
	// Unknown codecs yield no suggestion rather than a wrong one.
	if got := ContainersFor("ac3"); got != nil {
		t.Errorf("ContainersFor(ac3) = %v, want nil", got)
	}
}

func TestCheckOutputContainer(t *testing.T) {
	cases := []struct {
		codec   Codec
		output  string
		wantErr bool
	}{
		{CodecMP3, "out.flac", true},
		{CodecMP3, "out.wav", true},
		{CodecFLAC, "out.opus", true},
		{CodecMP3, "out.mp3", false},
		{CodecAAC, "out.m4a", false},
		{CodecOpus, "out.mka", false},
		{CodecWAV, "out.mka", false}, // dual-name: canonical "wav" satisfies the PCM branch
		{CodecWAV, "out.w64", false}, // outside the table: unchecked
		{CodecFLAC, "out.aiff", false},
		{CodecALAC, "out.alac", false}, // codec-named: force-muxed
		{CodecFLAC, "out", false},      // extensionless: force-muxed
		{CodecCopy, "out.flac", false}, // copy follows the source container
	}
	for _, c := range cases {
		err := CheckOutputContainer(c.codec, c.output)
		if c.wantErr {
			if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
				t.Errorf("CheckOutputContainer(%v,%q) = %v, want ErrIncompatibleSpec", c.codec, c.output, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("CheckOutputContainer(%v,%q) = %v, want nil", c.codec, c.output, err)
		}
	}
}
