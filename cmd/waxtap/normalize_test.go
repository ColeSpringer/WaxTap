package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
)

// TestAlbumTableHeaders guards the album table labels. Process output uses
// IN-LUFS for per-track input loudness because the row also includes OUTPUT;
// measure output has no output column and keeps LUFS.
func TestAlbumTableHeaders(t *testing.T) {
	inputs := []string{"a.flac", "b.flac"}
	perTrack := []waxtap.LoudnessInfo{{IntegratedLUFS: -11}, {IntegratedLUFS: -13}}

	t.Run("process table uses IN-LUFS", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		res := &waxtap.AlbumProcessResult{
			Album:    waxtap.LoudnessInfo{IntegratedLUFS: -12},
			GainDB:   -2,
			PerTrack: perTrack,
			Outputs:  []string{"out/a.flac", "out/b.flac"},
		}
		if err := emitAlbumProcess(env, inputs, res); err != nil {
			t.Fatal(err)
		}
		if s := out.String(); !strings.Contains(s, "IN-LUFS") || !strings.Contains(s, "OUTPUT") {
			t.Errorf("process table header = %q, want IN-LUFS beside OUTPUT", s)
		}
	})

	t.Run("measure table keeps LUFS", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		res := &waxtap.AlbumLoudnessResult{
			Album:    waxtap.LoudnessInfo{IntegratedLUFS: -12},
			PerTrack: perTrack,
		}
		if err := emitAlbumMeasure(env, inputs, res); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		// Locate the row naming TRACK and require the plain LUFS column.
		var header string
		for line := range strings.SplitSeq(s, "\n") {
			if strings.Contains(line, "TRACK") {
				header = line
				break
			}
		}
		if header == "" {
			t.Fatalf("measure table has no TRACK header line: %q", s)
		}
		if !strings.Contains(header, "LUFS") || strings.Contains(header, "IN-LUFS") {
			t.Errorf("measure table header = %q, want a plain LUFS column", header)
		}
	})
}

func TestNormalizeFlagSurface(t *testing.T) {
	flags := newNormalizeCmd().Flags()
	for _, name := range []string{"measure-loudness", "loudness-target", "format", "bitrate", "collision"} {
		if flags.Lookup(name) == nil {
			t.Errorf("normalize should expose --%s", name)
		}
	}
	// Normalize writes by default; --measure-loudness selects analysis-only mode.
	for _, name := range []string{"apply", "normalize", "measure"} {
		if flags.Lookup(name) != nil {
			t.Errorf("normalize should not expose --%s", name)
		}
	}
}

// TestNormalizeMeasureRejectsWriteFlags verifies that analysis-only mode rejects
// output-related flags and a positional output path.
func TestNormalizeMeasureRejectsWriteFlags(t *testing.T) {
	for _, args := range [][]string{
		{"in.wav", "--measure-loudness", "--loudness-target", "-16"},
		{"in.wav", "--measure-loudness", "--format", "flac"},
		{"in.wav", "--measure-loudness", "--bitrate", "128000"},
		{"in.wav", "--measure-loudness", "--out", "out.flac"},
		{"in.wav", "--measure-loudness", "--dir", "out"},
		{"in.wav", "--measure-loudness", "--collision", "overwrite"},
		{"in.wav", "--measure-loudness", "--channels", "mono"},
		{"in.wav", "--measure-loudness", "--downmix"},
		{"in.wav", "out.flac", "--measure-loudness"}, // positional output
	} {
		assertNormalizeUsageError(t, args)
	}
}

// TestNormalizeWriteByDefaultNeedsOutput checks every supported input shape.
func TestNormalizeWriteByDefaultNeedsOutput(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.wav")
	f2 := filepath.Join(dir, "b.wav")
	for _, p := range []string{f1, f2} {
		if err := os.WriteFile(p, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	emptyDir := t.TempDir()

	cases := []struct {
		name string
		args []string
	}{
		// An existing file with no output/--format: the "needs output" hint fires.
		// A missing file is intercepted earlier by source validation and reported as
		// "no such file", covered by TestProcessSourceCheckedBeforeCollision.
		{"file", []string{f1}},
		{"album", []string{"--album", f1, f2}},
		{"directory", []string{emptyDir}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newNormalizeCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Fatalf("normalize %v: err = %v (%T), want *usageError", tc.args, err, err)
			}
			if !strings.Contains(err.Error(), "--measure-loudness") {
				t.Errorf("normalize %v: message = %q, want it to name --measure-loudness", tc.args, err)
			}
		})
	}
}

