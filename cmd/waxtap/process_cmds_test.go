package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/spf13/cobra"
)

// TestProcessSourceCheckedBeforeCollision verifies that a missing input is
// reported before an existing output path is considered.
func TestProcessSourceCheckedBeforeCollision(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.wav")
	existing := filepath.Join(dir, "existing.flac")
	if err := os.WriteFile(existing, []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"transcode", []string{"transcode", missing, "-f", "flac", "-o", existing}},
		{"cut", []string{"cut", missing, "--cut-range", "0-1", "-f", "flac", "-o", existing}},
		{"normalize", []string{"normalize", missing, "-f", "flac", "-o", existing}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			root.SetArgs(tc.args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			err := root.Execute()
			if err == nil {
				t.Fatal("expected an error for a missing source")
			}
			msg := err.Error()
			if !strings.Contains(msg, "no such file") {
				t.Errorf("error = %q, want it to report the missing source", msg)
			}
			if strings.Contains(msg, "already exists") {
				t.Errorf("error = %q, the existing output masked the missing source", msg)
			}
		})
	}
}

func TestDispatchProcessMangledPath(t *testing.T) {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	env := &appEnv{
		client: client,
		cfg:    &appConfig{},
		out:    io.Discard,
		errOut: io.Discard,
		log:    slog.New(slog.DiscardHandler),
	}

	// A non-existent path that is neither a YouTube URL nor an 11-character ID must be
	// reported as a missing file (usage, exit 2), not "invalid characters in
	// video ID". This returns before any network work.
	_, derr := dispatchProcess(context.Background(), env, "no such file.mp3",
		waxtap.BestAudio(), waxtap.MinimizeLoss(),
		waxtap.ProcessSpec{Output: waxtap.ToFile("out.flac")}, false)
	if _, ok := errors.AsType[*usageError](derr); !ok {
		t.Fatalf("err = %v (%T), want a usageError", derr, derr)
	}
	if !strings.Contains(derr.Error(), "no such file") {
		t.Errorf("message = %q, want it to mention the missing file", derr)
	}
}

func TestDispatchProcessIDLikeFilename(t *testing.T) {
	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatal(err)
	}
	env := &appEnv{
		client: client,
		cfg:    &appConfig{},
		out:    io.Discard,
		errOut: io.Discard,
		log:    slog.New(slog.DiscardHandler),
	}

	// A missing path whose stem is exactly an 11-character ID, matching the
	// --output-template shape, should stay a missing-file error. The same rule
	// applies when a separator or drive prefix appears before the ID.
	for _, source := range []string{
		"testVideo01.flac",
		"/tmp/x/testVideo01",
		"wrong name testVideo01",
		"D:testVideo01",
	} {
		t.Run(source, func(t *testing.T) {
			_, derr := dispatchProcess(context.Background(), env, source,
				waxtap.BestAudio(), waxtap.MinimizeLoss(),
				waxtap.ProcessSpec{Output: waxtap.ToFile("out.flac")}, false)
			if _, ok := errors.AsType[*usageError](derr); !ok {
				t.Fatalf("err = %v (%T), want a usageError", derr, derr)
			}
			if !strings.Contains(derr.Error(), "no such file") {
				t.Errorf("message = %q, want it to mention the missing file", derr)
			}
		})
	}
}

// runProcessCmd executes a process subcommand through the root command with
// discarded output and returns the error.
func runProcessCmd(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

// TestRejectStdoutOutput verifies that `-o -` and positional `-` are rejected on
// process commands before format inference.
func TestRejectStdoutOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	if err := os.WriteFile(in, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"transcode -o - no format", []string{"transcode", in, "-o", "-"}},
		{"transcode -o - with format", []string{"transcode", in, "-f", "wav", "-o", "-"}},
		{"transcode positional -", []string{"transcode", in, "-"}},
		{"normalize -o - no format", []string{"normalize", in, "-o", "-"}},
		{"normalize -o - with format", []string{"normalize", in, "-f", "flac", "-o", "-"}},
		{"cut -o - no format", []string{"cut", in, "--cut-range", "0-1", "-o", "-"}},
		{"cut -o - with format", []string{"cut", in, "--cut-range", "0-1", "-f", "flac", "-o", "-"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runProcessCmd(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), "stdout streaming") {
				t.Errorf("%v = %v, want the stdout-streaming rejection", tc.args, err)
			}
		})
	}
}

