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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/iox"
	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/youtube"
	"github.com/spf13/cobra"
)

// schemaVersion tags JSON output so callers can handle shape changes. Version 1 is
// the pre-1.0 baseline. Version 2 dropped the redundant outputFormat field from
// non-transcoded local results, and itag from local formats, which do not come
// from a YouTube format.
//
// Version 3 made every failure in every document the same object. A playlist
// item's error was a raw string, a batch item's was a different raw string, and
// only the top-level envelope carried a machine-readable code, so a consumer
// aggregating a run had to grep prose to tell a missing PO token from a bad
// path. All three are now {code, message} through the one classifier. In the
// same pass: error and not-run batch items stopped carrying an output path they
// never wrote, batch summary counts became unconditional (an absent key no
// longer has to be read as zero) and gained a total, and chapterCount is omitted
// rather than asserting 0 when chapters were never fetched.
const schemaVersion = 3

// appEnv carries the per-invocation client, resolved config, IO writers, and
// logger. Commands obtain one with setup at the top of their RunE.
type appEnv struct {
	client *waxtap.Client
	cfg    *appConfig
	out    io.Writer // stdout: command results (human or JSON)
	errOut io.Writer // stderr: progress, logs, errors
	// notes collects this document's note: diagnostics so --json carries them.
	// Nil means nothing collects, which is what the pre-setup paths get.
	notes *noteCollector
	log   *slog.Logger
	// sidecars are the providers this run's sidecar URLs built, so doctor probes
	// the same objects the download uses. Zero value means none are configured.
	sidecars sidecarProviders
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
	// The very providers options wired into the client: sidecarProviders builds
	// one set per run, so a probe exercises what a download would use.
	sidecars, err := cfg.sidecarProviders()
	if err != nil {
		return nil, err
	}
	env := &appEnv{
		client:   client,
		cfg:      cfg,
		out:      cmd.OutOrStdout(),
		errOut:   cmd.ErrOrStderr(),
		log:      log,
		notes:    &noteCollector{},
		sidecars: sidecars,
	}
	// An error envelope is rendered from main, which has no appEnv, so the run's
	// collector is published for it: a failure has to honor the document contract
	// after the command that could describe it is gone. Notes
	// from a failed run matter most of all, since some of them (the file kept but
	// not recorded in the archive) only ever fire on a failure path.
	setRunNotes(env.notes)
	return env, nil
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

// noteCode is the stable machine-readable identifier of a CLI note. Notes are
// the class of diagnostic that says what WaxTap did on the user's behalf when
// the request did not fully determine it: a container left alone, a concurrency
// clamped, a file the archive did not record.
//
// They carry codes rather than prose because a note that lives only in a
// stderr sentence is not a contract. A consumer matching container-ext-mismatch
// keeps working when the sentence is reworded; one grepping the sentence does
// not. The codes are CLI-side and deliberately separate from the library's
// WarningCode: these describe decisions the command line made, not conditions
// the library observed.
type noteCode string

const (
	noteALACContainer        noteCode = "alac-mp4-container"
	noteArchiveNotRecorded   noteCode = "archive-not-recorded"
	noteChannelsIgnored      noteCode = "channels-ignored"
	noteChannelsUnavailable  noteCode = "channels-unavailable"
	noteConcurrencyClamped   noteCode = "concurrency-clamped"
	noteContainerExtMismatch noteCode = "container-ext-mismatch"
	noteCueFileMismatch      noteCode = "cue-file-mismatch"
	noteDoctorCaveat         noteCode = "doctor-caveat"
	noteFlagInert            noteCode = "flag-inert"
	noteForcedClientRisky    noteCode = "forced-client-risky"
	noteKeptOutput           noteCode = "kept-output"
	notePlaylistIgnored      noteCode = "playlist-ignored"
	noteProbeSkipped         noteCode = "probe-skipped"
	noteEnumerationError     noteCode = "enumeration-error"
	noteSameFormatCopied     noteCode = "same-format-copied"
	noteSidecarWriteFailed   noteCode = "sidecar-write-failed"
	noteSelectionUnmatched   noteCode = "selection-unmatched"
	noteUnalteredCopy        noteCode = "unaltered-copy"
	noteWatchPageFormats     noteCode = "watch-page-formats"
	noteWatchPageMetadata    noteCode = "watch-page-metadata"
	noteWebSources           noteCode = "web-sources"
)

// allNoteCodes is every note the CLI can record. A new code is added here as
// well as above; TestNoteCodesDocumented reads this list to check that README
// documents the vocabulary a --json consumer matches on.
var allNoteCodes = []noteCode{
	noteALACContainer,
	noteArchiveNotRecorded,
	noteChannelsIgnored,
	noteChannelsUnavailable,
	noteConcurrencyClamped,
	noteContainerExtMismatch,
	noteCueFileMismatch,
	noteDoctorCaveat,
	noteFlagInert,
	noteForcedClientRisky,
	noteKeptOutput,
	notePlaylistIgnored,
	noteProbeSkipped,
	noteEnumerationError,
	noteSameFormatCopied,
	noteSidecarWriteFailed,
	noteSelectionUnmatched,
	noteUnalteredCopy,
	noteWatchPageFormats,
	noteWatchPageMetadata,
	noteWebSources,
}

// noteJSON is one note in a JSON document, the same {code, detail} shape
// warnings use.
type noteJSON struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// noteCollector accumulates the notes one document will carry. It is shared by
// value through appEnv copies, so a scope created with withScopedNotes collects
// independently of its parent.
//
// The mutex is not decoration: playlist items run concurrently and out of order,
// and BuildRequest for item N+1 overlaps item N, so notes from two items can be
// added at the same moment.
type noteCollector struct {
	mu    sync.Mutex
	notes []noteJSON
}

func (c *noteCollector) add(n noteJSON) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notes = append(c.notes, n)
}

