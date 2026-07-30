package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/spf13/cobra"
)

// TestResolveValidatesOutputTemplate checks that malformed templates fail during
// flag resolution, before download or playlist work starts.
func TestResolveValidatesOutputTemplate(t *testing.T) {
	df := &downloadFlags{}
	cmd := &cobra.Command{Use: "download"}
	bindDownloadFlags(cmd, df)
	mustSet(t, cmd, "output-template", "{artist}")
	err := df.resolve(cmd, testResolveEnv())
	if !isUsageError(err) || !strings.Contains(err.Error(), "output-template") {
		t.Fatalf("resolve err = %v, want an --output-template usage error", err)
	}
}

func TestResolveValidatesProcessSpec(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
	}{
		{"negative bitrate", map[string]string{"format": "mp3", "bitrate": "-1"}},
		{"out-of-range loudness target", map[string]string{"format": "flac", "normalize": "true", "loudness-target": "50"}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			df := &downloadFlags{}
			cmd := &cobra.Command{Use: "download"}
			bindDownloadFlags(cmd, df)
			for name, val := range tt.set {
				mustSet(t, cmd, name, val)
			}
			if err := df.resolve(cmd, testResolveEnv()); !errors.Is(err, waxtap.ErrIncompatibleSpec) {
				t.Fatalf("resolve err = %v, want ErrIncompatibleSpec", err)
			}
		})
	}
}

// Publish date and chapters come only from the watch-page pass, so the flags that
// promise them have to turn it on.
func TestBuildRequestFullMetadata(t *testing.T) {
	cases := []struct {
		name string
		df   *downloadFlags
		want bool
	}{
		{"embed-metadata", &downloadFlags{embedMetadata: true}, true},
		{"write-info-json", &downloadFlags{writeInfoJSON: true}, true},
		{"both", &downloadFlags{embedMetadata: true, writeInfoJSON: true}, true},
		{"neither", &downloadFlags{}, false},
		// A thumbnail carries no date or chapters, so it buys no extra fetch.
		{"embed-thumbnail only", &downloadFlags{embedThumbnail: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := tc.df.buildRequest("dummyVideo0", "out.opus")
			if err != nil {
				t.Fatal(err)
			}
			if req.FullMetadata != tc.want {
				t.Errorf("FullMetadata = %v, want %v", req.FullMetadata, tc.want)
			}
		})
	}
}

// TestBuildCutSpecValidatesCutModeWithoutRanges ensures invalid input fails
// before extraction, even when no cut ranges are configured.
func TestBuildCutSpecValidatesCutModeWithoutRanges(t *testing.T) {
	df := &downloadFlags{cutMode: "bogus"} // no ranges, no SponsorBlock
	if _, err := df.buildCutSpec(); err == nil {
		t.Fatal("buildCutSpec err = nil, want a cut-mode validation error before the no-cut early return")
	}
	// No cut work still yields a nil spec.
	if cs, err := (&downloadFlags{}).buildCutSpec(); err != nil || cs != nil {
		t.Errorf("buildCutSpec(empty) = (%v, %v), want (nil, nil)", cs, err)
	}
}

func TestBuildCutSpecValidatesSponsorErrorWithoutRanges(t *testing.T) {
	df := &downloadFlags{cutMode: "smart", sbOnError: "bogus"}
	_, err := df.buildCutSpec()
	if err == nil {
		t.Fatal("buildCutSpec err = nil, want a --sponsorblock-on-error validation error before the no-cut early return")
	}
	if !isUsageError(err) {
		t.Errorf("err = %#v, want a usageError (exit 2)", err)
	}
}

// TestMeasureSpecCarriesTarget guards the JSON contract: a measure-only run
// reports the configured loudness target instead of zero.
func TestMeasureSpecCarriesTarget(t *testing.T) {
	df := &downloadFlags{measure: true, loudTarget: -16}
	ls, err := df.buildLoudnessSpec()
	if err != nil {
		t.Fatal(err)
	}
	if ls == nil || ls.Mode != waxtap.LoudnessMeasureOnly {
		t.Fatalf("buildLoudnessSpec = %+v, want a measure-only spec", ls)
	}
	if ls.Target != -16 {
		t.Errorf("measure-only Target = %v, want -16 (carried into --json output)", ls.Target)
	}
}

