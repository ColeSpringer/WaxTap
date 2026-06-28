package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// Defaults applied by NewRunner when a RunnerConfig field is left zero.
const (
	defaultShutdownGrace = 5 * time.Second
	stderrTailMax        = 256 << 10 // bytes of stderr retained for diagnostics
)

// Command is a built ffmpeg invocation. Args excludes the binary name.
type Command struct {
	Args []string // ffmpeg arguments, excluding the binary name
}

func (c Command) String() string { return "ffmpeg " + strings.Join(c.Args, " ") }

// Spec describes a transcode. Codec selects the output codec, Bitrate overrides
// lossy preset defaults in bits/sec, and the optional filter fields run before
// encoding. The zero value is a stream copy with no filters.
type Spec struct {
	Codec   Codec // output codec; zero selects stream copy
	Bitrate int   // target bits per second for lossy codecs; zero uses the preset default
	// Channels sets the output channel count with -ac when positive. Changing the
	// channel count requires encoding.
	Channels int
	// Filters is a comma-joined -af chain for linear audio filters such as
	// loudnorm. It is mutually exclusive with FilterComplex.
	Filters []string
	// FilterComplex is a complete -filter_complex graph. The graph must read the
	// source audio from [0:a:0] and write the final audio to [out]; buildCommand
	// maps [out] as the only output stream. Use it for labeled or multi-input
	// graphs such as concat/acrossfade. It is mutually exclusive with Filters and
	// cannot be used with CodecCopy.
	FilterComplex string
	// StageExt sets the staged file's extension, with or without a leading dot.
	// An empty value derives the extension from the output path.
	StageExt string
	// Threads limits ffmpeg's worker threads when positive. Zero lets ffmpeg choose.
	Threads int
}

// buildCommand assembles the ffmpeg arguments to read input, apply spec's
// filters, encode per spec, and write output. It always selects a single audio
// stream (-vn -map 0:a:0) so an embedded cover-art video stream cannot be picked
// by mistake. A stream copy combined with filters is rejected, because filtering
// requires a re-encode.
func buildCommand(input, output string, spec Spec) (Command, error) {
	return buildCommandWith(input, output, spec, "")
}

