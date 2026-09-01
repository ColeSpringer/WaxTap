package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxtap/v3"
)

// runMain executes args the way main does: through the root command, then, for a
// terminal error, through report with the same writers. Commands that fail write
// their document from report rather than from their own RunE, so the --json
// contract can only be asserted through this seam.
func runMain(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	// report reads rootFlagsValue, which newRootCmd rebinds but no test resets on
	// the way out. Restoring it keeps a --json run from coloring whatever comes
	// next.
	saved := rootFlagsValue
	t.Cleanup(func() { rootFlagsValue = saved })

	var outBuf, errBuf bytes.Buffer
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	if err := root.Execute(); err != nil {
		code = report(&outBuf, &errBuf, args, normalizeExecuteError(err, args))
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
	saved := rootFlagsValue
	t.Cleanup(func() { rootFlagsValue = saved })
	rootFlagsValue = rootFlags{}

	var stdout, stderr bytes.Buffer
	code := report(&stdout, &stderr, []string{"info", "x", "--nope", "--json"}, unparsedFlagsError("unknown flag: --nope"))
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
// --json eaten as --format's value. rootFlagsValue is then the whole truth, and
// re-reading the args would turn a typo into a JSON document nobody asked for.
func TestReportIgnoresJSONAsAFlagValue(t *testing.T) {
	saved := rootFlagsValue
	t.Cleanup(func() { rootFlagsValue = saved })
	rootFlagsValue = rootFlags{}

	args := []string{"transcode", "nope.flac", "--format", "--json"}
	var stdout, stderr bytes.Buffer
	code := report(&stdout, &stderr, args, usagef("no such file and not a valid YouTube URL or ID: nope.flac"))
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
	report(&stdout, &stderr, []string{"bogus", "--json"}, normalizeExecuteError(errors.New(`unknown command "bogus" for "waxtap"`), []string{"bogus", "--json"}))
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
