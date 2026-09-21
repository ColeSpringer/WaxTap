package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxtap/v3"
)

// runMain executes args the way main does: through the root command, then, for a
// terminal error, through report with the same writers. Commands that fail write
// their document from report rather than from their own RunE, so the --json
// contract can only be asserted through this seam.
func runMain(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	if err := root.Execute(); err != nil {
		code = report(&outBuf, &errBuf, args, outputFlags(root).json, normalizeExecuteError(err, args))
	}
	return outBuf.String(), errBuf.String(), code
}

// oneJSONDoc decodes stdout as exactly one JSON object, failing if a second
// document follows it.
func oneJSONDoc(t *testing.T, stdout string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\nstdout:\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout carries more than one JSON document:\n%s", stdout)
	}
	return doc
}

// withArgs builds a fresh argument list, so a shared base slice cannot be aliased
// by two cases appending to it.
func withArgs(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	return append(append(out, base...), extra...)
}

// TestJSONContractOneDocument pins the invariant behind F4, F5, and F6: every
// terminal outcome writes exactly one JSON document, and it writes it to stdout.
func TestJSONContractOneDocument(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")

	for name, base := range map[string][]string{
		"transcode": {"transcode", in, "--format", "flac"},
		"normalize": {"normalize", in, "--format", "flac"},
		"cut":       {"cut", in, "--cut-range", "0-0.3", "--format", "flac"},
	} {
		t.Run(name, func(t *testing.T) {
			taken := filepath.Join(dir, name+"-taken.flac")
			if err := os.WriteFile(taken, []byte("present"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct {
				outcome  string
				args     []string
				wantCode int
			}{
				{"success", withArgs(base, "-o", filepath.Join(dir, name+"-ok.flac"), "--json"), 0},
				{"skip", withArgs(base, "-o", taken, "--collision", "skip", "--json"), 0},
				// Rejected inside RunE, after the flags parsed.
				{"usage error", withArgs(base, "-o", filepath.Join(dir, name+"-usage.flac"), "--itag", "0", "--json"), 2},
				// Rejected by the flag parser, which never reaches --json.
				{"flag-parse error", withArgs(base, "-o", filepath.Join(dir, name+"-parse.flac"), "--nope", "--json"), 2},
			} {
				t.Run(c.outcome, func(t *testing.T) {
					stdout, stderr, code := runMain(t, c.args...)
					if code != c.wantCode {
						t.Errorf("exit code = %d, want %d\nstderr:\n%s", code, c.wantCode, stderr)
					}
					oneJSONDoc(t, stdout)
					if strings.Contains(stderr, "schemaVersion") {
						t.Errorf("a JSON document reached stderr:\n%s", stderr)
					}
				})
			}
		})
	}
}

// TestSkipEmitsJSON: a run that wrote nothing because the output already existed
// is a terminal outcome and owes --json a document, the same as a write does.
func TestSkipEmitsJSON(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")

	for name, base := range map[string][]string{
		"transcode": {"transcode", in, "--format", "flac"},
		"normalize": {"normalize", in, "--format", "flac"},
		"cut":       {"cut", in, "--cut-range", "0-0.3", "--format", "flac"},
	} {
		t.Run(name, func(t *testing.T) {
			taken := filepath.Join(dir, name+".flac")
			if err := os.WriteFile(taken, []byte("present"), 0o644); err != nil {
				t.Fatal(err)
			}
			stdout, _, code := runMain(t, withArgs(base, "-o", taken, "--collision", "skip", "--json")...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			doc := oneJSONDoc(t, stdout)
			if doc["skipped"] != "exists" {
				t.Errorf(`skipped = %v, want "exists"`, doc["skipped"])
			}
			if doc["outputPath"] != displayPath(taken) {
				t.Errorf("outputPath = %v, want %q", doc["outputPath"], displayPath(taken))
			}
			if doc["schemaVersion"] != float64(schemaVersion) {
				t.Errorf("schemaVersion = %v, want %d", doc["schemaVersion"], schemaVersion)
			}
		})
	}

	// download shares the helper but keeps its existing pathless document: the
	// archive and collision skips it reports have no path to add.
	t.Run("download", func(t *testing.T) {
		taken := filepath.Join(dir, "dl.opus")
		if err := os.WriteFile(taken, []byte("present"), 0o644); err != nil {
			t.Fatal(err)
		}
		stdout, _, code := runMain(t, "download", "dummyVideo0", "-o", taken, "--collision", "skip", "--json")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		var want bytes.Buffer
		if err := writeJSON(&want, struct {
			SchemaVersion int    `json:"schemaVersion"`
			Skipped       string `json:"skipped"`
		}{schemaVersion, "exists"}); err != nil {
			t.Fatal(err)
		}
		if stdout != want.String() {
			t.Errorf("download skip document moved:\ngot:\n%s\nwant:\n%s", stdout, want.String())
		}
	})
}

// TestSkipQuietPrintsPath: --quiet promises the output path on stdout, and a skip
// leaves a usable file at that path, so `out=$(waxtap ... --quiet)` must not come
// back empty just because the file was already there.
func TestSkipQuietPrintsPath(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")
	taken := filepath.Join(dir, "taken.flac")
	if err := os.WriteFile(taken, []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runMain(t, "transcode", in, "--format", "flac", "-o", taken, "--collision", "skip", "--quiet")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	if got := strings.TrimRight(stdout, "\n"); got != displayPath(taken) {
		t.Errorf("quiet stdout = %q, want the existing path %q", stdout, displayPath(taken))
	}
	if strings.Count(stdout, "\n") != 1 {
		t.Errorf("quiet stdout should be exactly one line, got %q", stdout)
	}
	if stderr != "" {
		t.Errorf("quiet stderr = %q, want nothing", stderr)
	}
}

// TestMeasureOnlyReportsNoOutputBytes: a measurement sinks to io.Discard, so the
// byte count it used to report belonged to a file it never wrote.
func TestMeasureOnlyReportsNoOutputBytes(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	synthAudio(t, in, "flac")

	stdout, stderr, code := runMain(t, "normalize", in, "--measure-loudness", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	doc := oneJSONDoc(t, stdout)
	if doc["outputBytes"] != float64(0) {
		t.Errorf("outputBytes = %v, want 0 (nothing was written)", doc["outputBytes"])
	}
	if _, ok := doc["outputPath"]; ok {
		t.Errorf("outputPath = %v, want the key omitted", doc["outputPath"])
	}
	if b, ok := doc["sourceBytes"].(float64); !ok || b <= 0 {
		t.Errorf("sourceBytes = %v, want the measured input's size", doc["sourceBytes"])
	}
}

func TestJSONRequested(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"info", "x", "--nope", "--json"}, true}, // parsing aborts at --nope
		{[]string{"--json", "info"}, true},
		{[]string{"info", "--", "--json"}, false}, // -- terminates flags
		{[]string{"info", "--json=false"}, false},
		{[]string{"info"}, false},
	} {
		if got := jsonRequested(c.args); got != c.want {
			t.Errorf("jsonRequested(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestReportRendersJSONError: a flag the parser rejects aborts before --json is
// read, and the error still owes the contract a document on stdout.
func TestReportRendersJSONError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := report(&stdout, &stderr, []string{"info", "x", "--nope", "--json"}, false, unparsedFlagsError("unknown flag: --nope"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	doc := oneJSONDoc(t, stdout.String())
	errObj, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("document has no error object:\n%s", stdout.String())
	}
	if errObj["code"] != "usage" {
		t.Errorf("error.code = %v, want \"usage\"", errObj["code"])
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing (the document went to stdout)", stderr.String())
	}
}

// TestReportIgnoresJSONAsAFlagValue: `--format --json` parses cleanly, with
// --json eaten as --format's value. The parsed flags are then the whole truth,
// and re-reading the args would turn a typo into a JSON document nobody asked
// for.
func TestReportIgnoresJSONAsAFlagValue(t *testing.T) {
	args := []string{"transcode", "nope.flac", "--format", "--json"}
	var stdout, stderr bytes.Buffer
	code := report(&stdout, &stderr, args, false, usagef("no such file and not a valid YouTube URL or ID: nope.flac"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing: --json was a flag value, not a request", stdout.String())
	}
	if !strings.HasPrefix(stderr.String(), "waxtap: ") {
		t.Errorf("stderr = %q, want the human error line", stderr.String())
	}

	// An unknown command never reaches flag parsing either, so it keeps the probe.
	stdout.Reset()
	stderr.Reset()
	report(&stdout, &stderr, []string{"bogus", "--json"}, false, normalizeExecuteError(errors.New(`unknown command "bogus" for "waxtap"`), []string{"bogus", "--json"}))
	if stdout.Len() == 0 {
		t.Error("an unknown command with --json wrote no document; its flags were never parsed either")
	}
}

// Every document reports a failure the same way: {code, message}, from the one
// classifier the exit code also comes from. Before this, only the top-level
// envelope carried a code; a playlist item's error was a raw err.Error() string
// and a batch item's was friendlyError prose, so a consumer aggregating a run
// had to match text in two of the three shapes.
func TestErrorObjectsAreUniform(t *testing.T) {
	// The envelope, through the real seam a failing command uses.
	stdout, _, code := runMain(t, "info", "not a video id", "--json")
	if code == 0 {
		t.Fatal("a bad video ID succeeded")
	}
	doc := oneJSONDoc(t, stdout)
	envErr, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("envelope error = %#v, want an object", doc["error"])
	}
	for _, k := range []string{"code", "message"} {
		if v, _ := envErr[k].(string); v == "" {
			t.Errorf("envelope error.%s is empty: %#v", k, envErr)
		}
	}

	// A batch item record, which now carries the same object.
	rec := itemRecord(batchOutcome{index: 0, input: "in.wav", output: "out.flac",
		status: statusError, err: waxtap.ErrNeedsPOToken}, false)
	if rec.Error == nil {
		t.Fatal("batch item carried no error object")
	}
	if rec.Error.Code != "needs-po-token" {
		t.Errorf("batch item error.code = %q, want the classifier's code", rec.Error.Code)
	}
	if rec.Error.Message == "" {
		t.Error("batch item error.message is empty")
	}
}

// A batch item that failed or never ran wrote nothing, so it must not name an
// output path. The human renderer has always left it out.
func TestFailedBatchItemsOmitOutput(t *testing.T) {
	for _, st := range []batchStatus{statusError, statusNotRun} {
		rec := itemRecord(batchOutcome{index: 0, input: "in.wav", output: "out.flac",
			status: st, err: errors.New("boom")}, false)
		if rec.Output != "" {
			t.Errorf("status %s named output %q, but nothing was written", st, rec.Output)
		}
	}
	ok := itemRecord(batchOutcome{index: 0, input: "in.wav", output: "out.flac", status: statusOK}, false)
	if ok.Output == "" {
		t.Error("a successful item lost its output path")
	}
}

// Batch summary counts are unconditional so a consumer never has to read an
// absent key as zero, and total lets a summary be checked without summing the
// other seven and hoping the set is complete.
func TestBatchSummaryCountsAreStable(t *testing.T) {
	var buf bytes.Buffer
	emitBatchSummaryJSON(&appEnv{out: &buf}, batchCounts{processed: 2, failed: 1})

	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("summary is not JSON: %v (%s)", err, buf.String())
	}
	for _, k := range []string{"total", "processed", "copied", "unchanged", "skipped", "ignored", "failed", "notRun"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("summary omits %q; every count must be present, including the zeros", k)
		}
	}
	if got, _ := doc["total"].(float64); got != 3 {
		t.Errorf("total = %v, want 3 (2 processed + 1 failed)", got)
	}
}

// Notes used to live only in stderr prose, gated by --quiet, so --json --quiet
// (the combination a script actually uses) lost them entirely. They are now
// collected regardless of --quiet and carried as {code, detail}, the same shape
// warnings use, so a consumer matches a code instead of a sentence.
func TestNotesSurviveQuietAndCarryCodes(t *testing.T) {
	env := &appEnv{
		out: io.Discard, errOut: io.Discard,
		cfg: &appConfig{json: true, quiet: true}, notes: &noteCollector{},
	}
	env.note(noteContainerExtMismatch, "output path uses .%s", "mp3")

	got := env.notesJSON()
	if len(got) != 1 {
		t.Fatalf("notes = %+v, want one collected under --quiet", got)
	}
	if got[0].Code != "container-ext-mismatch" {
		t.Errorf("code = %q, want the stable kebab code", got[0].Code)
	}
	if got[0].Detail != "output path uses .mp3" {
		t.Errorf("detail = %q, want the formatted sentence with no note: prefix", got[0].Detail)
	}
}

// A note prints to stderr with its prefix when not quiet, and the prefix belongs
// to the helper so no call site can spell it differently.
func TestNotePrintsPrefixedLine(t *testing.T) {
	var errBuf bytes.Buffer
	env := &appEnv{out: io.Discard, errOut: &errBuf, cfg: &appConfig{}, notes: &noteCollector{}}
	env.note(noteConcurrencyClamped, "clamping to %d", 4)
	if got := errBuf.String(); got != "note: clamping to 4\n" {
		t.Errorf("stderr = %q, want a prefixed note line", got)
	}
}

// Playlist items are written from concurrent, out-of-order handlers, so a shared
// collector would attribute one item's note to whichever record was written
// next. Each item collects in its own scope, and draining it leaves nothing for
// the following item to inherit.
func TestScopedNotesDoNotLeakBetweenItems(t *testing.T) {
	run := &appEnv{out: io.Discard, errOut: io.Discard, cfg: &appConfig{}, notes: &noteCollector{}}

	first := run.withScopedNotes()
	first.note(noteUnalteredCopy, "item one")
	if got := first.notes.drain(); len(got) != 1 || got[0].Detail != "item one" {
		t.Fatalf("first item drained %+v, want its own note", got)
	}

	second := run.withScopedNotes()
	if got := second.notes.drain(); len(got) != 0 {
		t.Errorf("second item inherited %+v", got)
	}
	// The run scope stays clean: item notes belong to item records.
	if got := run.notesJSON(); len(got) != 0 {
		t.Errorf("run scope collected item notes: %+v", got)
	}
}

// Concurrent items must not race the collector. Run with -race.
func TestScopedNotesConcurrent(t *testing.T) {
	run := &appEnv{out: io.Discard, errOut: io.Discard, cfg: &appConfig{quiet: true}, notes: &noteCollector{}}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			item := run.withScopedNotes()
			item.note(noteUnalteredCopy, "item %d", i)
			run.note(noteKeptOutput, "run note %d", i)
			if got := item.notes.drain(); len(got) != 1 {
				t.Errorf("item %d drained %d notes, want 1", i, len(got))
			}
		}(i)
	}
	wg.Wait()
	if got := run.notesJSON(); len(got) != 8 {
		t.Errorf("run collected %d notes, want 8", len(got))
	}
}

// An Opus source normalized under cap is delivered as a packet copy whose head
// carries the gain, so the document is a remux document: no re-encode, no
// output format, and the gain reported under loudness.
func TestOpusHeaderGainDocument(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.opus")
	synthAudio(t, in, "libopus")
	out := filepath.Join(dir, "out.opus")

	stdout, stderr, code := runMain(t, "normalize", in, "-o", out, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	doc := oneJSONDoc(t, stdout)
	if doc["schemaVersion"] != float64(schemaVersion) {
		t.Errorf("schemaVersion = %v, want %d", doc["schemaVersion"], schemaVersion)
	}
	if doc["transcoded"] != false {
		t.Errorf("transcoded = %v, want false: the packets were copied", doc["transcoded"])
	}
	if _, ok := doc["outputFormat"]; ok {
		t.Errorf("outputFormat = %v, want the key omitted for a local result that was not transcoded", doc["outputFormat"])
	}
	if doc["outputPath"] == nil {
		t.Error("outputPath missing")
	}
	l, ok := doc["loudness"].(map[string]any)
	if !ok {
		t.Fatalf("loudness = %v, want an object", doc["loudness"])
	}
	if l["headerGain"] != true {
		t.Errorf("loudness.headerGain = %v, want true", l["headerGain"])
	}
	if g, ok := l["gainDb"].(float64); !ok || g == 0 {
		t.Errorf("loudness.gainDb = %v, want the applied gain", l["gainDb"])
	}

	// A measurement applies nothing, so neither key is there to mislead.
	stdout, stderr, code = runMain(t, "normalize", in, "--measure-loudness", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, stderr)
	}
	l, _ = oneJSONDoc(t, stdout)["loudness"].(map[string]any)
	if _, ok := l["gainDb"]; ok {
		t.Errorf("measure-only carries gainDb = %v", l["gainDb"])
	}
	if _, ok := l["headerGain"]; ok {
		t.Errorf("measure-only carries headerGain = %v", l["headerGain"])
	}
}

// split takes neither -o nor --collision skip, so it pins the one-document
// invariant on its own shapes: a written set, a refusal inside RunE, and one
// the flag parser makes before --json is ever read.
func TestJSONContractOneDocumentSplit(t *testing.T) {
	dir := t.TempDir()
	rip := filepath.Join(dir, "rip.wav")
	synthAudio(t, rip, "wav")
	cue := filepath.Join(dir, "rip.cue")
	if err := os.WriteFile(cue, []byte("FILE \"rip.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:00:37\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		outcome  string
		args     []string
		wantCode int
	}{
		{"success", []string{"split", rip, "--cue", cue, "-f", "flac", "-d", filepath.Join(dir, "ok"), "--json"}, 0},
		{"usage error", []string{"split", rip, "--cue", cue, "-f", "copy", "--json"}, 2},
		{"flag-parse error", []string{"split", rip, "--nope", "--json"}, 2},
	} {
		t.Run(c.outcome, func(t *testing.T) {
			stdout, stderr, code := runMain(t, c.args...)
			if code != c.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr:\n%s", code, c.wantCode, stderr)
			}
			oneJSONDoc(t, stdout)
			if strings.Contains(stderr, "schemaVersion") {
				t.Errorf("a JSON document reached stderr:\n%s", stderr)
			}
		})
	}
}

// badRecord marshals as a failure, standing in for a record whose contents the
// encoder refuses (a non-finite float reached through an any field).
type badRecord struct{}

func (badRecord) MarshalJSON() ([]byte, error) { return nil, errors.New("boom") }

// An NDJSON stream promises one record per item. A record the encoder refuses
// used to vanish, leaving a consumer counting a shorter stream with no sign
// anything was lost.
func TestWriteRecordKeepsTheCountHonest(t *testing.T) {
	var out bytes.Buffer
	writeRecord(&out, struct {
		SchemaVersion int `json:"schemaVersion"`
		Payload       any `json:"payload"`
	}{schemaVersion, badRecord{}})

	line := strings.TrimRight(out.String(), "\n")
	if line == "" || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("output = %q, want exactly one line", out.String())
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(line), &doc); err != nil {
		t.Fatalf("the replacement record is not JSON: %v (%q)", err, line)
	}
	if doc["schemaVersion"] != float64(schemaVersion) || doc["type"] != "error" {
		t.Errorf("record = %v, want the error envelope", doc)
	}
	e, _ := doc["error"].(map[string]any)
	if msg, _ := e["message"].(string); !strings.Contains(msg, "encode record") {
		t.Errorf("error = %v, want it to name the encode failure", doc["error"])
	}

	// A record that marshals is written as itself.
	out.Reset()
	writeRecord(&out, map[string]any{"ok": true})
	if got := strings.TrimRight(out.String(), "\n"); got != `{"ok":true}` {
		t.Errorf("record = %q", got)
	}
}

// TestSubcommandOutputModeIsItsOwn pins the order dependence the output flags
// used to carry: they lived in a package-level value that only newRootCmd
// rebound, so a subcommand a test built on its own inherited whatever the last
// root run had asked for and printed NDJSON at a human assertion. The failure
// needed a shuffle to show; this runs the two in order on purpose.
func TestSubcommandOutputModeIsItsOwn(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"version", "--json"})
	var rootOut bytes.Buffer
	root.SetOut(&rootOut)
	root.SetErr(&rootOut)
	if err := root.Execute(); err != nil {
		t.Fatalf("version --json: %v\n%s", err, rootOut.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(rootOut.String()), "{") {
		t.Fatalf("version --json wrote no document:\n%s", rootOut.String())
	}

	dir := t.TempDir()
	in := filepath.Join(dir, "a.flac")
	synthAudio(t, in, "flac")

	for _, c := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{"normalize", newNormalizeCmd(), []string{in, "--measure-loudness"}},
		{"transcode", newTranscodeCmd(), []string{in, "--format", "mp3", "-o", filepath.Join(dir, "a.mp3")}},
	} {
		c.cmd.SetArgs(c.args)
		var buf bytes.Buffer
		c.cmd.SetOut(&buf)
		c.cmd.SetErr(&buf)
		if err := c.cmd.Execute(); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, buf.String())
		}
		if got := buf.String(); strings.Contains(got, `"schemaVersion"`) {
			t.Errorf("%s built on its own printed JSON; --json belongs to the root run that asked for it:\n%s", c.name, got)
		}
	}
}

