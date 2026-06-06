package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
)

// schemaVersion tags JSON output so callers can handle shape changes. Version 2
// adds audioQuality to format objects.
const schemaVersion = 2

// appEnv carries the per-invocation client, resolved config, IO writers, and
// logger. Commands obtain one with setup at the top of their RunE.
type appEnv struct {
	client *waxtap.Client
	cfg    *appConfig
	out    io.Writer // stdout: command results (human or JSON)
	errOut io.Writer // stderr: progress, logs, errors
	log    *slog.Logger
}

func (e *appEnv) jsonMode() bool { return e.cfg.json }
func (e *appEnv) quiet() bool    { return e.cfg.quiet }

// setup resolves configuration and builds the WaxTap client for a command.
func setup(cmd *cobra.Command) (*appEnv, error) {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return nil, err
	}
	log := newLogger(cmd.ErrOrStderr(), cfg)
	opts, err := cfg.options(log)
	if err != nil {
		return nil, err
	}
	client, err := waxtap.New(opts)
	if err != nil {
		return nil, err
	}
	return &appEnv{
		client: client,
		cfg:    cfg,
		out:    cmd.OutOrStdout(),
		errOut: cmd.ErrOrStderr(),
		log:    log,
	}, nil
}

// newLogger builds a slog logger whose level follows --quiet/--verbose. Logs use
// stderr so stdout remains reserved for command output.
func newLogger(w io.Writer, cfg *appConfig) *slog.Logger {
	level := slog.LevelWarn
	switch {
	case cfg.verbose:
		level = slog.LevelDebug
	case cfg.quiet:
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// emitJSON writes v as indented JSON followed by a newline.
func (e *appEnv) emitJSON(v any) error {
	return writeJSON(e.out, v)
}

// writeJSON writes v as indented JSON followed by a newline. It is used by
// commands that produce no network client (version, cache).
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printf writes human output to stdout (command results; not gated by --quiet).
func (e *appEnv) printf(format string, args ...any) {
	fmt.Fprintf(e.out, format, args...)
}

// info writes an informational line to stderr unless --quiet is set.
func (e *appEnv) info(format string, args ...any) {
	if e.quiet() {
		return
	}
	fmt.Fprintf(e.errOut, format, args...)
}

// jsonFloat marshals non-finite loudness values as null because encoding/json
// rejects them.
type jsonFloat float64

func (f jsonFloat) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

// humanBytes formats a byte count with a binary-magnitude unit.
func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	const unit = 1024
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanDuration formats a duration as H:MM:SS or M:SS.
func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0:00"
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// humanLUFS formats a loudness value, rendering non-finite (silent) as "n/a".
func humanLUFS(v float64) string {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", v)
}

// usageError marks a bad-arguments failure, which maps to exit code 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// jsonError is the --json error envelope.
type jsonError struct {
	SchemaVersion int `json:"schemaVersion"`
	Error         struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// renderError writes the final command error as JSON or as a human-readable line.
func renderError(w io.Writer, jsonMode bool, err error) {
	if err == nil {
		return
	}
	if jsonMode {
		var je jsonError
		je.SchemaVersion = schemaVersion
		je.Error.Code = errorCode(err)
		je.Error.Message = cleanMessage(err.Error())
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(je)
		return
	}
	fmt.Fprintf(w, "waxtap: %s\n", cleanMessage(friendlyError(err)))
	if hint := errorHint(err); hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", hint)
	}
}

// cleanMessage strips a redundant leading "waxtap: " before the CLI adds its own
// prefix.
func cleanMessage(msg string) string {
	return strings.TrimPrefix(msg, "waxtap: ")
}

// friendlyError returns a human message for an error, expanding common sentinels.
func friendlyError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, waxtap.ErrFFmpegNotFound):
		return "ffmpeg/ffprobe not found on PATH"
	case errors.Is(err, waxtap.ErrIsPlaylist):
		return "that is a playlist URL, not a single video"
	case errors.Is(err, waxtap.ErrNeedsPOToken):
		return "YouTube requires a PO token for this video and none is configured"
	case errors.Is(err, waxtap.ErrRateLimited):
		return "rate limited by YouTube; back off and retry later"
	}
	return err.Error()
}

// errorHint returns an optional next-step hint for an error.
func errorHint(err error) string {
	switch {
	case errors.Is(err, waxtap.ErrFFmpegNotFound):
		return "install ffmpeg (it bundles ffprobe) to use download/cut/transcode/normalize processing"
	case errors.Is(err, waxtap.ErrIsPlaylist):
		return "the download command expands playlist URLs automatically; info/formats take a single video"
	case errors.Is(err, waxtap.ErrNeedsPOToken):
		return "try a different video, or run `waxtap doctor` to check extraction health"
	}
	return ""
}

// errorCode returns a stable machine code for the --json error envelope.
func errorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, waxtap.ErrFFmpegNotFound):
		return "ffmpeg-not-found"
	case errors.Is(err, waxtap.ErrIsPlaylist):
		return "is-playlist"
	case errors.Is(err, waxtap.ErrNeedsPOToken):
		return "needs-po-token"
	case errors.Is(err, waxtap.ErrRateLimited):
		return "rate-limited"
	case errors.Is(err, waxtap.ErrVideoUnavailable):
		return "video-unavailable"
	case errors.Is(err, waxtap.ErrVideoRestricted):
		return "video-restricted"
	case errors.Is(err, waxtap.ErrLoginRequired):
		return "login-required"
	case errors.Is(err, waxtap.ErrLiveContent):
		return "live-content"
	case errors.Is(err, waxtap.ErrNoAudioFormats):
		return "no-audio-formats"
	case errors.Is(err, waxtap.ErrExtractionFailed):
		return "extraction-failed"
	case errors.Is(err, waxtap.ErrCipherSolve):
		return "cipher-solve"
	case errors.Is(err, waxtap.ErrIncompatibleSpec):
		return "incompatible-spec"
	case errors.Is(err, waxtap.ErrUnsupportedInput):
		return "unsupported-input"
	case errors.Is(err, waxtap.ErrInvalidVideoID), errors.Is(err, waxtap.ErrInvalidPlaylistID):
		return "invalid-input"
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return "usage"
	}
	return "error"
}

// exitCodeFor maps an error to a process exit code so scripts can branch on the
// failure class without parsing messages.
func exitCodeFor(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return 130
	case errors.Is(err, waxtap.ErrVideoUnavailable),
		errors.Is(err, waxtap.ErrVideoRestricted),
		errors.Is(err, waxtap.ErrLoginRequired),
		errors.Is(err, waxtap.ErrLiveContent),
		errors.Is(err, waxtap.ErrNoAudioFormats):
		return 3
	case errors.Is(err, waxtap.ErrExtractionFailed), errors.Is(err, waxtap.ErrCipherSolve):
		return 4
	case errors.Is(err, waxtap.ErrRateLimited):
		return 5
	case errors.Is(err, waxtap.ErrFFmpegNotFound):
		return 6
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return 2
	}
	return 1
}