// TestRejectEmptyPathFlags checks that an explicitly empty --out/--dir (usually
// an unset shell/env $VAR) is a usage error on the process commands instead of a
// silent fallback to the beside-input default.
func TestRejectEmptyPathFlags(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	if err := os.WriteFile(in, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		flag string
		args []string
	}{
		{"transcode -o empty", "out", []string{"transcode", in, "-f", "flac", "-o", ""}},
		{"transcode -o whitespace", "out", []string{"transcode", in, "-f", "flac", "-o", "   "}},
		{"transcode --dir empty", "dir", []string{"transcode", in, "-f", "flac", "--dir", ""}},
		{"normalize -o empty", "out", []string{"normalize", in, "-f", "flac", "-o", ""}},
		{"normalize --dir empty", "dir", []string{"normalize", in, "-f", "flac", "--dir", ""}},
		{"cut -o empty", "out", []string{"cut", in, "--cut-range", "0-1", "-o", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runProcessCmd(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), "empty --"+tc.flag+" path") {
				t.Errorf("%v = %v, want an empty --%s usage error", tc.args, err, tc.flag)
			}
		})
	}
}

// TestChannelURLErrorPrecedence verifies that channel URLs fail as channel URLs,
// even when output-format validation would also fail later.
func TestChannelURLErrorPrecedence(t *testing.T) {
	const channel = "https://www.youtube.com/channel/UCabcdefghijklmnopqrstuv"
	cases := []struct {
		name string
		args []string
	}{
		{"transcode", []string{"transcode", channel}},
		{"normalize", []string{"normalize", channel}},
		{"cut with bitrate", []string{"cut", channel, "--cut-range", "0-1", "--bitrate", "128000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runProcessCmd(t, tc.args...)
			if !errors.Is(err, waxtap.ErrIsChannel) {
				t.Errorf("%v = %v, want ErrIsChannel", tc.args, err)
			}
			if err != nil && strings.Contains(err.Error(), "--format") {
				t.Errorf("%v = %v, the channel error must precede the format/bitrate error", tc.args, err)
			}
		})
	}
}

// TestCutInfersFormatFromExtension verifies that re-encoding cuts can infer their
// format from a recognized output extension.
func TestCutInfersFormatFromExtension(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac") // 1s sine

	// A crossfade with a recognized -o extension infers flac and runs to success.
	if err := runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--crossfade", "100ms", "-o", filepath.Join(dir, "out.flac")); err != nil {
		t.Errorf("crossfade cut into .flac should infer flac and run: %v", err)
	}
	// Accurate mode with -o out.mp3 infers mp3, and the inference runs before the
	// "--bitrate requires --format" check so --bitrate pairs with the inferred format.
	if err := runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--cut-mode", "accurate", "--bitrate", "128000", "-o", filepath.Join(dir, "out.mp3")); err != nil {
		t.Errorf("accurate cut into .mp3 should infer mp3 and run: %v", err)
	}
	// An unrecognized extension (.mka) is only a container hint: --format is still required.
	err := runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--crossfade", "100ms", "-o", filepath.Join(dir, "out.mka"))
	if err == nil || !strings.Contains(err.Error(), "--format") {
		t.Errorf("crossfade cut into .mka should still demand --format, got %v", err)
	}
	// The copy/remux pseudo-formats are not real container extensions, so
	// `-o out.copy` falls through to the "pass --format" error like any
	// unrecognized extension.
	err = runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--crossfade", "100ms", "-o", filepath.Join(dir, "out.copy"))
	if err == nil || !strings.Contains(err.Error(), "--format") {
		t.Errorf("crossfade cut into .copy should still demand --format, got %v", err)
	}
}

// TestBitDepthSurfaceAndGuards covers the flag's CLI seams: it exists on every
// re-encoding command, it needs a format to apply to, and out-of-range values are
// rejected with the exit-2 spec error rather than reaching the encoder.
func TestBitDepthSurfaceAndGuards(t *testing.T) {
	for _, c := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"download", newDownloadCmd()},
		{"cut", newCutCmd()},
		{"transcode", newTranscodeCmd()},
		{"normalize", newNormalizeCmd()},
	} {
		if c.cmd.Flags().Lookup("bit-depth") == nil {
			t.Errorf("%s should expose --bit-depth", c.name)
		}
	}

	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")

	// cut has no --format here, so the depth has nothing to apply to.
	err := runProcessCmd(t, "cut", in, "--cut-range", "0.3-0.5", "--bit-depth", "16", "-o", filepath.Join(dir, "a.flac"))
	if err == nil || !strings.Contains(err.Error(), "--bit-depth requires --format") {
		t.Errorf("cut --bit-depth without --format = %v, want the requires-format usage error", err)
	}

	for _, depth := range []string{"8", "20", "32"} {
		out := filepath.Join(dir, "d"+depth+".flac")
		err := runProcessCmd(t, "transcode", in, "--format", "flac", "--bit-depth", depth, "-o", out)
		if !errors.Is(err, waxtap.ErrIncompatibleSpec) {
			t.Errorf("--bit-depth %s = %v, want ErrIncompatibleSpec", depth, err)
		}
	}
	for _, depth := range []string{"16", "24"} {
		out := filepath.Join(dir, "ok"+depth+".flac")
		if err := runProcessCmd(t, "transcode", in, "--format", "flac", "--bit-depth", depth, "-o", out); err != nil {
			t.Errorf("--bit-depth %s = %v, want success", depth, err)
		}
	}
}

