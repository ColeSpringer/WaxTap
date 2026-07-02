package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/colespringer/waxtap/v2"
	"github.com/colespringer/waxtap/v2/internal/iox"
	"github.com/colespringer/waxtap/v2/internal/tempfile"
	"github.com/colespringer/waxtap/v2/youtube"
	"github.com/spf13/cobra"
)

// schemaVersion tags JSON output so callers can handle shape changes. Version 1 is
// the pre-1.0 baseline. Non-transcoded local results omit the redundant
// outputFormat field, and local formats omit itag because they do not come from a
// YouTube format.
const schemaVersion = 1

// appEnv carries the per-invocation client, resolved config, IO writers, and
// logger. Commands obtain one with setup at the top of their RunE.
type appEnv struct {
	client *waxtap.Client
	cfg    *appConfig
	out    io.Writer // stdout: command results (human or JSON)
	errOut io.Writer // stderr: progress, logs, errors
	log    *slog.Logger
	// audioStream is set when stdout carries streamed audio (download -o -). A
	// measure-only run to a real writer sink leaves OutputPath empty just like a
	// discarded measurement, so the renderer uses this to print "(streamed)" rather
	// than "none (measurement only)".
	audioStream bool
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
	if nonFinite(v) {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

// nonFinite reports whether v is NaN or infinite. humanLUFS renders such loudness
// values as "n/a" and the JSON encoders as null; a clip shorter than the LUFS gate
// produces one.
func nonFinite(v float64) bool {
	return math.IsNaN(v) || math.IsInf(v, 0)
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
	if nonFinite(v) {
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

// alreadyRenderedError marks a failure that a command has already written.
// main uses the wrapped cause for the exit code without rendering it again.
type alreadyRenderedError struct{ cause error }

func (e *alreadyRenderedError) Error() string { return e.cause.Error() }
func (e *alreadyRenderedError) Unwrap() error { return e.cause }

// alreadyRendered wraps cause so main does not render it again.
func alreadyRendered(cause error) error {
	if cause == nil {
		return nil
	}
	return &alreadyRenderedError{cause: cause}
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
// Both forms use the same classification.
func renderError(w io.Writer, jsonMode bool, err error) {
	if err == nil {
		return
	}
	c := classifyError(err)
	if jsonMode {
		var je jsonError
		je.SchemaVersion = schemaVersion
		je.Error.Code = c.code
		je.Error.Message = c.message
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(je)
		return
	}
	fmt.Fprintf(w, "waxtap: %s\n", c.message)
	if c.hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", c.hint)
	}
}

// cleanMessage strips a redundant leading "waxtap: " before the CLI adds its own
// prefix.
func cleanMessage(msg string) string {
	return strings.TrimPrefix(msg, "waxtap: ")
}

// classifiedError contains every user-visible representation of a terminal error.
type classifiedError struct {
	exitCode int
	code     string
	message  string
	hint     string
}

// classifyError maps a terminal error to its exit code, machine code, message,
// and optional hint.
//
// Domain sentinels take precedence over wrapped transport and filesystem errors.
// Structural checks therefore run after all sentinel checks.
func classifyError(err error) classifiedError {
	if err == nil {
		return classifiedError{}
	}
	c := classifiedError{message: cleanMessage(friendlyError(err)), exitCode: 1, code: "error"}
	// Classify invalid sidecar responses by status, including responses wrapped in
	// ErrNeedsPOToken.
	sre, hasSidecarResp := errors.AsType[*waxtap.SidecarResponseError](err)
	switch {
	case errors.Is(err, context.Canceled):
		c.exitCode, c.code = 130, "canceled"

	// Domain sentinels keep their classification even when they wrap another cause.
	case errors.Is(err, waxtap.ErrNeedsPOToken):
		switch {
		case hasSidecarResp:
			// The sidecar responded, so classify the failure by its HTTP status or
			// response content.
			c.exitCode, c.code = sidecarResponseExit(sre.StatusCode)
			c.hint = sidecarAuthHint(sre.StatusCode)
		case isSidecarConnection(err):
			c.exitCode, c.code, c.hint = 9, "network", "start the PO-token sidecar or correct --potoken-url"
		default:
			c.exitCode, c.code, c.hint = 8, "needs-po-token", poTokenHint
		}
	case errors.Is(err, waxtap.ErrFFmpegNotFound):
		c.exitCode, c.code, c.hint = 6, "ffmpeg-not-found", "install ffmpeg (it bundles ffprobe) to use download/cut/transcode/normalize processing"
	case errors.Is(err, waxtap.ErrRateLimited):
		c.exitCode, c.code = 5, "rate-limited"
	case errors.Is(err, waxtap.ErrIncompleteStream):
		c.exitCode, c.code, c.hint = 7, "incomplete-stream", incompleteStreamHint
	case errors.Is(err, waxtap.ErrURLExpired):
		// An expired, unrefreshable URL is an incomplete delivery; same exit class.
		c.exitCode, c.code = 7, "url-expired"
	case errors.Is(err, waxtap.ErrVideoUnavailable):
		c.exitCode, c.code, c.hint = 3, "video-unavailable", embedHint(err)
	case errors.Is(err, waxtap.ErrVideoRestricted):
		c.exitCode, c.code = 3, "video-restricted"
	case errors.Is(err, waxtap.ErrLoginRequired):
		c.exitCode, c.code = 3, "login-required"
	case errors.Is(err, waxtap.ErrLiveContent):
		c.exitCode, c.code = 3, "live-content"
	case errors.Is(err, waxtap.ErrLiveNotStarted):
		c.exitCode, c.code = 3, "live-not-started"
	case errors.Is(err, waxtap.ErrAgeRestricted):
		c.exitCode, c.code = 3, "age-restricted"
	case errors.Is(err, waxtap.ErrMembersOnly):
		c.exitCode, c.code = 3, "members-only"
	case errors.Is(err, waxtap.ErrGeoBlocked):
		c.exitCode, c.code = 3, "geo-blocked"
	case errors.Is(err, waxtap.ErrNoAudioFormats):
		c.exitCode, c.code = 3, "no-audio-formats"
	case errors.Is(err, waxtap.ErrPlaylistUnavailable):
		c.exitCode, c.code = 3, "playlist-unavailable"
	case errors.Is(err, waxtap.ErrPlaylistEmpty):
		c.exitCode, c.code = 3, "playlist-empty"
	case errors.Is(err, waxtap.ErrRequestedFormatUnavailable):
		c.exitCode, c.code, c.hint = 2, "format-unavailable", "run `waxtap formats <url>` to list the available itags and codecs"
	case errors.Is(err, waxtap.ErrPlaylistParse):
		c.exitCode, c.code = 4, "stale-parser"
	case errors.Is(err, waxtap.ErrCipherSolve):
		c.exitCode, c.code, c.hint = 4, "cipher-solve", cipherSolveHint
	case errors.Is(err, waxtap.ErrExtractionFailed),
		// An over-cap response body (iox truncation guard) is an anomalous extraction
		// failure, classified the same as any other so player/innertube/SABR agree.
		errors.Is(err, iox.ErrResponseTooLarge):
		c.exitCode, c.code = 4, "extraction-failed"
	case errors.Is(err, waxtap.ErrIncompatibleSpec):
		c.exitCode, c.code = 2, "incompatible-spec"
	case errors.Is(err, waxtap.ErrUnsupportedInput):
		c.exitCode, c.code = 2, "unsupported-input"
	case errors.Is(err, waxtap.ErrIsPlaylist):
		c.exitCode, c.code, c.hint = 2, "is-playlist", "the download command expands playlist URLs automatically; info/formats take a single video"
	case errors.Is(err, waxtap.ErrIsChannel):
		c.exitCode, c.code, c.hint = 2, "is-channel", "open a specific video, or run `waxtap download <channel> --list` to list its uploads"
	case errors.Is(err, waxtap.ErrInvalidVideoID),
		errors.Is(err, waxtap.ErrVideoIDTooShort),
		errors.Is(err, waxtap.ErrVideoIDTooLong),
		errors.Is(err, waxtap.ErrInvalidPlaylistID):
		c.exitCode, c.code = 2, "invalid-input"
	case errors.Is(err, waxtap.ErrInvalidConfig):
		c.exitCode, c.code = 2, "invalid-config"
	case isUsageError(err):
		c.exitCode, c.code, c.hint = 2, "usage", flagOrderHint(err)

	// Deadlines during dialing and reading are both network timeouts.
	case errors.Is(err, context.DeadlineExceeded):
		c.exitCode, c.code = 9, "timeout"

	// Structural fallbacks apply only when no domain sentinel or timeout matched.
	case isProxyError(err):
		c.exitCode, c.code, c.hint = 9, "network", "check the proxy is reachable and that --proxy is a correct URL"
	// Classify a sidecar response before checking for provider connection errors.
	case hasSidecarResp:
		c.exitCode, c.code = sidecarResponseExit(sre.StatusCode)
		c.hint = sidecarAuthHint(sre.StatusCode)
	case isProviderError(err):
		c.exitCode, c.code, c.hint = 9, "network", "start the provider sidecar or correct its URL (--player-context-url/--session-url)"
	case isConnectionError(err):
		c.exitCode, c.code, c.hint = 9, "network", "check network connectivity and any configured provider URLs"
	// Only output failures receive output-directory guidance.
	case isOutputError(err):
		c.exitCode, c.code, c.hint = 10, "io", "check the output directory exists and is writable"
	case isLocalIOError(err):
		c.exitCode, c.code = 10, "io"
	}
	return c
}

const (
	// poTokenHint covers a missing provider, a failed mint, or a token YouTube
	// rejected. A status-2 cap is classified as ErrIncompleteStream instead.
	poTokenHint          = "configure --potoken-url, or if one is set the provider's mint failed or YouTube rejected the token (attestation status 3); run `waxtap doctor` or see MAINTENANCE.md"
	incompleteStreamHint = "another client may deliver the full stream (omit --no-fallback); for forced WEB audio supply both --player-context-url and --session-url (both also require --potoken-url), then retry if WEB hit a transient status-2 cap"
	cipherSolveHint      = "full WEB audio needs an attested identity; supply both --player-context-url and --session-url (both also require --potoken-url)"
)

// emitWatchPageBreadcrumb notes on stderr that forced WEB metadata was served
// from the watch page, which does not need a PO token. The note is limited to
// forced WEB so the default client chain does not print a misleading token hint
// after falling back to the watch page. The info and formats commands call it
// before their output branch so human and JSON modes behave the same.
func emitWatchPageBreadcrumb(env *appEnv, info *waxtap.InfoResult) {
	if strings.EqualFold(env.cfg.client, "web") && info.ViaWatchPage {
		env.info("note: WEB metadata via the watch-page fallback (no PO token)\n")
	}
}

// noteDroppedPlaylist reports a list= parameter that the current command will not
// process. hint gives the command-specific next step for handling the whole
// playlist. The note stays on stderr, keeping JSON and -o - stdout parseable.
func noteDroppedPlaylist(env *appEnv, input, hint string) {
	if id, err := youtube.ExtractPlaylistID(input); err == nil {
		env.info("note: ignoring playlist %s; %s\n", id, hint)
	}
}

// noteUseBothWebSources prints one pre-flight note when a stream command is likely
// to use WEB token extraction without both identity sources. The paired setup lets
// WEB try player-context and adopted-session paths before a transient status-2 cap
// becomes user-visible. Keep this on commands that resolve a stream; local
// processing and SponsorBlock preview should stay quiet.
func noteUseBothWebSources(env *appEnv) {
	if msg, ok := webSourcesNote(env.cfg); ok {
		env.info("%s\n", msg)
	}
}

// webSourcesNote returns the "supply both WEB sources" note and whether the
// config is on a single-source WEB path the note applies to. It is the shared
// gate behind the pre-flight note (info/formats) and the outcome-aware download
// note. A deliberately forced non-WEB client is not attempting WEB extraction, so
// a "use both / set --client web" note would contradict that choice; only the WEB
// client or the default chain reach WEB.
func webSourcesNote(c *appConfig) (string, bool) {
	if c.client != "" && !strings.EqualFold(c.client, "web") {
		return "", false
	}
	onWebPath := c.potokenURL != "" || c.playerContextURL != "" || c.sessionURL != "" || c.visitorData != ""
	bothSources := c.playerContextURL != "" && c.sessionURL != ""
	if !onWebPath || bothSources {
		return "", false
	}
	msg := "note: for WEB extraction, supply both --player-context-url and --session-url (both also require --potoken-url)"
	if !strings.EqualFold(c.client, "web") {
		msg += ", and set --client web"
	}
	return msg, true
}

// noteForcedIOSIncomplete suggests the default client chain after a forced iOS
// client returns an incomplete stream. It is called only for commands that report
// a single error, avoiding a repeated note for playlist failures.
func noteForcedIOSIncomplete(env *appEnv, err error) {
	if errors.Is(err, waxtap.ErrIncompleteStream) && strings.EqualFold(env.cfg.client, "ios") {
		env.info("note: iOS media delivery is unreliable in current testing, even on short clips; omit --client for reliable audio\n")
	}
}

// friendlyError returns a human message for an error, expanding common sentinels.
func friendlyError(err error) string {
	// Provider connection errors may be wrapped by ErrNeedsPOToken. Check them
	// first so the endpoint failure remains visible.
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if isProxyError(err) {
		return "proxy connection failed (check --proxy)"
	}
	// The sidecar error types self-redact their endpoint, so their Error() is safe
	// to surface directly; the 429 rate-limit advisory rides on sidecarAuthHint.
	if se, ok := errors.AsType[*waxtap.SidecarError](err); ok {
		return se.Error()
	}
	if sre, ok := errors.AsType[*waxtap.SidecarResponseError](err); ok {
		return sre.Error()
	}
	// A typed playlist-unavailable error carries YouTube's own reason (no URL), so
	// surface it. The bare wrapped sentinel from a browse 403/404 embeds the
	// internal endpoint URL and is handled by the switch with a fixed message.
	if pue, ok := errors.AsType[*waxtap.PlaylistUnavailableError](err); ok && pue.Reason != "" {
		return pue.Error()
	}
	switch {
	case errors.Is(err, waxtap.ErrInvalidConfig):
		// Library errors name Go option fields. Present the corresponding CLI flags
		// while preserving the wrapped error and its exit-code classification.
		return translateConfigSymbols(err.Error())
	case errors.Is(err, waxtap.ErrFFmpegNotFound):
		return "ffmpeg/ffprobe not found on PATH"
	case errors.Is(err, waxtap.ErrIsPlaylist):
		return "that is a playlist URL, not a single video"
	case errors.Is(err, waxtap.ErrIsChannel):
		return "that is a channel, not a single video; open a specific video"
	case errors.Is(err, waxtap.ErrInvalidPlaylistID):
		return "invalid or missing playlist ID"
	case errors.Is(err, waxtap.ErrPlaylistUnavailable):
		// A fixed message keeps the wrapped browse URL out of the output.
		return "this playlist is unavailable; it may be private, deleted, or nonexistent"
	case errors.Is(err, waxtap.ErrPlaylistEmpty):
		return "this playlist has no videos"
	case errors.Is(err, waxtap.ErrShortsPlaylist):
		return "Shorts shelf playlists aren't supported because YouTube doesn't expose them as a complete list; use the channel's uploads playlist instead (replace the leading UUSH with UU)"
	case errors.Is(err, waxtap.ErrNeedsPOToken):
		return "YouTube requires a verified PO token for this stream (none configured, or the provided token was not accepted)"
	case errors.Is(err, waxtap.ErrRateLimited):
		return "rate limited by YouTube; back off and retry later"
	case errors.Is(err, waxtap.ErrIncompleteStream):
		return "the download ended before the full stream was received"
	case errors.Is(err, waxtap.ErrURLExpired):
		return "the stream URL expired and could not be refreshed"
	case errors.Is(err, waxtap.ErrPlaylistParse):
		return "YouTube returned a playlist shape WaxTap doesn't recognize; the parser may need updating"
	}
	// Any remaining HTTP status error, such as an innertube 500/503 or a
	// SponsorBlock fetch failure, would otherwise leak its endpoint URL through
	// err.Error(). Render it without the URL and attribute it to the right service.
	if hse, ok := errors.AsType[*waxtap.HTTPStatusError](err); ok {
		return fmt.Sprintf("%s returned HTTP %d", httpStatusSource(hse.URL), hse.StatusCode)
	}
	return err.Error()
}

// httpStatusSource names the service behind an HTTPStatusError from its URL host,
// so an unclassified status is attributed correctly (YouTube vs SponsorBlock vs
// googlevideo) without leaking the full URL. Unknown hosts get a neutral label.
func httpStatusSource(rawURL string) string {
	if rawURL == "" {
		return "the server"
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "the server"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.Contains(host, "sponsor"):
		return "SponsorBlock"
	case strings.Contains(host, "googlevideo"):
		return "googlevideo"
	case strings.Contains(host, "youtube"):
		return "YouTube"
	default:
		return "the server"
	}
}

// configSymbolReplacer maps Go option field names in ErrInvalidConfig templates
// to the corresponding CLI flags. Each key includes fixed template text so a
// user-supplied path or value that contains a field name remains unchanged.
// config_symbols_test.go checks that the keys still match reachable templates.
var configSymbolReplacer = strings.NewReplacer(
	"invalid ChromeMajor", "invalid --chrome-major",
	"ChromeMajor and ProfileOverridePath", "--chrome-major and --profile-override",
	"Client and ProfileOverridePath", "--client and --profile-override",
	"invalid Cooldown", "invalid --cooldown",
	"invalid PerHostQPS", "invalid --qps",
	"set Options.Client", "set --client",
	"single-client ProfileOverridePath", "single-client --profile-override",
	// config.go currently catches these conflicts before waxtap.New does.
	"PlayerContextProvider requires a POTokenProvider", "--player-context-url requires --potoken-url",
	"non-empty VisitorData", "non-empty --visitor-data",
	"invalid SponsorBlock BaseURL", "invalid --sponsorblock-url",
)

// translateConfigSymbols rewrites Go option field names in a config error message
// to their CLI flag equivalents.
func translateConfigSymbols(msg string) string {
	return configSymbolReplacer.Replace(msg)
}

// isProxyError reports whether err is a failure to connect to the configured
// proxy. It prefers typed unwrapping and falls back to a string match for
// transports that do not expose a typed proxyconnect error.
func isProxyError(err error) bool {
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "proxyconnect" {
		return true
	}
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.Err != nil && strings.Contains(ue.Err.Error(), "proxyconnect") {
		return true
	}
	return strings.Contains(err.Error(), "proxyconnect")
}

// isProviderError reports whether err came from a player-context or session
// provider. PO-token provider failures use ErrNeedsPOToken instead.
func isProviderError(err error) bool {
	_, ok := errors.AsType[*waxtap.ProviderError](err)
	return ok
}

// isSidecarConnection reports whether err contains a sidecar connection failure.
func isSidecarConnection(err error) bool {
	_, ok := errors.AsType[*waxtap.SidecarError](err)
	return ok
}

// sidecarAuthHint returns guidance for a sidecar response status: authentication
// help for 401/403 and a rate-limit advisory for 429. It rides the c.hint channel
// so package main needs no redact helper for the 429 message.
func sidecarAuthHint(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "the sidecar requires authentication; set or verify --api-key"
	case http.StatusTooManyRequests:
		return "check the sidecar's rate limits"
	}
	return ""
}

// sidecarResponseExit maps a sidecar response to its CLI exit code and machine
// code. Client errors indicate invalid configuration, 429 indicates rate
// limiting, and timeouts, server errors, and invalid HTTP 200 responses indicate
// network or provider failures.
func sidecarResponseExit(status int) (int, string) {
	switch {
	case status == http.StatusTooManyRequests:
		return 5, "rate-limited"
	case status >= 400 && status < 500 && status != http.StatusRequestTimeout:
		return 2, "invalid-config"
	default:
		return 9, "network"
	}
}

// isConnectionError reports whether err is a dial or DNS failure.
func isConnectionError(err error) bool {
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	_, ok := errors.AsType[*net.DNSError](err)
	return ok
}

// isLocalIOError reports whether err is a local filesystem failure.
func isLocalIOError(err error) bool {
	_, ok := errors.AsType[*fs.PathError](err)
	return ok
}

// isOutputError reports whether err occurred while staging or publishing output.
func isOutputError(err error) bool {
	_, ok := errors.AsType[*tempfile.OutputError](err)
	return ok
}

// isUsageError reports whether err marks a bad-arguments failure.
func isUsageError(err error) bool {
	_, ok := errors.AsType[*usageError](err)
	return ok
}

// embedHint returns fallback guidance for a web_embedded playability error.
func embedHint(err error) string {
	if pe, ok := errors.AsType[*waxtap.PlayabilityError](err); ok && pe.Embed {
		return "web_embedded currently falls back to web; use --client web or --client android_vr"
	}
	return ""
}

// errorCode returns the stable machine code for the --json error envelope.
func errorCode(err error) string { return classifyError(err).code }

// errorHint returns an optional next-step hint for an error.
func errorHint(err error) string { return classifyError(err).hint }

// exitCodeFor maps an error to a process exit code so scripts can branch on the
// failure class without parsing messages.
func exitCodeFor(err error) int { return classifyError(err).exitCode }

// normalizeExecuteError converts Cobra's untyped unknown-command errors into
// usage errors.
func normalizeExecuteError(err error) error {
	if err == nil {
		return nil
	}
	if isUsageError(err) {
		return err
	}
	// Preserve the marker even if its cause resembles an unknown-command error.
	if _, ok := errors.AsType[*alreadyRenderedError](err); ok {
		return err
	}
	if msg := err.Error(); strings.HasPrefix(msg, "unknown command") || strings.HasPrefix(msg, "unknown subcommand") {
		return &usageError{msg: msg}
	}
	return err
}

// flagOrderHint adds CLI help for YouTube-looking arguments that Cobra or pflag
// parsed before the command could receive them.
func flagOrderHint(err error) string {
	ue, ok := errors.AsType[*usageError](err)
	if !ok {
		return ""
	}
	tok, isUnknownCmd := unknownCommandToken(ue.msg)
	if isUnknownCmd && looksLikeYouTubeTarget(tok) {
		return fmt.Sprintf("did you mean `waxtap download %s`? global flags go before the subcommand, command flags after it", tok)
	}
	// A video ID that starts with "-" reaches pflag as shorthand flags. The
	// original token still has the dash, so looksLikeYouTubeTarget can match it.
	if dtok, ok := dashFlagToken(ue.msg); ok && looksLikeYouTubeTarget(dtok) {
		return fmt.Sprintf("a leading-dash video ID is parsed as flags; pass it after -- (e.g. `-- %s`) or use the full https://youtu.be/%s URL", dtok, dtok)
	}
	// A non-target token can be reported as the command, for example
	// `--no-cache --cache-dir /p info <id>` as `unknown command "/p"`. cobra can
	// consume a later subcommand while traversing flags, so when a flag precedes a
	// real subcommand, explain the ordering. Keep the hint generic: a flag value can
	// coincidentally equal a subcommand name.
	if isUnknownCmd && flagBeforeSubcommand(os.Args[1:], rootSubcommandNames) {
		return "a flag before the subcommand can be parsed as part of command lookup; put global flags (--json/--quiet/--verbose) before the subcommand and any command flags after it"
	}
	return ""
}

// rootSubcommandNames is the set of subcommand names and aliases (including
// cobra's built-in help and completion) used to detect a flag placed before the
// subcommand. TestRootSubcommandNamesMatchTree keeps it aligned with the live
// command tree.
var rootSubcommandNames = map[string]bool{
	"info": true, "formats": true, "download": true, "cut": true,
	"transcode": true, "normalize": true, "sponsorblock": true, "sb": true,
	"cache": true, "doctor": true, "version": true, "exit-codes": true,
	"help": true, "completion": true,
}

// flagBeforeSubcommand reports whether a flag token precedes the first recognized
// subcommand in args. cobra traversal can consume a later subcommand after an
// unknown bare boolean flag, turning `waxtap --no-cache info <id>` into
// `unknown command "<id>"`; this detects that shape. A bare "-" (stdin) and "--"
// (terminator) are not flags. A subcommand with no preceding flag yields false so a
// genuine command typo is not given an ordering hint. A flag value can coincide with
// a subcommand name, so callers should use only generic guidance.
func flagBeforeSubcommand(args []string, names map[string]bool) bool {
	firstFlag := -1
	for i, a := range args {
		if firstFlag < 0 && len(a) > 1 && a[0] == '-' && a != "--" {
			firstFlag = i
		}
		if names[a] {
			return firstFlag >= 0 && firstFlag < i
		}
	}
	return false
}

// unknownCommandToken extracts the quoted token from a cobra "unknown command"
// message.
func unknownCommandToken(msg string) (string, bool) {
	if !strings.HasPrefix(msg, "unknown command") {
		return "", false
	}
	_, after, ok := strings.Cut(msg, `"`)
	if !ok {
		return "", false
	}
	tok, _, ok := strings.Cut(after, `"`)
	if !ok {
		return "", false
	}
	return tok, true
}

// dashFlagToken extracts the original argument from pflag's unknown-shorthand
// message, for example "unknown shorthand flag: 'a' in -abcdefghij". Long flags
// are left alone because real command flags can also look like video IDs.
func dashFlagToken(msg string) (string, bool) {
	if !strings.HasPrefix(msg, "unknown shorthand flag:") {
		return "", false
	}
	_, after, ok := strings.Cut(msg, " in ")
	if !ok {
		return "", false
	}
	return after, true
}

// looksLikeYouTubeTarget reports whether s resembles a YouTube URL or bare video
// ID.
func looksLikeYouTubeTarget(s string) bool {
	if strings.Contains(s, "youtube.com") || strings.Contains(s, "youtu.be") {
		return true
	}
	if len(s) != 11 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