// buildCommandWith is buildCommand with an optional encoder override. An empty
// override uses the preset's encoder; Transcode passes a resolved encoder for
// codecs whose precision depends on the source (currently WAV bit depth).
func buildCommandWith(input, output string, spec Spec, encoderOverride string) (Command, error) {
	p, err := presetFor(spec.Codec)
	if err != nil {
		return Command{}, err
	}
	isCopy := spec.Codec == CodecCopy
	hasAF := len(spec.Filters) > 0
	hasFC := spec.FilterComplex != ""
	switch {
	case hasAF && hasFC:
		return Command{}, fmt.Errorf("%w: -af filters and filter_complex are mutually exclusive", waxerr.ErrIncompatibleSpec)
	case isCopy && (hasAF || hasFC):
		return Command{}, fmt.Errorf("%w: stream copy cannot apply audio filters", waxerr.ErrIncompatibleSpec)
	case isCopy && spec.Channels > 0:
		return Command{}, fmt.Errorf("%w: stream copy cannot change the channel count", waxerr.ErrIncompatibleSpec)
	}
	encoder := p.encoder
	if encoderOverride != "" {
		encoder = encoderOverride
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", input}
	if spec.Threads > 0 {
		args = append(args, "-threads", strconv.Itoa(spec.Threads))
	}
	switch {
	case hasFC:
		// The graph owns audio selection and writes [out]. Mapping only [out]
		// keeps cover-art/video streams out of audio-only outputs.
		args = append(args, "-filter_complex", spec.FilterComplex, "-map", "[out]")
	default:
		args = append(args, "-vn", "-map", "0:a:0")
		if hasAF {
			args = append(args, "-af", strings.Join(spec.Filters, ","))
		}
	}
	if spec.Channels > 0 {
		// -ac folds (or expands) channels with ffmpeg's normalized downmix matrix.
		args = append(args, "-ac", strconv.Itoa(spec.Channels))
	}
	args = append(args, "-c:a", encoder)
	if !isCopy && !p.lossless {
		switch {
		case spec.Bitrate > 0:
			args = append(args, "-b:a", strconv.Itoa(spec.Bitrate))
		case len(p.qualityArgs) > 0:
			args = append(args, p.qualityArgs...)
		case p.defaultRate > 0:
			args = append(args, "-b:a", strconv.Itoa(p.defaultRate))
		}
	}
	// Force a muxer only when the output path cannot identify one. Explicit
	// container extensions such as ".m4a" and ".caf" remain authoritative.
	if p.muxer != "" && needsForcedMuxer(output) {
		args = append(args, "-f", p.muxer)
	}
	args = append(args, output)
	return Command{Args: args}, nil
}

// inferableContainers are output extensions ffmpeg maps to a muxer on its own, so
// buildCommand leaves them authoritative. Other extensions, whether codec names
// like ".vorbis" and ".alac" or unrelated names like ".out", get the preset's
// canonical muxer.
//
// This is the set of audio containers ffmpeg can infer, which is broader than
// (and serves a different purpose from) pipeline's stream-copy compatibility
// table: it includes the Matroska/WebM family that holds YouTube's native Opus,
// AIFF/Wave64 PCM containers, and ".caf" for ALAC. ".aac" stays for raw ADTS, and
// ".caf"/".m4a" let ffmpeg pick ALAC's container. The list is intentionally
// conservative; a less common inferable container outside it will be force-muxed.
var inferableContainers = map[string]bool{
	"mp3": true, "flac": true, "wav": true, "m4a": true, "m4b": true,
	"mp4": true, "aac": true, "ogg": true, "oga": true, "opus": true,
	"webm": true, "mka": true, "mkv": true, "caf": true,
	"aiff": true, "aif": true, "w64": true,
}

// needsForcedMuxer reports whether ffmpeg needs the preset's -f argument because
// the output path does not identify a container it can infer.
//
// An empty extension, a codec-name extension such as ".vorbis" or ".alac", and
// an unrelated one such as ".out" all force the canonical muxer. A recognized
// audio-container extension stays authoritative so ffmpeg infers it, preserving
// cases like the .caf versus .m4a split for ALAC.
func needsForcedMuxer(output string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	return !inferableContainers[ext]
}

// CanInferContainer reports whether path's extension selects a container ffmpeg
// can infer. When false, as with extensionless or codec-named paths such as
// ".alac", stream copy has no forced muxer to rely on. Callers should encode in
// that case; encode presets provide the muxer explicitly.
func CanInferContainer(path string) bool {
	return !needsForcedMuxer(path)
}

// ContainerAccepts reports whether the container named by ext can stream-copy the
// given codec. Some extensions support several codecs, so this consults a
// container-compatibility table rather than comparing names; unknown extensions
// are left for ffmpeg to validate (permissive).
//
// Keep all stream-copy container checks routed here. CheckOutputContainer uses it
// before encoding starts, and the pipeline uses it before copying an already
// probed source. codecName may be an ffprobe name such as "pcm_s16le" or "opus",
// or a canonical Codec.String() value such as "wav". The two forms only differ
// for PCM, so every PCM branch accepts both spellings.
func ContainerAccepts(ext, codecName string) bool {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	c := strings.ToLower(codecName)
	isPCM := c == "wav" || strings.HasPrefix(c, "pcm")
	switch ext {
	case "flac":
		return c == "flac"
	case "wav":
		return isPCM
	case "mp3":
		return c == "mp3"
	case "m4a", "mp4", "m4b":
		return c == "aac" || c == "alac"
	case "aac":
		// .aac selects the raw ADTS muxer, which carries AAC only (not ALAC).
		return c == "aac"
	case "ogg", "oga":
		return c == "vorbis" || c == "opus" || c == "flac"
	case "opus":
		return c == "opus"
	case "webm":
		return c == "opus" || c == "vorbis"
	case "mka", "mkv":
		// Matroska is a general-purpose container; accept the codecs WaxTap handles.
		switch c {
		case "opus", "vorbis", "aac", "flac", "mp3", "alac":
			return true
		}
		return isPCM
	}
	return true
}

// ContainersFor returns a short list of conventional container extensions, each
// with a leading dot, that can stream-copy codecName. codecName may be an
// ffprobe name such as "pcm_s16le" or a canonical codec name such as "wav"; PCM
// is normalized here. The result is a subset of the extensions ContainerAccepts
// allows. Unknown codecs return nil.
func ContainersFor(codecName string) []string {
	c := strings.ToLower(codecName)
	switch {
	case c == "flac":
		return []string{".flac", ".mka"}
	case c == "wav" || strings.HasPrefix(c, "pcm"):
		return []string{".wav", ".mka"}
	case c == "mp3":
		return []string{".mp3", ".mka"}
	case c == "aac":
		return []string{".m4a", ".aac", ".mka"}
	case c == "alac":
		return []string{".m4a", ".mka"}
	case c == "opus":
		return []string{".opus", ".webm", ".ogg", ".mka"}
	case c == "vorbis":
		return []string{".ogg", ".webm", ".mka"}
	}
	return nil
}

// CheckOutputContainer reports whether output's extension can hold codec's audio.
// CodecCopy passes because its container follows the source. An extensionless or
// codec-named output is force-muxed, so its extension does not constrain the
// container. A recognized extension that cannot hold codec returns
// waxerr.ErrIncompatibleSpec with suggested containers. Extensions outside the
// table pass unchecked; ffmpeg reports the final error if they are invalid.
func CheckOutputContainer(codec Codec, output string) error {
	if codec == CodecCopy || needsForcedMuxer(output) {
		return nil
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	if ContainerAccepts(ext, codec.String()) {
		return nil
	}
	return fmt.Errorf("%w: the output extension .%s cannot hold %s audio; use one of %s",
		waxerr.ErrIncompatibleSpec, ext, codec, strings.Join(ContainersFor(codec.String()), ", "))
}

// RunnerConfig configures a Runner. The binary paths are looked up in PATH when
// left empty.
type RunnerConfig struct {
	// FFmpegPath and FFprobePath override the binaries; empty looks them up in
	// PATH by name.
	FFmpegPath  string
	FFprobePath string // ffprobe binary path or name
	// ShutdownGrace is how long a canceled process is given to exit after SIGTERM
	// before it is force-killed (default 5s).
	ShutdownGrace time.Duration
	// MaxProcs bounds concurrent ffmpeg/ffprobe processes (0 = unlimited). It
	// guards local CPU independently from network parallelism.
	MaxProcs int
	// Logger receives debug logs. Nil discards them.
	Logger *slog.Logger
}

// Runner executes ffmpeg and ffprobe. It resolves the binaries once, bounds the
// number of concurrent processes, captures a bounded tail of stderr for
// diagnostics, and terminates the process on context cancellation. On Unix it
// uses a process group so helper children are signaled with the ffmpeg process.
// Runner is safe for concurrent use.
type Runner struct {
	ffmpegPath  string
	ffprobePath string
	shutdown    time.Duration
	sem         chan struct{}
	log         *slog.Logger
}

// NewRunner resolves the ffmpeg and ffprobe binaries and returns a Runner. It
// returns a wrapped waxerr.ErrFFmpegNotFound if either binary cannot be located.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	ffmpeg, err := resolveBinary(cfg.FFmpegPath, "ffmpeg")
	if err != nil {
		return nil, err
	}
	ffprobe, err := resolveBinary(cfg.FFprobePath, "ffprobe")
	if err != nil {
		return nil, err
	}
	grace := cfg.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var sem chan struct{}
	if cfg.MaxProcs > 0 {
		sem = make(chan struct{}, cfg.MaxProcs)
	}
	return &Runner{
		ffmpegPath:  ffmpeg,
		ffprobePath: ffprobe,
		shutdown:    grace,
		sem:         sem,
		log:         log,
	}, nil
}

// FFmpegPath returns the resolved ffmpeg binary path.
func (r *Runner) FFmpegPath() string { return r.ffmpegPath }

// FFprobePath returns the resolved ffprobe binary path.
func (r *Runner) FFprobePath() string { return r.ffprobePath }

func resolveBinary(explicit, name string) (string, error) {
	target := explicit
	if target == "" {
		target = name
	}
	// LookPath validates an explicit path (when it contains a separator) and
	// otherwise searches PATH, in both cases checking executability.
	path, err := exec.LookPath(target)
	if err != nil {
		return "", fmt.Errorf("%w: %s (%v)", waxerr.ErrFFmpegNotFound, name, err)
	}
	return path, nil
}

// RunResult holds captured output from an ffmpeg run. Stderr is a bounded tail.
type RunResult struct {
	Stdout []byte // captured standard output
	Stderr []byte // bounded tail of standard error
}

// Run executes ffmpeg with cmd's arguments. A non-zero exit becomes a *RunError
// carrying the stderr tail; a canceled context returns ctx.Err() after the child
// process has been stopped.
func (r *Runner) Run(ctx context.Context, cmd Command) (RunResult, error) {
	stdout, stderr, err := r.run(ctx, r.ffmpegPath, cmd.Args, false)
	res := RunResult{Stdout: stdout, Stderr: stderr}
	if err != nil {
		return res, classifyRun("ffmpeg", cmd.Args, stderr, err)
	}
	return res, nil
}

// run starts the process, pumps stderr into a bounded tail (and stdout into a
// buffer when wantStdout), and waits. On context cancellation it sends the
// platform's graceful termination signal, force-kills after the shutdown grace,
// and returns ctx.Err().
func (r *Runner) run(ctx context.Context, bin string, args []string, wantStdout bool) (stdout, stderr []byte, err error) {
	if err := r.acquire(ctx); err != nil {
		return nil, nil, err
	}
	defer r.release()

	cmd := exec.Command(bin, args...)
	setProcessGroup(cmd)
	var outBuf bytes.Buffer
	if wantStdout {
		cmd.Stdout = &outBuf
	}
	errTail := &tailWriter{max: stderrTailMax}
	cmd.Stderr = errTail

	if err := cmd.Start(); err != nil {
		return nil, nil, startError(bin, err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		r.log.Debug("transcode: canceling process", "bin", bin, "pid", cmd.Process.Pid)
		_ = terminate(cmd.Process)
		timer := time.NewTimer(r.shutdown)
		defer timer.Stop()
		select {
		case <-waitCh:
		case <-timer.C:
			r.log.Debug("transcode: force-killing process after grace", "bin", bin, "pid", cmd.Process.Pid)
			_ = kill(cmd.Process)
			<-waitCh
		}
		return outBuf.Bytes(), errTail.bytes(), ctx.Err()
	case werr := <-waitCh:
		return outBuf.Bytes(), errTail.bytes(), werr
	}
}

func (r *Runner) acquire(ctx context.Context) error {
	if r.sem == nil {
		return nil
	}
	select {
	case r.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) release() {
	if r.sem != nil {
		<-r.sem
	}
}

// startError maps a failure to start the binary to ErrFFmpegNotFound when the
// binary is missing, otherwise wraps it.
func startError(bin string, err error) error {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s: %v", waxerr.ErrFFmpegNotFound, bin, err)
	}
	return fmt.Errorf("transcode: start %s: %w", bin, err)
}

// classifyRun turns a non-zero exit into a *RunError and passes other errors
// (already-wrapped start errors, or ctx errors from the cancel path) through.
func classifyRun(bin string, args []string, stderr []byte, err error) error {
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return &RunError{
			Binary:   bin,
			Args:     append([]string(nil), args...),
			ExitCode: exitErr.ExitCode(),
			Stderr:   string(stderr),
			Err:      err,
		}
	}
	return err
}

// RunError reports a non-zero ffmpeg/ffprobe exit and includes a bounded stderr
// tail for diagnosis.
type RunError struct {
	Binary   string   // executable name
	Args     []string // arguments passed to the executable
	ExitCode int      // process exit code
	Stderr   string   // bounded tail of standard error
	Err      error    // underlying process error
}

func (e *RunError) Error() string {
	tail := lastLines(strings.TrimSpace(e.Stderr), 4)
	if tail != "" {
		return fmt.Sprintf("transcode: %s exited %d: %s", e.Binary, e.ExitCode, tail)
	}
	return fmt.Sprintf("transcode: %s exited %d", e.Binary, e.ExitCode)
}

func (e *RunError) Unwrap() error { return e.Err }

// emptyDecodeFailure reports whether ffmpeg accepted an input but produced no
// decodable audio frames.
func emptyDecodeFailure(err error) bool {
	runErr, ok := errors.AsType[*RunError](err)
	if !ok {
		return false
	}
	tail := strings.ToLower(runErr.Stderr)
	for _, phrase := range []string{
		"could not open encoder before eof",
		"nothing was written into output file",
		"output file is empty",
		"output file does not contain any stream",
	} {
		if strings.Contains(tail, phrase) {
			return true
		}
	}
	return false
}

// RedactPath replaces occurrences of from with to in the message of a RunError
// chain. It preserves wrapping and returns other errors unchanged.
func RedactPath(err error, from, to string) error {
	if err == nil || from == "" || from == to {
		return err
	}
	if _, ok := errors.AsType[*RunError](err); !ok {
		return err
	}
	return &redactedError{err: err, from: from, to: to}
}

// redactedError rewrites the wrapped error's message while preserving its chain.
type redactedError struct {
	err      error
	from, to string
}

func (e *redactedError) Error() string { return strings.ReplaceAll(e.err.Error(), e.from, e.to) }

func (e *redactedError) Unwrap() error { return e.err }

// lastLines returns at most n trailing non-empty lines of s, collapsed onto one
// line for a compact error message.
func lastLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			kept = append([]string{t}, kept...)
		}
	}
	return strings.Join(kept, "; ")
}

// stderrSummary builds a short human message from a captured stderr tail,
// falling back to the underlying error when stderr is empty.
func stderrSummary(stderr []byte, fallback error) string {
	if s := lastLines(strings.TrimSpace(string(stderr)), 4); s != "" {
		return s
	}
	return fallback.Error()
}

// tailWriter retains only the last max bytes written to it. That keeps large
// ffmpeg stderr streams bounded while preserving the trailing diagnostics,
// including loudnorm's JSON block.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if w.max <= 0 {
		return len(p), nil
	}
	if len(p) >= w.max {
		w.buf = append(w.buf[:0], p[len(p)-w.max:]...)
		return len(p), nil
	}
	if len(w.buf)+len(p) > w.max {
		drop := len(w.buf) + len(p) - w.max
		w.buf = append(w.buf[:0], w.buf[drop:]...) // shift the retained tail left
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *tailWriter) bytes() []byte { return w.buf }