// TestCutModeCopyRejectsEncode covers F4 from the CLI: --cut-mode copy plus a
// re-encoding flag exits 2 instead of silently dropping the copy request. The
// coherent combinations must keep working.
func TestCutModeCopyRejectsEncode(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")

	rejected := [][]string{
		{"--cut-range", "0.1-0.3", "--cut-mode", "copy", "--format", "flac"},
		{"--cut-range", "0.1-0.3", "--cut-mode", "copy", "--format", "mp3"},
		{"--cut-range", "0.1-0.3", "--cut-mode", "copy", "--downmix", "--format", "flac"},
	}
	for i, args := range rejected {
		out := filepath.Join(dir, "rej"+strconv.Itoa(i)+".flac")
		full := append([]string{"cut", in}, args...)
		err := runProcessCmd(t, append(full, "-o", out)...)
		if !errors.Is(err, waxtap.ErrIncompatibleSpec) {
			t.Errorf("cut %v = %v, want ErrIncompatibleSpec", args, err)
		}
		if _, serr := os.Stat(out); serr == nil {
			t.Errorf("cut %v wrote %s despite rejection", args, out)
		}
	}

	// A copy cut with no transcode target keeps the source codec: the .flac
	// container cannot hold cut-remuxed FLAC, so this still exits 2, but with the
	// pre-existing container message rather than the new one.
	err := runProcessCmd(t, "cut", in, "--cut-range", "0.1-0.3", "--cut-mode", "copy", "-o", filepath.Join(dir, "bare.flac"))
	if !errors.Is(err, waxtap.ErrIncompatibleSpec) {
		t.Errorf("cut --cut-mode copy (no format) = %v, want ErrIncompatibleSpec", err)
	}
	if err != nil && strings.Contains(err.Error(), "cannot be combined with --format") {
		t.Errorf("bare copy cut got the transcode-target message: %v", err)
	}

	// --cut-mode smart with a format is the ordinary fused cut+encode.
	if err := runProcessCmd(t, "cut", in, "--cut-range", "0.1-0.3", "--cut-mode", "smart",
		"--format", "flac", "-o", filepath.Join(dir, "smart.flac")); err != nil {
		t.Errorf("cut --cut-mode smart --format flac = %v, want success", err)
	}
	// --format copy is a container remux, not an encode, so it stays legal. Opus is
	// the source codec because a packet-level cut needs one WaxFlow can copy-cut.
	opus := filepath.Join(dir, "in.opus")
	synthAudio(t, opus, "libopus")
	if err := runProcessCmd(t, "cut", opus, "--cut-range", "0.1-0.3", "--cut-mode", "copy",
		"--format", "copy", "-o", filepath.Join(dir, "remux.opus")); err != nil {
		t.Errorf("cut --cut-mode copy --format copy = %v, want success", err)
	}
}

// TestLocalChannelsRejected: --channels picks among a video's source streams, so
// on a local file it reached nothing. `--channels mono` on a 5.1 file delivered
// six channels and said nothing about it.
func TestLocalChannelsRejected(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")
	batchDir := filepath.Join(dir, "batch")
	if err := os.MkdirAll(batchDir, 0o777); err != nil {
		t.Fatal(err)
	}
	synthAudio(t, filepath.Join(batchDir, "a.flac"), "flac")

	for _, c := range []struct {
		name string
		dir  bool
		args []string
	}{
		{"transcode", false, []string{"transcode", in, "--format", "flac", "-o", filepath.Join(dir, "t.flac")}},
		{"normalize", false, []string{"normalize", in, "--format", "flac", "-o", filepath.Join(dir, "n.flac")}},
		{"cut", false, []string{"cut", in, "--cut-range", "0-0.3", "--format", "flac", "-o", filepath.Join(dir, "c.flac")}},
		{"transcode dir", true, []string{"transcode", batchDir, "--format", "flac", "--dir", filepath.Join(dir, "td")}},
		{"normalize dir", true, []string{"normalize", batchDir, "--format", "flac", "--dir", filepath.Join(dir, "nd")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := runProcessCmd(t, append(c.args, "--channels", "mono")...)
			if !isUsageError(err) {
				t.Fatalf("--channels mono on a local input = %v (%T), want a usageError", err, err)
			}
			// The message has to name the flag that does work here, or the only way
			// forward is to drop the request.
			if !strings.Contains(err.Error(), "--downmix") {
				t.Errorf("message = %q, want it to point at --downmix", err)
			}
			// One helper serves files and directories, so its message cannot name an
			// input kind the user did not pass.
			if c.dir && strings.Contains(err.Error(), "a local file;") {
				t.Errorf("message = %q, but the input was a directory", err)
			}
		})
	}

	// With --downmix the flag names the fold target, so it does reach the audio.
	if err := runProcessCmd(t, "transcode", in, "--format", "flac",
		"-o", filepath.Join(dir, "folded.flac"), "--channels", "mono", "--downmix"); err != nil {
		t.Errorf("--channels mono --downmix = %v, want success", err)
	}

	// A configured default exists to steer downloads; breaking local processing
	// with it would make the setting unusable.
	t.Setenv("WAXTAP_CHANNELS", "mono")
	if err := runProcessCmd(t, "transcode", in, "--format", "flac", "-o", filepath.Join(dir, "env.flac")); err != nil {
		t.Errorf("WAXTAP_CHANNELS with no --channels flag = %v, want success", err)
	}
}

