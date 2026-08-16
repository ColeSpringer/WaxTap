package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		code = report(&outBuf, &errBuf, args, normalizeExecuteError(err))
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
	report(&stdout, &stderr, []string{"bogus", "--json"}, normalizeExecuteError(errors.New(`unknown command "bogus" for "waxtap"`)))
	if stdout.Len() == 0 {
		t.Error("an unknown command with --json wrote no document; its flags were never parsed either")
	}
}