// TestNormalizeAlbumEmptyFlagsKeepAlbumMessage verifies that an explicitly empty
// --dir/--out in album mode keeps album's specific requirement errors rather than
// the generic empty-path hint. --album requires --dir and rejects --out, so "omit
// it to use the default location" would be misleading.
func TestNormalizeAlbumEmptyFlagsKeepAlbumMessage(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.wav")
	f2 := filepath.Join(dir, "b.wav")
	for _, p := range []string{f1, f2} {
		if err := os.WriteFile(p, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty --dir", []string{"--album", "--format", "flac", "--dir", "", f1, f2}, "pass --dir"},
		{"empty --out", []string{"--album", "--format", "flac", "--out", "", f1, f2}, "not used with --album"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newNormalizeCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			err := cmd.Execute()
			if _, ok := errors.AsType[*usageError](err); !ok {
				t.Fatalf("normalize %v: err = %v (%T), want *usageError", tc.args, err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("normalize %v: message = %q, want album-specific %q", tc.args, err, tc.want)
			}
			if strings.Contains(err.Error(), "omit it to use the default location") {
				t.Errorf("normalize %v: message = %q, leaked the generic empty-path hint", tc.args, err)
			}
		})
	}
}

// TestValidateNormalizeModeFlags checks flags whose validity depends on mode.
func TestValidateNormalizeModeFlags(t *testing.T) {
	writeFlags := []struct{ name, val string }{
		{"loudness-target", "-16"}, {"format", "flac"}, {"bitrate", "128000"},
		{"out", "o.flac"}, {"dir", "d"}, {"collision", "overwrite"},
		{"channels", "mono"}, {"downmix", "true"},
	}
	for _, wf := range writeFlags {
		write := newNormalizeCmd()
		mustSet(t, write, wf.name, wf.val)
		if err := validateNormalizeModeFlags(write, false); err != nil {
			t.Errorf("write mode rejected --%s: %v", wf.name, err)
		}
		measure := newNormalizeCmd()
		mustSet(t, measure, wf.name, wf.val)
		if err := validateNormalizeModeFlags(measure, true); err == nil {
			t.Errorf("--measure-loudness accepted --%s, want a usage error", wf.name)
		}
	}
}

// TestValidateNormalizeInputFlags checks flags whose validity depends on input
// shape.
func TestValidateNormalizeInputFlags(t *testing.T) {
	cases := []struct {
		name              string
		measure, dir, alb bool
		flag, val         string
		wantErr           bool
	}{
		// Single-file writes reject the directory-batch flags, including --dir.
		{"single write recursive", false, false, false, "recursive", "true", true},
		{"single write concurrency", false, false, false, "concurrency", "2", true},
		{"single write dir", false, false, false, "dir", "out", true},
		// Single-file measurement rejects recursive/concurrency (--dir is already
		// rejected by the mode check).
		{"single measure recursive", true, false, false, "recursive", "true", true},
		{"single measure concurrency", true, false, false, "concurrency", "2", true},
		// Directory writes accept batch flags but reject URL-only flags.
		{"dir write recursive ok", false, true, false, "recursive", "true", false},
		{"dir write concurrency ok", false, true, false, "concurrency", "2", false},
		{"dir write itag", false, true, false, "itag", "140", true},
		{"dir write codec", false, true, false, "codec", "opus", true},
		// Albums reject URL flags and per-file/channel flags.
		{"album itag", false, false, true, "itag", "140", true},
		{"album recursive", false, false, true, "recursive", "true", true},
		{"album channels", false, false, true, "channels", "mono", true},
		{"album out", false, false, true, "out", "o.flac", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newNormalizeCmd()
			mustSet(t, cmd, tc.flag, tc.val)
			err := validateNormalizeInputFlags(cmd, tc.measure, tc.dir, tc.alb)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateNormalizeInputFlags err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func assertNormalizeUsageError(t *testing.T, args []string) {
	t.Helper()
	cmd := newNormalizeCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("normalize %v: err = %v (%T), want *usageError (exit 2)", args, err, err)
	}
}
