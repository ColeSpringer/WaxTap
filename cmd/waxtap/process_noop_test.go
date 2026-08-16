package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// runTranscode executes the transcode command through the root command (so the
// persistent --json/--quiet flags are wired up) with separated stdout/stderr.
func runTranscode(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"transcode"}, args...))
	var outBuf, errBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	err = root.Execute()
	return outBuf.String(), errBuf.String(), err
}

// probeCodec returns the first audio stream's codec name via the in-process
// engine (no external tools).
func probeCodec(t *testing.T, path string) string {
	t.Helper()
	r := media.NewRunner(media.RunnerConfig{})
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	a, _ := pr.AudioStream()
	return a.CodecName
}

// pcmMD5 decodes a file's audio to PCM (a WAV) and hashes it, so a stream copy
// and a re-encode of the same source are distinguishable (a lossy re-encode
// changes the samples; a copy decodes to the same PCM).
func pcmMD5(t *testing.T, path string) string {
	t.Helper()
	r := media.NewRunner(media.RunnerConfig{})
	wav := filepath.Join(t.TempDir(), "decoded.wav")
	if _, err := r.Transcode(context.Background(), path, wav, media.Spec{Codec: media.CodecWAV}); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	b, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// transcodedFalse reports whether a --json transcode result says no encoding was
// performed.
func transcodedFalse(t *testing.T, stdout string) bool {
	t.Helper()
	var got struct {
		Transcoded bool `json:"transcoded"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("unmarshal result JSON: %v\n%s", err, stdout)
	}
	return !got.Transcoded
}

func TestTranscodeSameFormatRemux(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")
	out := filepath.Join(dir, "out.flac")

	stdout, _, err := runTranscode(t, in, "--format", "flac", "-o", out, "--json")
	if err != nil {
		t.Fatalf("same-format transcode: %v", err)
	}
	if !transcodedFalse(t, stdout) {
		t.Errorf("same-format flac should not re-encode (want transcoded:false):\n%s", stdout)
	}
	if c := probeCodec(t, out); c != "flac" {
		t.Errorf("output codec = %q, want flac", c)
	}
	if a, b := pcmMD5(t, in), pcmMD5(t, out); a != b {
		t.Errorf("samples changed: in=%s out=%s", a, b)
	}

	// Human mode prints the copy note on stderr.
	_, stderr, err := runTranscode(t, in, "--format", "flac", "-o", filepath.Join(dir, "out2.flac"))
	if err != nil {
		t.Fatalf("same-format transcode (human): %v", err)
	}
	if !strings.Contains(stderr, "copied without re-encoding") {
		t.Errorf("missing no-op note on stderr:\n%s", stderr)
	}
}

func TestTranscodeContainerChangeRemux(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.webm")
	synthAudio(t, in, "libopus") // opus in a WebM container
	out := filepath.Join(dir, "out.opus")

	stdout, _, err := runTranscode(t, in, "--format", "opus", "-o", out, "--json")
	if err != nil {
		t.Fatalf("container-change remux: %v", err)
	}
	// transcoded:false proves the opus stream was copied, not re-encoded; a probe
	// confirms the .opus output is a valid opus file (not mislabeled). The decoded
	// PCM is not compared: opus preskip handling differs across containers even for
	// a pure stream copy, so it is not a reliable cross-container equality check.
	if !transcodedFalse(t, stdout) {
		t.Errorf("opus source -> .opus should remux, not re-encode:\n%s", stdout)
	}
	if c := probeCodec(t, out); c != "opus" {
		t.Errorf("output codec = %q, want opus", c)
	}
}

// headBytes returns the first n bytes of a file, for identifying a container by
// its magic.
func headBytes(t *testing.T, path string, n int) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < n {
		t.Fatalf("%s is %d bytes, want at least %d", path, len(b), n)
	}
	return b[:n]
}

// TestTranscodeNonInferableExtRemuxes: an output path whose extension does not
// name a container (".alac", or none at all) still takes the same-format remux.
// The container comes from the source codec, not the filename, so there is
// nothing for the encoder to supply that a copy lacks.
func TestTranscodeNonInferableExtRemuxes(t *testing.T) {
	dir := t.TempDir()
	alac := filepath.Join(dir, "in.m4a")
	synthAudio(t, alac, "alac") // ALAC source in an MP4 container

	out := filepath.Join(dir, "out.alac")
	stdout, _, err := runTranscode(t, alac, "--format", "alac", "-o", out, "--json")
	if err != nil {
		t.Fatalf("alac -> .alac: %v", err)
	}
	if !transcodedFalse(t, stdout) {
		t.Errorf("alac -> .alac should remux (want transcoded:false):\n%s", stdout)
	}
	if c := probeCodec(t, out); c != "alac" {
		t.Errorf("output codec = %q, want alac", c)
	}
	// ALAC always muxes into a progressive MP4, whatever the path is named.
	if got := string(headBytes(t, out, 8)[4:]); got != "ftyp" {
		t.Errorf(".alac output magic = %q, want an MP4 ftyp box", got)
	}

	// The same ALAC source into an inferable .m4a also remuxes, as it always did.
	out2 := filepath.Join(dir, "out2.m4a")
	stdout2, _, err := runTranscode(t, alac, "--format", "alac", "-o", out2, "--json")
	if err != nil {
		t.Fatalf("alac -> .m4a: %v", err)
	}
	if !transcodedFalse(t, stdout2) {
		t.Errorf("alac -> .m4a should remux (transcoded:false):\n%s", stdout2)
	}

	// An extensionless output takes the format's own default container: Ogg for
	// opus, the same one `-o out.opus` gets.
	opus := filepath.Join(dir, "in.webm")
	synthAudio(t, opus, "libopus")
	noext := filepath.Join(dir, "noext")
	stdout3, _, err := runTranscode(t, opus, "--format", "opus", "-o", noext, "--json")
	if err != nil {
		t.Fatalf("opus -> extensionless: %v", err)
	}
	if !transcodedFalse(t, stdout3) {
		t.Errorf("opus -> extensionless should remux (transcoded:false):\n%s", stdout3)
	}
	if c := probeCodec(t, noext); c != "opus" {
		t.Errorf("output codec = %q, want opus", c)
	}
	if got := string(headBytes(t, noext, 4)); got != "OggS" {
		t.Errorf("extensionless output magic = %q, want OggS", got)
	}
}

// TestInertKnobDoesNotForceReEncode: an encoding knob the target format's encoder
// never reads must not defeat the remux shortcut, or the run pays a generation of
// loss for a setting it also reports as ignored.
func TestInertKnobDoesNotForceReEncode(t *testing.T) {
	dir := t.TempDir()
	opus := filepath.Join(dir, "in.opus")
	synthAudio(t, opus, "libopus")
	flac := filepath.Join(dir, "in.flac")
	synthAudio(t, flac, "flac")

	cases := []struct {
		name   string
		source string
		format string
		extra  []string
	}{
		{"bit-depth on lossy", opus, "opus", []string{"--bit-depth", "16"}},
		{"bitrate on lossless", flac, "flac", []string{"--bitrate", "320000"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "-")+"."+c.format)
			args := append([]string{c.source, "--format", c.format, "-o", out, "--json"}, c.extra...)
			stdout, _, err := runTranscode(t, args...)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if !transcodedFalse(t, stdout) {
				t.Errorf("%s should remux (want transcoded:false):\n%s", c.name, stdout)
			}
		})
	}

	// A knob the encoder does read still re-encodes.
	out := filepath.Join(dir, "depth.flac")
	stdout, _, err := runTranscode(t, flac, "--format", "flac", "-o", out, "--bit-depth", "16", "--json")
	if err != nil {
		t.Fatalf("bit-depth on lossless: %v", err)
	}
	if transcodedFalse(t, stdout) {
		t.Errorf("--bit-depth on flac should re-encode (want transcoded:true):\n%s", stdout)
	}
}

// synthChannels writes a sine fixture with a given channel count, for the cases
// where the layout is the thing under test. synthAudio is always stereo.
func synthChannels(t *testing.T, path, encoder string, channels int) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src.wav")
	if err := os.WriteFile(src, mediatest.SineWAV(1, channels), 0o644); err != nil {
		t.Fatal(err)
	}
	r := media.NewRunner(media.RunnerConfig{})
	if _, err := r.Transcode(context.Background(), src, path, media.Spec{Codec: synthCodecFor(encoder)}); err != nil {
		t.Fatalf("synth %s (%dch): %v", path, channels, err)
	}
}

// probeChannels returns a file's channel count through the in-process engine.
func probeChannels(t *testing.T, path string) int {
	t.Helper()
	r := media.NewRunner(media.RunnerConfig{})
	pr, err := r.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe %s: %v", path, err)
	}
	a, _ := pr.AudioStream()
	return a.Channels
}

// TestInertDownmixDoesNotForceReEncode: --downmix folds a source that has more
// channels than the target. On one that already matches there is nothing to
// fold, and the engine computes fold = 0 either way, so paying a generation for
// it is the same defect an inert --bitrate used to cause.
func TestInertDownmixDoesNotForceReEncode(t *testing.T) {
	dir := t.TempDir()
	mono := filepath.Join(dir, "mono.opus")
	synthChannels(t, mono, "libopus", 1)
	stereo := filepath.Join(dir, "stereo.opus")
	synthChannels(t, stereo, "libopus", 2)

	// Nothing to fold: remux.
	monoOut := filepath.Join(dir, "mono-out.opus")
	stdout, _, err := runTranscode(t, mono, "--format", "opus", "-o", monoOut, "--channels", "mono", "--downmix", "--json")
	if err != nil {
		t.Fatalf("mono --downmix: %v", err)
	}
	if !transcodedFalse(t, stdout) {
		t.Errorf("a mono source folded to mono should remux (want transcoded:false):\n%s", stdout)
	}
	if got := probeChannels(t, monoOut); got != 1 {
		t.Errorf("output channels = %d, want 1", got)
	}

	// A real fold still re-encodes, and still folds.
	stereoOut := filepath.Join(dir, "stereo-out.opus")
	stdout, _, err = runTranscode(t, stereo, "--format", "opus", "-o", stereoOut, "--channels", "mono", "--downmix", "--json")
	if err != nil {
		t.Fatalf("stereo --downmix: %v", err)
	}
	if transcodedFalse(t, stdout) {
		t.Errorf("a stereo source folded to mono must re-encode (want transcoded:true):\n%s", stdout)
	}
	if got := probeChannels(t, stereoOut); got != 1 {
		t.Errorf("folded output channels = %d, want 1", got)
	}

	// A downmix to stereo leaves a stereo source alone.
	keepOut := filepath.Join(dir, "keep.opus")
	stdout, _, err = runTranscode(t, stereo, "--format", "opus", "-o", keepOut, "--channels", "stereo", "--downmix", "--json")
	if err != nil {
		t.Fatalf("stereo --downmix stereo: %v", err)
	}
	if !transcodedFalse(t, stdout) {
		t.Errorf("a stereo source folded to stereo should remux (want transcoded:false):\n%s", stdout)
	}
}

// TestKnobEffectDrivesWarnings pins the note to the classification: every format
// whose encoder does not read a knob says so, only the values that really reach
// nothing are called "ignored", and the reason given is the one that classified
// the format rather than a neighbour's.
func TestKnobEffectDrivesWarnings(t *testing.T) {
	formats := append([]string{"copy"}, transcodeFormatNames...)
	for _, name := range formats {
		tf, err := parseTranscodeFormat(name)
		if err != nil {
			t.Fatalf("parse %q: %v", name, err)
		}
		knobs := []struct {
			flag string
			note knobNote
			warn func(*appEnv)
		}{
			{"--bitrate", bitrateEffect(tf), func(e *appEnv) { warnBitrateIgnored(e, tf, 320000) }},
			{"--bit-depth", bitDepthEffect(tf), func(e *appEnv) { warnBitDepthIgnored(e, tf, 16) }},
		}
		for _, k := range knobs {
			var buf bytes.Buffer
			k.warn(&appEnv{cfg: &appConfig{}, out: io.Discard, errOut: &buf})
			got := buf.String()
			if noted := got != ""; noted != (k.note.effect != knobHonored) {
				t.Errorf("%s on --format %s: note=%q, effect=%d; a note must appear iff the encoder does not read the value", k.flag, name, got, k.note.effect)
			}
			if said := strings.Contains(got, "is ignored"); said != (k.note.effect == knobIgnored) {
				t.Errorf("%s on --format %s: note=%q, effect=%d; \"is ignored\" must appear iff the value reaches nothing", k.flag, name, got, k.note.effect)
			}
			// A lossy target told "ignored for lossless targets" is a note that came
			// from a different row than the classification did.
			if lossless := isLosslessFormat(tf); strings.Contains(got, "lossless") && !lossless {
				t.Errorf("%s on --format %s: note=%q calls a lossy target lossless", k.flag, name, got)
			} else if strings.Contains(got, "lossy") && lossless {
				t.Errorf("%s on --format %s: note=%q calls a lossless target lossy", k.flag, name, got)
			}
		}
	}
}

// TestMeasureOnlyOmitsOutputFormat: nothing was written, so there is no output
// format to describe. The local-file shape already omitted it (formatDTOs gates
// on Transcoded); a URL source reported one for a run that sank to io.Discard.
func TestMeasureOnlyOmitsOutputFormat(t *testing.T) {
	measured := &waxtap.Result{
		SourceKind:       waxtap.SourceYouTube,
		VideoID:          "dummyVideo0",
		SourceFormat:     waxtap.Format{Itag: 251, Codec: "opus", Extension: "webm"},
		OutputFormat:     waxtap.Format{Itag: 251, Codec: "opus", Extension: "webm"},
		SourceBytes:      4096,
		OutputBytes:      4096,
		LoudnessMeasured: true,
	}
	var buf bytes.Buffer
	env := &appEnv{cfg: &appConfig{json: true}, out: &buf, errOut: io.Discard}
	if err := emitResult(env, measured); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if _, ok := doc["outputFormat"]; ok {
		t.Errorf("outputFormat = %v, want the key omitted for a run that wrote nothing", doc["outputFormat"])
	}
	if doc["outputBytes"] != float64(0) {
		t.Errorf("outputBytes = %v, want 0", doc["outputBytes"])
	}
	if _, ok := doc["sourceFormat"]; !ok {
		t.Error("sourceFormat was dropped; the measured input still has one")
	}

	// A run that did deliver audio keeps both.
	delivered := *measured
	delivered.OutputPath = "out.opus"
	buf.Reset()
	if err := emitResult(env, &delivered); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if _, ok := doc["outputFormat"]; !ok {
		t.Error("outputFormat was dropped from a run that wrote a file")
	}
	if doc["outputBytes"] != float64(4096) {
		t.Errorf("outputBytes = %v, want 4096", doc["outputBytes"])
	}
}

func TestTranscodeProbeFailureNotCopiedThrough(t *testing.T) {
	dir := t.TempDir()
	// A file with an audio extension but unreadable content: ProbeAudio fails, so
	// the no-op shortcut must fall through to the encode rather than copying
	// unreadable bytes as a successful same-codec output.
	in := filepath.Join(dir, "garbage.flac")
	if err := os.WriteFile(in, []byte("not real flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.flac")

	_, _, err := runTranscode(t, in, "--format", "flac", "-o", out)
	if err == nil {
		t.Fatal("garbage input was accepted as a same-format copy; want an encode error")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("a failed encode must not leave an output file")
	}
}

func TestTranscodeQuietPrintsOnlyPath(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")
	out := filepath.Join(dir, "out.mp3")

	stdout, _, err := runTranscode(t, in, "--format", "mp3", "-o", out, "--quiet")
	if err != nil {
		t.Fatalf("quiet transcode: %v", err)
	}
	if got := strings.TrimRight(stdout, "\n"); got != out {
		t.Errorf("quiet stdout = %q, want exactly the output path %q", stdout, out)
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("quiet stdout should be exactly one line, got %q", stdout)
	}

	// --quiet --json still prints the full JSON document to stdout.
	out2 := filepath.Join(dir, "out2.mp3")
	jstdout, _, err := runTranscode(t, in, "--format", "mp3", "-o", out2, "--quiet", "--json")
	if err != nil {
		t.Fatalf("quiet json transcode: %v", err)
	}
	var got struct {
		OutputPath string `json:"outputPath"`
		Transcoded bool   `json:"transcoded"`
	}
	if err := json.Unmarshal([]byte(jstdout), &got); err != nil {
		t.Fatalf("quiet --json stdout is not the full JSON document: %v\n%s", err, jstdout)
	}
	if got.OutputPath != out2 || !got.Transcoded {
		t.Errorf("quiet --json result = %+v, want full document with outputPath=%q transcoded=true", got, out2)
	}
}

func TestTranscodeForceBitrateDownmixBypassRemux(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.mp3")
	synthAudio(t, in, "libmp3lame")

	cases := []struct {
		name  string
		extra []string
	}{
		{"force", []string{"--force"}},
		{"bitrate", []string{"--bitrate", "128000"}},
		{"downmix", []string{"--downmix", "--channels", "mono"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(dir, c.name+".mp3")
			args := append([]string{in, "--format", "mp3", "-o", out, "--json"}, c.extra...)
			stdout, _, err := runTranscode(t, args...)
			if err != nil {
				t.Fatalf("%s transcode: %v", c.name, err)
			}
			if transcodedFalse(t, stdout) {
				t.Errorf("%s should re-encode (want transcoded:true):\n%s", c.name, stdout)
			}
		})
	}
}