// TestBuildProcessSpecNormalizeRequiresEncode covers flag combinations before
// any download starts.
func TestBuildProcessSpecNormalizeRequiresEncode(t *testing.T) {
	cases := []struct {
		name      string
		df        *downloadFlags
		wantError bool
	}{
		{"normalize without format", &downloadFlags{normalize: true, loudTarget: -14}, true},
		{"normalize + copy", &downloadFlags{normalize: true, format: "copy", loudTarget: -14}, true},
		{"normalize + flac", &downloadFlags{normalize: true, format: "flac", loudTarget: -14}, false},
		{"measure only", &downloadFlags{measure: true}, false},
		{"keep source", &downloadFlags{}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.df.buildProcessSpec()
			if tt.wantError && err == nil {
				t.Error("expected an error")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestReportKeptOutput covers F6: a SIGINT that lands after finalization used to
// exit 130 with a complete file on disk and say nothing about it. Every writer
// commits atomically, so a file at the output path is whole; the gap was reporting.
func TestReportKeptOutput(t *testing.T) {
	canceled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	// newFile writes path and returns the stamp taken before it existed.
	newFile := func(t *testing.T, path, body string) outputStamp {
		t.Helper()
		before := stampOutputPath(path)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return before
	}
	run := func(t *testing.T, ctx context.Context, df *downloadFlags, path string, before outputStamp, jsonMode bool) (error, string, string) {
		t.Helper()
		var out, errOut bytes.Buffer
		env := &appEnv{cfg: &appConfig{json: jsonMode}, out: &out, errOut: &errOut}
		got := env.reportKeptOutput(ctx, df, path, before, context.Canceled)
		return got, out.String(), errOut.String()
	}

	t.Run("names a file the canceled run committed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "track.flac")
		before := newFile(t, path, "audio-bytes")
		err, _, errOut := run(t, canceled(), &downloadFlags{}, path, before, false)
		if _, ok := errors.AsType[*alreadyRenderedError](err); !ok {
			t.Fatalf("err = %v (%T), want *alreadyRenderedError", err, err)
		}
		if got := exitCodeFor(err); got != 130 {
			t.Errorf("exit code = %d, want 130", got)
		}
		if !strings.Contains(errOut, path) {
			t.Errorf("human output does not name the kept file:\n%s", errOut)
		}
		if !strings.Contains(errOut, "11 bytes") {
			t.Errorf("human output does not report the size:\n%s", errOut)
		}
	})

	t.Run("json names the path and byte count", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "track.flac")
		before := newFile(t, path, "audio-bytes")
		err, out, _ := run(t, canceled(), &downloadFlags{}, path, before, true)
		if _, ok := errors.AsType[*alreadyRenderedError](err); !ok {
			t.Fatalf("err = %v, want *alreadyRenderedError", err)
		}
		var doc struct {
			SchemaVersion int    `json:"schemaVersion"`
			OutputPath    string `json:"outputPath"`
			OutputBytes   int64  `json:"outputBytes"`
			Error         struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if uerr := json.Unmarshal([]byte(out), &doc); uerr != nil {
			t.Fatalf("unmarshal %q: %v", out, uerr)
		}
		if doc.SchemaVersion != schemaVersion {
			t.Errorf("schemaVersion = %d, want %d (the fields are additive)", doc.SchemaVersion, schemaVersion)
		}
		if doc.OutputPath != path || doc.OutputBytes != 11 {
			t.Errorf("document = %+v, want path %s and 11 bytes", doc, path)
		}
		if doc.Error.Code != "canceled" {
			t.Errorf("error.code = %q, want canceled (naming the file does not make it a success)", doc.Error.Code)
		}
	})

	t.Run("stays silent on a stale file the run never touched", func(t *testing.T) {
		// The --collision overwrite case: a file was already there and the run was
		// canceled before replacing it. Reporting it would name the wrong bytes.
		path := filepath.Join(t.TempDir(), "track.flac")
		if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := stampOutputPath(path)
		err, out, errOut := run(t, canceled(), &downloadFlags{}, path, before, false)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want the original error", err)
		}
		if _, ok := errors.AsType[*alreadyRenderedError](err); ok {
			t.Error("a stale file must not be rendered as a kept output")
		}
		if out != "" || errOut != "" {
			t.Errorf("expected no output, got stdout=%q stderr=%q", out, errOut)
		}
	})

	t.Run("stays silent when the run was not canceled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "track.flac")
		before := newFile(t, path, "audio-bytes")
		err, _, errOut := run(t, context.Background(), &downloadFlags{}, path, before, false)
		if _, ok := errors.AsType[*alreadyRenderedError](err); ok {
			t.Error("a non-cancellation must keep its own error and rendering")
		}
		if errOut != "" {
			t.Errorf("expected no output, got %q", errOut)
		}
	})

	t.Run("stays silent for a stdout stream", func(t *testing.T) {
		// -o - has no output path, so this and the audio-sink render seam cannot both
		// fire.
		err, out, errOut := run(t, canceled(), &downloadFlags{}, "", outputStamp{}, false)
		if _, ok := errors.AsType[*alreadyRenderedError](err); ok {
			t.Error("a stdout stream has no path to keep")
		}
		if out != "" || errOut != "" {
			t.Errorf("expected no output, got stdout=%q stderr=%q", out, errOut)
		}
	})

	t.Run("stays silent on an empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "track.flac")
		before := newFile(t, path, "")
		err, _, errOut := run(t, canceled(), &downloadFlags{}, path, before, false)
		if _, ok := errors.AsType[*alreadyRenderedError](err); ok {
			t.Error("a zero-byte file is not a delivered output")
		}
		if errOut != "" {
			t.Errorf("expected no output, got %q", errOut)
		}
	})

	t.Run("warns that the archive was not updated", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "track.flac")
		before := newFile(t, path, "audio-bytes")
		arch, aerr := openArchive(filepath.Join(dir, "archive.txt"))
		if aerr != nil {
			t.Fatal(aerr)
		}
		_, _, errOut := run(t, canceled(), &downloadFlags{archive: arch}, path, before, false)
		if !strings.Contains(errOut, "download archive") {
			t.Errorf("no archive note; a re-run silently refetches:\n%s", errOut)
		}
	})
}