// A cut document names the mode that rendered it, and carries the joins a
// packet copy moved. A re-encoded cut names accurate and omits the figures.
func TestJSONCutDocumentCarriesTheCutMode(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.opus")
	synthChannels(t, in, "libopus", 2)

	stdout, stderr, code := runMain(t, "cut", in, filepath.Join(dir, "copy.opus"),
		"--cut-range", "0.21-0.41", "--cut-range", "0.61-0.81", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	doc := oneJSONDoc(t, stdout)
	if doc["cutMode"] != "copy" {
		t.Errorf("cutMode = %v, want copy", doc["cutMode"])
	}
	snaps, _ := doc["cutSnaps"].(float64)
	if snaps < 1 {
		t.Errorf("cutSnaps = %v, want the interior joins the copy moved", doc["cutSnaps"])
	}
	if _, ok := doc["cutSnapMaxMs"]; !ok {
		t.Errorf("cutSnapMaxMs missing from a document that snapped: %v", doc)
	}

	stdout, stderr, code = runMain(t, "cut", in, filepath.Join(dir, "acc.flac"),
		"--cut-range", "0.21-0.41", "--cut-mode", "accurate", "--format", "flac", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	doc = oneJSONDoc(t, stdout)
	if doc["cutMode"] != "accurate" {
		t.Errorf("cutMode = %v, want accurate", doc["cutMode"])
	}
	if _, ok := doc["cutSnaps"]; ok {
		t.Errorf("cutSnaps = %v, want the key omitted when nothing moved", doc["cutSnaps"])
	}
}

// `cache clean` reports truthfully in --json: a directory holding nothing of
// WaxTap's is left alone and says so, and the file that shares its name
// survives.
func TestJSONCacheCleanReportsWhatItRemoved(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notacache")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "sub", "data.bin")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runMain(t, "cache", "clean", "--cache-dir", dir, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	doc := oneJSONDoc(t, stdout)
	if doc["removed"] != false {
		t.Errorf("removed = %v, want false", doc["removed"])
	}
	if _, serr := os.Stat(keep); serr != nil {
		t.Fatalf("cache clean removed a file it did not write: %v", serr)
	}
}

// assertJSONCodec checks the codec a result document reports: outputFormat's
// when the run transcoded, sourceFormat's when it did not (a copy leaves both
// describing the same audio).
func assertJSONCodec(t *testing.T, doc map[string]any, wantCodec string, wantTranscoded bool) {
	t.Helper()
	if got, _ := doc["transcoded"].(bool); got != wantTranscoded {
		t.Errorf("transcoded = %v, want %v", doc["transcoded"], wantTranscoded)
	}
	key := "sourceFormat"
	if wantTranscoded {
		key = "outputFormat"
	}
	f, _ := doc[key].(map[string]any)
	if got, _ := f["codec"].(string); got != wantCodec {
		t.Errorf("%s.codec = %v, want %q", key, f["codec"], wantCodec)
	}
}

// assertJSONWarning checks a document's warnings for one code. An empty want
// asserts that implicit-lossy is absent, which is the one the container rule
// can raise by accident.
func assertJSONWarning(t *testing.T, doc map[string]any, want string) {
	t.Helper()
	ws, _ := doc["warnings"].([]any)
	var codes []string
	for _, w := range ws {
		m, _ := w.(map[string]any)
		c, _ := m["code"].(string)
		codes = append(codes, c)
	}
	if want == "" {
		if slices.Contains(codes, "implicit-lossy") {
			t.Errorf("warnings = %v, want no implicit-lossy", codes)
		}
		return
	}
	if !slices.Contains(codes, want) {
		t.Errorf("warnings = %v, want %q", codes, want)
	}
}

// D1: an extension names a container. With no --format the source codec is
// kept when the container holds it, else the container's usual encoder runs
// and says so. The same rule on every command.
func TestExtensionNamesAContainer(t *testing.T) {
	dir := t.TempDir()
	opus := filepath.Join(dir, "in.opus")
	mp3 := filepath.Join(dir, "in.mp3")
	synthChannels(t, opus, "libopus", 2)
	synthChannels(t, mp3, "libmp3lame", 2)
	cases := []struct {
		name       string
		args       []string
		wantCodec  string // outputFormat.codec, or sourceFormat.codec when transcoded is false
		transcoded bool
		wantWarn   string
	}{
		{"transcode opus to ogg keeps opus", []string{"transcode", opus, filepath.Join(dir, "a.ogg")}, "opus", false, ""},
		{"transcode opus to mka keeps opus", []string{"transcode", opus, filepath.Join(dir, "b.mka")}, "opus", false, ""},
		{"transcode mp3 to mka takes the container's encoder and says so", []string{"transcode", mp3, filepath.Join(dir, "c.mka")}, "opus", true, "implicit-lossy"},
		{"cut mp3 to mka says so", []string{"cut", mp3, filepath.Join(dir, "d.mka"), "--cut-range", "0-0.5"}, "opus", true, "implicit-lossy"},
		{"normalize opus to ogg takes the header gain", []string{"normalize", opus, filepath.Join(dir, "e.ogg")}, "opus", false, ""},
		{"normalize opus to webm takes the header gain", []string{"normalize", opus, filepath.Join(dir, "f.webm")}, "opus", false, ""},
		{"explicit --format ogg still means vorbis", []string{"transcode", opus, filepath.Join(dir, "g.ogg"), "--format", "ogg"}, "vorbis", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runMain(t, append(tc.args, "--json")...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			doc := oneJSONDoc(t, stdout)
			assertJSONCodec(t, doc, tc.wantCodec, tc.transcoded)
			assertJSONWarning(t, doc, tc.wantWarn)
		})
	}
}

// A copy cut with a crossfade is refused on the contradiction it really is,
// not on a missing --format: the crossfade decodes, which is what copy forbids.
func TestCutCopyModeNamesTheRealConflict(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	synthChannels(t, in, "pcm_s16le", 2)
	_, stderr, code := runMain(t, "cut", in, filepath.Join(dir, "out.wav"),
		"--cut-range", "0.1-0.5", "--crossfade", "10ms", "--cut-mode", "copy")
	if code != 2 {
		t.Fatalf("exit %d, want 2: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--crossfade") {
		t.Errorf("stderr = %q, want it to name --crossfade", stderr)
	}
	if strings.Contains(stderr, "--format") {
		t.Errorf("stderr = %q, want it not to blame --format", stderr)
	}
}