// drain returns the collected notes and empties the collector, so a scope reused
// across items cannot leak one item's notes onto the next.
func (c *noteCollector) drain() []noteJSON {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.notes
	c.notes = nil
	return out
}

// snapshot returns a copy of the collected notes without draining them, safe
// against writers still appending.
func (c *noteCollector) snapshot() []noteJSON {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.notes)
}

// note records a diagnostic and prints it to stderr as a "note: " line unless
// --quiet is set.
//
// The recording is not gated by --quiet, which is the point: --json --quiet used
// to discard notes entirely, so the mode most likely to be read by a program was
// the one that lost the machine-readable half.
func (e *appEnv) note(code noteCode, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if e.notes != nil {
		e.notes.add(noteJSON{Code: string(code), Detail: detail})
	}
	if e.quiet() {
		return
	}
	fmt.Fprintf(e.errOut, "note: %s\n", detail)
}

// notesJSON returns the notes collected so far, without draining them.
func (e *appEnv) notesJSON() []noteJSON {
	if e.notes == nil {
		return nil
	}
	return e.notes.snapshot()
}

// withScopedNotes returns a copy of e collecting into a fresh scope, for work
// whose notes belong to one item rather than to the run. Playlist items need it:
// their handlers are concurrent and out of order, so a single shared collector
// would attribute one item's notes to whichever record happened to be written
// next.
func (e *appEnv) withScopedNotes() *appEnv {
	c := *e
	c.notes = &noteCollector{}
	return &c
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

// durationOrDash renders a duration the source reported, or a dash when it
// reported none: a live item or a finished stream can answer no length, and
// "0:00" would present that as a zero-length video. It is for lengths only; a
// chapter or cut position of 0:00 is a real time and keeps humanDuration.
func durationOrDash(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return humanDuration(d)
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
//
// flagsUnparsed marks the subset raised before the persistent flags were read: a
// bad flag aborts the parse at that token, and an unknown command never reaches
// parsing at all. Only those may re-read the command line for --json; after a
// successful parse the parsed flag is the answer, and a second look would misread
// `--format --json`, where --json is a flag's value rather than a request.
//
// cause is optional and never rendered: it lets a usage error carry the sentinel
// that identifies the condition, such as tempfile.ErrRenumberExhausted from the
// auto-number pre-flight, without giving up the exit code the user's own
// mistake earns.
type usageError struct {
	msg           string
	flagsUnparsed bool
	cause         error
}

func (e *usageError) Error() string { return e.msg }
func (e *usageError) Unwrap() error { return e.cause }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// unparsedFlagsError builds the usage error for a failure that preceded flag
// parsing. Only the two sites that can precede it use this.
func unparsedFlagsError(msg string) error {
	return &usageError{msg: msg, flagsUnparsed: true}
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

// finalError makes a failure that followed a signal report as a cancellation.
// Any error after the signal fired is treated as one, including an unrelated
// failure from a moment earlier; the signal is the reason the process is exiting.
// An error that already reports the cancellation is left alone.
//
// It joins rather than wrapping (unlike the library's cancelCause, which wraps to
// mask ErrIncompleteStream from a caller's retry logic) so an
// *alreadyRenderedError stays visible to errors.AsType; a single %w would hide it
// and main would write a second error document next to the command's own. Nothing
// here retries, and classifyError checks cancellation first, so the joined
// sentinel cannot change the exit code. It joins ctx.Err() rather than a literal
// context.Canceled: identical today, honest if a root deadline is ever wired in.
func finalError(ctx context.Context, err error) error {
	ce := ctx.Err()
	if err == nil || ce == nil || errors.Is(err, ce) {
		return err
	}
	return errors.Join(ce, err)
}

// errorJSON is how every WaxTap JSON document reports a failure: the classifier's
// stable kebab code plus its human message. One shape everywhere, so a consumer
// aggregating a run reads error.code in a playlist item, a batch item and the
// top-level envelope alike instead of matching message prose in two of the three.
type errorJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeRecord writes one compact NDJSON record followed by a newline. A record
// the encoder refuses is replaced by an error envelope rather than dropped: an
// NDJSON stream promises one record per item, and a consumer counting a shorter
// stream has no way to learn that something went missing.
func writeRecord(w io.Writer, rec any) {
	b, err := json.Marshal(rec)
	if err != nil {
		b, err = json.Marshal(struct {
			SchemaVersion int       `json:"schemaVersion"`
			Type          string    `json:"type"`
			Error         errorJSON `json:"error"`
		}{schemaVersion, "error", errorJSON{Code: "error", Message: "encode record: " + err.Error()}})
		if err != nil {
			// Unreachable: the envelope is three strings and an int. A bare
			// line still keeps the record count honest.
			b = []byte(`{"schemaVersion":` + strconv.Itoa(schemaVersion) + `,"type":"error","error":{"code":"error","message":"encode record"}}`)
		}
	}
	fmt.Fprintf(w, "%s\n", b)
}

// errorObject renders err as an errorJSON, or nil for no error, so the item
// renderers cannot drift from the envelope's classification.
func errorObject(err error) *errorJSON {
	if err == nil {
		return nil
	}
	c := classifyError(err)
	return &errorJSON{Code: c.code, Message: c.message}
}

// jsonError is the --json error envelope. outputPath and outputBytes are set only
// when a failed run nevertheless left a complete file behind, so their absence is
// the norm.
type jsonError struct {
	SchemaVersion int        `json:"schemaVersion"`
	Error         *errorJSON `json:"error"`
	OutputPath    string     `json:"outputPath,omitempty"`
	OutputBytes   int64      `json:"outputBytes,omitempty"`
	Notes         []noteJSON `json:"notes,omitempty"`
}

// keptOutput names a complete file a failed run left at its output path.
type keptOutput struct {
	path  string
	bytes int64
}

// renderError writes the final command error as JSON or as a human-readable line.
// Both forms use the same classification. args is the command line the failure
// came from, minus the program name; only the flag-ordering hint reads it.
func renderError(w io.Writer, jsonMode bool, err error, args []string) {
	renderErrorKept(w, jsonMode, err, args, nil)
}

// renderErrorKept is renderError plus the file a failed run left behind. Every
// writer to an output path in WaxTap stages and commits atomically, so a file
// sitting there after a cancellation is complete; the gap this closes is that
// nothing used to say so. A nil kept produces byte-identical output to
// renderError.
//
// A cancellation reported this way is still exit 130 and still a failure. Naming
// the file does not promote it to a success, and the caller says so in the note
// when post-processing (the info sidecar, the archive entry) did not run.
func renderErrorKept(w io.Writer, jsonMode bool, err error, args []string, kept *keptOutput) {
	if err == nil {
		return
	}
	c := classifyArgs(err, args)
	if jsonMode {
		je := jsonError{
			SchemaVersion: schemaVersion,
			Error:         &errorJSON{Code: c.code, Message: c.message},
			Notes:         currentRunNotes(),
		}
		if kept != nil {
			// Same key resultToJSON emits, so it gets the same normalization.
			je.OutputPath, je.OutputBytes = displayPath(kept.path), kept.bytes
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(je)
		return
	}
	fmt.Fprintf(w, "waxtap: %s\n", c.message)
	if c.hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", c.hint)
	}
	if kept != nil {
		fmt.Fprintf(w, "  note: the finished file was kept: %s (%d bytes)\n", displayPath(kept.path), kept.bytes)
	}
}

// libraryPrefixRe matches the name a library entry point puts on its own
// errors ("waxtap.ProcessAlbum: ..."): the package, a dot, an exported name. A
// file named waxtap.something does not match, since its extension is lowercase.
var libraryPrefixRe = regexp.MustCompile(`^waxtap\.[A-Z][A-Za-z]*: `)

// cleanMessage strips the library's own prefixes before the CLI adds its own:
// the leading "waxtap: " a sentinel carries, the "waxtap.Func: " a library
// entry point names itself by, and a sentinel's "waxtap: " nested behind
// context the library put in front of it ("track x: waxtap: unsupported..."),
// which would otherwise print as "waxtap: track x: waxtap: unsupported...".
func cleanMessage(msg string) string {
	msg = strings.TrimPrefix(msg, "waxtap: ")
	msg = libraryPrefixRe.ReplaceAllString(msg, "")
	return strings.ReplaceAll(msg, ": waxtap: ", ": ")
}

// classifiedError contains every user-visible representation of a terminal error.
type classifiedError struct {
	exitCode int
	code     string
	message  string
	hint     string
}

// classifyError maps a terminal error to its exit code, machine code, message,
// and optional hint, reading this process's command line for the flag-ordering
// hint. Callers that hold the command line pass it to classifyArgs instead.
func classifyError(err error) classifiedError {
	return classifyArgs(err, os.Args[1:])
}

// classifyArgs is classifyError against a given command line. Only the
// flag-ordering hint depends on args, so exit code, machine code, and message are
// the same whatever is passed.
//
// Domain sentinels take precedence over wrapped transport and filesystem errors.
// Structural checks therefore run after all sentinel checks.
func classifyArgs(err error, args []string) classifiedError {
	if err == nil {
		return classifiedError{}
	}
	c := classifiedError{message: cleanMessage(friendlyError(err)), exitCode: 1, code: "error"}
	// Classify invalid sidecar responses by status, including responses wrapped in
	// ErrNeedsPOToken.
	sre, hasSidecarResp := errors.AsType[*waxtap.SidecarResponseError](err)
	hse, hasHTTPStatus := errors.AsType[*waxtap.HTTPStatusError](err)
	switch {
	case errors.Is(err, context.Canceled):
		c.exitCode, c.code = 130, "canceled"

	// Domain sentinels keep their classification even when they wrap another cause.
	// A sidecar refusal that carries a playability verdict falls through to the
	// availability cases below: the video is what failed, not the configuration.
	case errors.Is(err, waxtap.ErrNeedsPOToken) && !(hasSidecarResp && sre.Unwrap() != nil):
		switch {
		case hasSidecarResp:
			// The sidecar responded, so classify the failure by its HTTP status or
			// response content.
			c.exitCode, c.code = sidecarResponseExit(sre)
			c.hint = sidecarHint(sre)
		case isSidecarConnection(err):
			c.exitCode, c.code, c.hint = 9, "network", "start the PO-token sidecar or correct --potoken-url"
		default:
			c.exitCode, c.code, c.hint = 8, "needs-po-token", poTokenHint
		}
	case errors.Is(err, waxtap.ErrRateLimited):
		c.exitCode, c.code = 5, "rate-limited"
	case errors.Is(err, waxtap.ErrTemporarilyUnavailable):
		// Classed with the rate limiting (5), not the availability verdicts (3):
		// the video is very likely fine and the answer is to come back. Today the
		// sentinel only ever reaches documents as an error.code on enumeration
		// errors (--list --json, playlist summaries); enumeration failures never
		// set the process exit, so the 5 names the class it belongs to rather
		// than an exit a run can currently produce. No hint for the same reason:
		// hints render only on a terminal error envelope.
		c.exitCode, c.code = 5, "temporarily-unavailable"
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
	// An exclusive publish that lost the path is a collision, not an I/O failure,
	// so it exits like the pre-flight check that a sequential run would have hit.
	case isCollisionError(err):
		c.exitCode, c.code = 2, "usage"
	case isUsageError(err):
		c.exitCode, c.code, c.hint = 2, "usage", flagOrderHint(err, args)

	// A proxy that hung until the deadline leaves both markers in one chain, and
	// only this order decides between them. The proxy is the actionable half:
	// "timeout" would send the user at their connection rather than the setting.
	case isProxyError(err):
		// A proxy that answered CONNECT is demonstrably reachable, so the generic
		// reachability hint would contradict its own message.
		c.exitCode, c.code, c.hint = 9, "network", proxyHint(err)

	// Deadlines during dialing and reading are both network timeouts.
	case errors.Is(err, context.DeadlineExceeded):
		c.exitCode, c.code = 9, "timeout"

	// Structural fallbacks apply only when no domain sentinel or timeout matched.
	// Classify a sidecar response before checking for provider connection errors.
	case hasSidecarResp:
		c.exitCode, c.code = sidecarResponseExit(sre)
		c.hint = sidecarHint(sre)
	case isProviderError(err):
		c.exitCode, c.code, c.hint = 9, "network", providerHint(err)
	// An upstream service that answers with an error status is the same failure
	// class as one that cannot be reached; only the hint differs.
	case hasHTTPStatus:
		c.exitCode, c.code, c.hint = 9, "network", httpStatusHint(hse.StatusCode)
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

// watchPageSuffix labels a Client line whose delivery came from the watch-page
// scrape. The client name is identical either way, so without this the two
// deliveries are indistinguishable in the output.
func watchPageSuffix(via bool) string {
	if via {
		return " (via watch page)"
	}
	return ""
}

// emitWatchPageBreadcrumb notes on stderr that forced WEB metadata was served
// from the watch page, which does not need a PO token. The note is limited to
// forced WEB so the default client chain does not print a misleading token hint
// after falling back to the watch page. Only formats still calls it: info
// replaced it with the Client line's "(via watch page)" suffix, which carries
// the delivery fact but not the no-token detail, while formats has no Client
// line to carry either.
func emitWatchPageBreadcrumb(env *appEnv, info *waxtap.InfoResult) {
	if strings.EqualFold(env.cfg.client, "web") && info.ViaWatchPage {
		env.note(noteWatchPageMetadata, "WEB metadata via the watch-page fallback (no PO token)")
	}
}

// noteDroppedPlaylist reports a list= parameter that the current command will not
// process. hint gives the command-specific next step for handling the whole
// playlist. The note stays on stderr, keeping JSON and -o - stdout parseable.
func noteDroppedPlaylist(env *appEnv, input, hint string) {
	if id, err := youtube.ExtractPlaylistID(input); err == nil {
		env.note(notePlaylistIgnored, "ignoring playlist %s; %s", id, hint)
	}
}

// noteUseBothWebSources prints one pre-flight note when a stream command is likely
// to use WEB token extraction without both identity sources. The paired setup lets
// WEB try player-context and adopted-session paths before a transient status-2 cap
// becomes user-visible. Keep this on commands that resolve a stream; local
// processing and SponsorBlock preview should stay quiet.
func noteUseBothWebSources(env *appEnv) {
	if msg, ok := webSourcesNote(env.cfg); ok {
		env.note(noteWebSources, "%s", msg)
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
	msg := "for WEB extraction, supply both --player-context-url and --session-url (both also require --potoken-url)"
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
		env.note(noteForcedClientRisky, "iOS media delivery is unreliable in current testing, even on short clips; omit --client for reliable audio")
	}
}

// friendlyError returns a human message for an error, expanding common sentinels.
func friendlyError(err error) string {
	// Provider connection errors may be wrapped by ErrNeedsPOToken. Check them
	// first so the endpoint failure remains visible.
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	// A proxy that answered names its status and remedy, so check it before the
	// generic proxy branch flattens it to "connection failed".
	if pse, ok := errors.AsType[*proxyStatusError](err); ok {
		return pse.Error()
	}
	if isProxyError(err) {
		return "proxy connection failed (check --proxy)"
	}
	// The sidecar error types self-redact their endpoint, so their Error() is safe
	// to surface directly; the 429 rate-limit advisory rides on sidecarHint.
	if se, ok := errors.AsType[*waxtap.SidecarError](err); ok {
		return se.Error()
	}
	if sre, ok := errors.AsType[*waxtap.SidecarResponseError](err); ok {
		return sre.Error()
	}
	// A 429 can come from SponsorBlock or a sidecar as well as YouTube. The typed
	// error names the throttling host and carries no URL, so it is safe to surface.
	// Every rate-limit failure is this type (httpx builds it on every 429 exit
	// path), so there is no bare-sentinel case in the switch below.
	if rle, ok := errors.AsType[*waxtap.RateLimitError](err); ok {
		return rle.Error() + "; back off and retry later"
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
	case errors.Is(err, waxtap.ErrIncompleteStream):
		return incompleteStreamMessage(err)
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
	// Staging and publishing failures otherwise surface as a bare OS error naming
	// a step the user never asked for ("rename out.mp3: Access is denied.").
	if oe, ok := errors.AsType[*tempfile.OutputError](err); ok {
		return outputFailureMessage(oe)
	}
	return err.Error()
}

// outputFailureMessage renders a staging or publish failure. Everything else in
// this file would otherwise surface these as a bare OS sentence naming a step
// the user never asked for.
//
// An occupied destination reached at publish time is its own case. A CLI run
// stats the destination first, so the file most likely appeared while the run
// was working, but a library caller reaches the same publish with no pre-flight
// at all: the wording offers that reading rather than asserting a cause this
// code cannot know. Only --collision fail reaches here now, since auto-number
// renumbers at publish rather than failing, so auto-number is a real remedy to
// offer. The exit code and machine code stay identical to the pre-flight
// collision, which is what scripts read.
//
// The concurrency note rides only on the publish steps. Appending it to a
// create, chmod, sync, or close failure would point away from the real cause;
// "no space left on device" is a full disk, not a competing writer.
func outputFailureMessage(oe *tempfile.OutputError) string {
	path, reason := "the output path", error(oe)
	if pe, ok := errors.AsType[*os.PathError](oe); ok {
		path, reason = pe.Path, pe.Err
	}
	// Renumbering ran out of numbers: only auto-number can produce this, so
	// advising auto-number would name the mode that just failed.
	if errors.Is(oe, tempfile.ErrRenumberExhausted) {
		return renumberExhaustedMessage(path)
	}
	if errors.Is(oe, fs.ErrExist) {
		return fmt.Sprintf("output file already exists: %s (it may have appeared while this run was working; set --collision to auto-number, overwrite, or skip)", path)
	}
	msg := fmt.Sprintf("could not %s the finished file at %s: %v", oe.Op, path, reason)
	switch oe.Op {
	case "publish", "rename":
		msg += "; another process may be writing the same output path"
	}
	return msg
}

// renumberExhaustedMessage renders a give-up on the numbered sequence. The CLI
// pre-flight's stat walk and the publish retry stop at the same bound and mean
// the same thing to the user, so whichever of them gives up first says this.
func renumberExhaustedMessage(path string) string {
	return fmt.Sprintf("output file already exists: %s, and so does every numbered variant tried; clean up the directory or choose a different output name", path)
}

// incompleteStreamMessage renders an incomplete delivery: the fixed sentence,
// plus every attempt's own terminal cause when the chain recorded them.
//
// The sentence alone is what the 2026-08-15 pass had to work with, and it is not
// enough to act on: it names no client, no offset, and no refresh count, so a
// truncated body and an exhausted refresh budget read identically. The chain
// carries signed URLs, so the appended detail is redacted rather than surfaced.
func incompleteStreamMessage(err error) string {
	const lead = "the download ended before the full stream was received"
	ide, ok := errors.AsType[*waxtap.IncompleteDeliveryError](err)
	if !ok || len(ide.Attempts) == 0 {
		return lead
	}
	// Already redacted: the library builds Attempts through its own redactor so the
	// text is safe in a playlist run's NDJSON too, not only here.
	return lead + "; " + strings.Join(ide.Attempts, "; ")
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

// httpStatusHint gives class-appropriate guidance for an unexpected upstream
// status. 5xx and 408 mean the endpoint is failing; anything else means it
// answered and rejected the request, which is a staleness signal, not a
// connectivity one.
func httpStatusHint(status int) string {
	if status >= 500 || status == http.StatusRequestTimeout {
		return "the upstream service is failing; retry later"
	}
	return "the endpoint rejected the request; if it persists, WaxTap may need an update"
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
	// Unlike every other entry here, this setting is not reachable by flag on
	// every command: only download, cut, and sponsorblock bind
	// --sponsorblock-url, while the config key and environment variable work
	// everywhere. Naming the flag alone sent a `normalize` user to a flag that
	// command rejects, so name the setting and let the user pick a surface.
	"invalid SponsorBlock BaseURL", "invalid SponsorBlock base URL (--sponsorblock-url, sponsorBlockBaseURL, or WAXTAP_SPONSORBLOCK_BASE_URL)",
)

// translateConfigSymbols rewrites Go option field names in a config error message
// to their CLI flag equivalents.
func translateConfigSymbols(msg string) string {
	return configSymbolReplacer.Replace(msg)
}

// proxyStatusError reports a proxy that answered CONNECT with a non-200 status.
//
// It exists because Go returns a bare errors.New(reasonPhrase) for a failed
// CONNECT, with no status, no type, and no "proxyconnect" marker, so a 407 would
// otherwise keep the unclassified exit-1 default. config.go's
// OnProxyConnectResponse hook sees the real response and builds this instead.
// String-matching the reason phrase is not an option: it varies by status and is
// indistinguishable from any other error text.
//
// It stays in package main deliberately. httpx never builds a transport (it wraps
// an injected *http.Client), and config.go holds the only http.Transport in
// non-test code, so there is no second caller to serve and no public type to
// freeze.
type proxyStatusError struct {
	status int
}

func (e *proxyStatusError) Error() string {
	msg := fmt.Sprintf("the proxy rejected CONNECT with HTTP %d %s",
		e.status, http.StatusText(e.status))
	if e.status == http.StatusProxyAuthRequired {
		msg += "; put the credentials in the --proxy URL (http://user:pass@host:port)"
	}
	return msg
}

// proxyHint gives guidance matching how the proxy failed. A proxy that answered
// CONNECT with an error status is reachable and its own message already carries the
// remedy, so it gets guidance about the status instead of about connectivity.
func proxyHint(err error) string {
	pse, ok := errors.AsType[*proxyStatusError](err)
	if !ok {
		return "check the proxy is reachable and that --proxy is a correct URL"
	}
	if pse.status == http.StatusProxyAuthRequired {
		// The message already gives the URL form (it has to: --json has no hint
		// field), so repeating it here would print the same remedy twice in human
		// mode. Add only what the message cannot say.
		return "the same form works when the proxy comes from HTTPS_PROXY"
	}
	return "the proxy is reachable but refused the tunnel; check its configuration and access rules"
}

// isProxyError reports whether err is a failure to reach or negotiate with the
// configured proxy. It prefers typed unwrapping and falls back to a string match
// for transports that do not expose a typed proxyconnect error.
func isProxyError(err error) bool {
	if _, ok := errors.AsType[*proxyStatusError](err); ok {
		return true
	}
	if op, ok := errors.AsType[*net.OpError](err); ok && op.Op == "proxyconnect" {
		return true
	}
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.Err != nil && strings.Contains(ue.Err.Error(), "proxyconnect") {
		return true
	}
	return strings.Contains(err.Error(), "proxyconnect")
}

// providerHint names the remedy for the provider that failed. SponsorBlock is
// not a sidecar the user starts, so the sidecar-URL advice would be wrong there;
// every other provider is one of the two sidecars.
func providerHint(err error) string {
	if isSponsorBlockProvider(err) {
		return "the SponsorBlock server returned an unusable response; retry later, or check --sponsorblock-url if one is set"
	}
	return "start the provider sidecar or correct its URL (--player-context-url/--session-url)"
}

// isProviderError reports whether err came from a provider WaxTap calls over
// HTTP (player-context, session, or SponsorBlock). PO-token provider failures
// use ErrNeedsPOToken instead. Callers whose advice fits only the sidecars
// should pair this with isSponsorBlockProvider.
func isProviderError(err error) bool {
	_, ok := errors.AsType[*waxtap.ProviderError](err)
	return ok
}

// isSponsorBlockProvider reports a provider error attributed to SponsorBlock,
// the one provider that is neither a sidecar nor part of stream delivery.
func isSponsorBlockProvider(err error) bool {
	pe, ok := errors.AsType[*waxtap.ProviderError](err)
	return ok && pe.Endpoint == "SponsorBlock"
}

// isSidecarConnection reports whether err contains a sidecar connection failure.
func isSidecarConnection(err error) bool {
	_, ok := errors.AsType[*waxtap.SidecarError](err)
	return ok
}

// sidecarHint returns guidance for a sidecar refusal: authentication help for
// 401/403, a rate-limit advisory for 429, the endpoint URL to correct for a
// redirect, and the wait the sidecar asked for when it stated one. It rides the c.hint
// channel so package main needs no redact helper for the 429 message.
func sidecarHint(sre *waxtap.SidecarResponseError) string {
	var parts []string
	switch status := sre.StatusCode; {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		parts = append(parts, "the sidecar requires authentication; set or verify --api-key")
	case status == http.StatusTooManyRequests:
		parts = append(parts, "check the sidecar's rate limits")
	case status >= 300 && status < 400:
		parts = append(parts, "the sidecar redirected the request, which WaxTap does not follow; configure the endpoint's canonical URL")
	}
	if sre.RetryAfter > 0 {
		parts = append(parts, fmt.Sprintf("the sidecar asked for a retry in %s", sre.RetryAfter.Round(time.Second)))
	}
	return strings.Join(parts, "; ")
}

// sidecarResponseExit maps a sidecar response to its CLI exit code and machine
// code. Client errors and redirects indicate invalid configuration, 429
// indicates rate limiting, and timeouts, server errors, and invalid HTTP 200
// responses indicate network or provider failures.
//
// A video-unavailable refusal never reaches here: it unwraps to its availability
// verdict, which classifyArgs matches first.
func sidecarResponseExit(sre *waxtap.SidecarResponseError) (int, string) {
	status := sre.StatusCode
	switch {
	case status == http.StatusTooManyRequests:
		return 5, "rate-limited"
	case status >= 400 && status < 500 && status != http.StatusRequestTimeout:
		return 2, "invalid-config"
	case status >= 300 && status < 400:
		// Redirects are never followed (newSidecarClient), so a 3xx is the
		// endpoint URL being wrong: a non-canonical path, or a scheme the
		// daemon's proxy upgrades. WaxSeal answers a doubled slash with a 307.
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

// isCollisionError reports whether an exclusive publish found the destination
// already taken. It is a subset of isOutputError and is classified first.
func isCollisionError(err error) bool {
	oe, ok := errors.AsType[*tempfile.OutputError](err)
	return ok && errors.Is(oe, fs.ErrExist)
}

// isUsageError reports whether err marks a bad-arguments failure.
func isUsageError(err error) bool {
	_, ok := errors.AsType[*usageError](err)
	return ok
}

// embedHint returns fallback guidance for a web_embedded playability error.
func embedHint(err error) string {
	if pe, ok := errors.AsType[*waxtap.PlayabilityError](err); ok && pe.Embed {
		return "web_embedded currently falls back to web; use --client web or --client visionos"
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
// usage errors. args is the command line the failure came from, minus the
// program name.
func normalizeExecuteError(err error, args []string) error {
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
	// Cobra rejects an unknown command before parsing any flags, so a --json on
	// that line was never parsed.
	msg := err.Error()
	if !strings.HasPrefix(msg, "unknown command") && !strings.HasPrefix(msg, "unknown subcommand") {
		return err
	}
	// Cobra consumes a flag it does not know before it looks up the command, so
	// the token it blames is whatever followed. Naming the flag says what the
	// user has to change; the reported token is a bystander. Only the three
	// persistent bools below are registered at the root, so any other flag in
	// that position is unknown there whether or not a subcommand defines it.
	if flag, ok := misplacedFlag(args, rootSubcommandNames); ok {
		return unparsedFlagsError(fmt.Sprintf(
			"unknown global flag %q: only --json, --quiet, and --verbose may precede the subcommand; move other flags after it", flag))
	}
	return unparsedFlagsError(msg)
}

// flagOrderHint adds CLI help for YouTube-looking arguments that Cobra or pflag
// parsed before the command could receive them. args is the command line the
// failure came from, minus the program name.
func flagOrderHint(err error, args []string) string {
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
	// The misplaced-flag shape (`--no-cache info <id>` reported as an unknown
	// command) does not reach here: normalizeExecuteError rewrites its message
	// to name the flag before classification ever runs, so no hint is needed.
	return ""
}

// rootSubcommandNames is the set of subcommand names and aliases (including
// cobra's built-in help and completion) used to detect a flag placed before the
// subcommand. TestRootSubcommandNamesMatchTree keeps it aligned with the live
// command tree.
var rootSubcommandNames = map[string]bool{
	"info": true, "formats": true, "download": true, "cut": true,
	"transcode": true, "normalize": true, "split": true, "sponsorblock": true, "sb": true,
	"cache": true, "doctor": true, "version": true, "exit-codes": true,
	"help": true, "completion": true,
}

// rootPersistentFlag reports a token that spells one of the root's own
// persistent flags, which are the only flags legal before the subcommand. The
// set is the three bools newRootCmd registers, in long, =value, shorthand, and
// combined-shorthand form.
func rootPersistentFlag(tok string) bool {
	if long, _, ok := strings.Cut(tok, "="); ok {
		tok = long
	}
	switch tok {
	case "--json", "--quiet", "--verbose":
		return true
	}
	// A shorthand cluster like -qv is every shorthand it combines.
	if len(tok) < 2 || tok[0] != '-' || tok[1] == '-' {
		return false
	}
	for _, r := range tok[1:] {
		if r != 'q' && r != 'v' {
			return false
		}
	}
	return true
}

// misplacedFlag returns the first non-persistent flag token preceding the first
// recognized subcommand in args. cobra traversal can consume a later subcommand
// after an unknown bare boolean flag, turning `waxtap --no-cache info <id>`
// into `unknown command "<id>"`; this detects that shape and names the flag to
// blame. The root's own persistent flags are skipped: they are legal in that
// position, so `--json --bogus info <id>` blames --bogus, not --json. A bare
// "-" (stdin) and "--" (terminator) are not flags. A subcommand with no
// preceding blamable flag yields false so a genuine command typo is not given
// an ordering hint.
//
// The token returned is the flag, never the subcommand: a flag value can
// coincide with a subcommand name, so nothing here may claim which subcommand
// the user meant.
func misplacedFlag(args []string, names map[string]bool) (string, bool) {
	firstFlag := -1
	for i, a := range args {
		if firstFlag < 0 && len(a) > 1 && a[0] == '-' && a != "--" && !rootPersistentFlag(a) {
			firstFlag = i
		}
		if names[a] {
			if firstFlag >= 0 && firstFlag < i {
				return args[firstFlag], true
			}
			return "", false
		}
	}
	return "", false
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