// TestNoteKeptItem covers the playlist half of the same gap. An item's result line
// reports the failure but knows no path, so a file a canceled item finished is
// invisible: absent from the archive, and under the default --collision fail it
// stops the re-run with nothing to explain why.
func TestNoteKeptItem(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	// outcome is one attempted item that failed, which is what a cancellation
	// mid-item produces.
	outcome := func(id string) waxtap.PlaylistItemOutcome {
		return waxtap.PlaylistItemOutcome{
			Entry:     waxtap.PlaylistEntry{VideoID: id},
			Attempted: true,
			Err:       context.Canceled,
		}
	}
	run := func(t *testing.T, ctx context.Context, df *downloadFlags, id string, stamps *itemStamps) string {
		t.Helper()
		var errOut bytes.Buffer
		env := &appEnv{cfg: &appConfig{}, out: io.Discard, errOut: &errOut}
		noteKeptItem(ctx, env, df, stamps, outcome(id))
		return errOut.String()
	}
	// staged records the item, then writes the file the download would have
	// committed just before the cancellation landed.
	staged := func(t *testing.T, path string) *itemStamps {
		t.Helper()
		s := newItemStamps()
		s.record("dummyVideo0", path)
		if err := os.WriteFile(path, []byte("audio-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("names a file the canceled item committed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "01 - track.flac")
		errOut := run(t, canceled, &downloadFlags{}, "dummyVideo0", staged(t, path))
		if !strings.Contains(errOut, path) || !strings.Contains(errOut, "11 bytes") {
			t.Errorf("the kept file is not named with its size:\n%s", errOut)
		}
	})

	t.Run("says the archive was not updated", func(t *testing.T) {
		dir := t.TempDir()
		arch, aerr := openArchive(filepath.Join(dir, "archive.txt"))
		if aerr != nil {
			t.Fatal(aerr)
		}
		path := filepath.Join(dir, "01 - track.flac")
		errOut := run(t, canceled, &downloadFlags{archive: arch}, "dummyVideo0", staged(t, path))
		if !strings.Contains(errOut, "download archive") {
			t.Errorf("no archive note; a re-run silently refetches:\n%s", errOut)
		}
	})

	t.Run("stays silent for a file that was already there", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "01 - track.flac")
		if err := os.WriteFile(path, []byte("old-bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := newItemStamps()
		s.record("dummyVideo0", path) // stamped with the stale file in place
		if errOut := run(t, canceled, &downloadFlags{}, "dummyVideo0", s); errOut != "" {
			t.Errorf("a file this run never wrote was reported as kept: %q", errOut)
		}
	})

	t.Run("stays silent when the run was not canceled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "01 - track.flac")
		if errOut := run(t, t.Context(), &downloadFlags{}, "dummyVideo0", staged(t, path)); errOut != "" {
			t.Errorf("an ordinary item failure reported a kept file: %q", errOut)
		}
	})

	t.Run("stays silent for a skipped item", func(t *testing.T) {
		// A skip resolves no path, so there is nothing recorded to compare.
		if errOut := run(t, canceled, &downloadFlags{}, "dummyVideo0", newItemStamps()); errOut != "" {
			t.Errorf("an unrecorded item reported a kept file: %q", errOut)
		}
	})
}