// TestBatchDownmixIsDecidedPerFile: --downmix is a property of the source, not
// of the spec, so a batch cannot answer it once for every file. The ones that
// have channels to fold re-encode; the ones already at the target copy through.
func TestBatchDownmixIsDecidedPerFile(t *testing.T) {
	root := t.TempDir()
	synthChannels(t, filepath.Join(root, "mono.opus"), "libopus", 1)
	synthChannels(t, filepath.Join(root, "stereo.opus"), "libopus", 2)
	outDir := filepath.Join(root, "out")

	cmd := newTranscodeCmd()
	cmd.SetArgs([]string{root, "--format", "opus", "--dir", outDir, "--channels", "mono", "--downmix"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("transcode dir --downmix: %v\n%s", err, buf.String())
	}
	if got := buf.String(); !strings.Contains(got, "1 encoded, 1 copied") {
		t.Errorf("want one encoded and one copied, got:\n%s", got)
	}
	// Both land at the requested layout whichever route they took.
	for _, name := range []string{"mono.opus", "stereo.opus"} {
		if ch := probeChannels(t, filepath.Join(outDir, name)); ch != 1 {
			t.Errorf("%s output channels = %d, want 1", name, ch)
		}
	}
}

// TestBitDepthDefeatsBatchCopyThrough: a file already in the target codec is
// normally copied through untouched, which would silently drop a --bit-depth
// request. specChangesAudio sends it to the encoder instead.
func TestBitDepthDefeatsBatchCopyThrough(t *testing.T) {
	root := t.TempDir()
	synthAudio(t, filepath.Join(root, "a.flac"), "flac")
	outDir := filepath.Join(root, "out")

	cmd := newTranscodeCmd()
	cmd.SetArgs([]string{root, "--format", "flac", "--dir", outDir, "--bit-depth", "16"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("transcode dir --bit-depth: %v\n%s", err, buf.String())
	}
	if got := buf.String(); strings.Contains(got, "copied:") {
		t.Errorf("--bit-depth should force a re-encode, not a copy-through:\n%s", got)
	}
}

// TestCutRejectsDirectoryOutput verifies that cut reports directory outputs at
// the CLI boundary instead of surfacing a downstream IO error.
func TestCutRejectsDirectoryOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	if err := os.WriteFile(in, []byte("placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runProcessCmd(t, "cut", in, "--cut-range", "0-1", "-o", dir)
	if err == nil || !strings.Contains(err.Error(), "existing directory") {
		t.Errorf("cut into a directory output = %v, want the clean directory message", err)
	}
}

// roundTripErr is a transport that fails every request, so a download attempt
// returns fast without network access.
type roundTripErr struct{}

func (roundTripErr) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network")
}

// TestDispatchProcessNotesDroppedPlaylist verifies that process commands note an
// ignored playlist before starting the download.
func TestDispatchProcessNotesDroppedPlaylist(t *testing.T) {
	client, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: roundTripErr{}}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	env := &appEnv{
		client: client,
		cfg:    &appConfig{},
		out:    io.Discard,
		errOut: &buf,
		log:    slog.New(slog.DiscardHandler),
	}
	// The download fails on the injected transport; the note is emitted before it.
	_, _ = dispatchProcess(context.Background(), env,
		"https://www.youtube.com/watch?v=dummyVideo0&list=PLtest123456789",
		waxtap.BestAudio(), waxtap.MinimizeLoss(),
		waxtap.ProcessSpec{Output: waxtap.ToFile(filepath.Join(t.TempDir(), "out.flac"))}, false)
	if !strings.Contains(buf.String(), "ignoring playlist PLtest123456789") {
		t.Errorf("errOut = %q, want a dropped-playlist note", buf.String())
	}
}
