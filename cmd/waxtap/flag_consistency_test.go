package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestFlagSurfaceConsistency(t *testing.T) {
	download := newDownloadCmd().Flags()
	if download.Lookup("best-audio") != nil {
		t.Error("download --best-audio should not be exposed; best audio is already the default")
	}
	if download.Lookup("measure-loudness") == nil || download.Lookup("measure") != nil {
		t.Error("download should expose only --measure-loudness")
	}
	if download.Lookup("sponsorblock-on-error") == nil || download.Lookup("sponsorblock-onerror") != nil {
		t.Error("download should expose only --sponsorblock-on-error")
	}

	cut := newCutCmd().Flags()
	if cut.Lookup("sponsorblock-on-error") == nil || cut.Lookup("sponsorblock-onerror") != nil {
		t.Error("cut should expose only --sponsorblock-on-error")
	}

	if got := newTranscodeCmd().Flags().ShorthandLookup("d"); got == nil || got.Name != "dir" {
		t.Error("transcode -d should select --dir")
	}

	// The output-format flag is --format/-f on every re-encoding command; the old
	// --transcode name was removed with no alias.
	for _, c := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"download", newDownloadCmd()},
		{"cut", newCutCmd()},
		{"transcode", newTranscodeCmd()},
		{"normalize", newNormalizeCmd()},
	} {
		f := c.cmd.Flags()
		if f.Lookup("format") == nil {
			t.Errorf("%s should expose --format", c.name)
		}
		if f.Lookup("transcode") != nil {
			t.Errorf("%s should not expose the removed --transcode", c.name)
		}
		if sh := f.ShorthandLookup("f"); sh == nil || sh.Name != "format" {
			t.Errorf("%s -f should select --format", c.name)
		}
	}

	// --skip-existing was removed in favor of --collision skip.
	if newDownloadCmd().Flags().Lookup("skip-existing") != nil {
		t.Error("download --skip-existing should be removed; use --collision skip")
	}

	// The read paths gained --no-fallback to disable the watch-page fallback.
	if newInfoCmd().Flags().Lookup("no-fallback") == nil {
		t.Error("info should expose --no-fallback")
	}
	if newFormatsCmd().Flags().Lookup("no-fallback") == nil {
		t.Error("formats should expose --no-fallback")
	}
}

func TestDownloadRejectsIgnoredFlagCombinations(t *testing.T) {
	cases := []map[string]string{
		{"bitrate": "128000"},
		{"loudness-target": "-16"},
		{"measure-loudness": "true", "loudness-target": "-16"},
		{"out": "track.webm", "output-template": "{id}.{ext}"},
		{"sponsorblock-on-error": "fail"},
		{"crossfade": "1s"},
		{"list": "true", "format": "flac"},
	}
	for _, values := range cases {
		df := &downloadFlags{}
		check := newFlagTestDownloadCmd(df)
		for name, value := range values {
			if err := check.Flags().Set(name, value); err != nil {
				t.Fatalf("set --%s=%s: %v", name, value, err)
			}
		}
		if err := df.resolve(check, testResolveEnv()); err == nil {
			t.Errorf("download flags %v should be rejected", values)
		}
	}
}

func newFlagTestDownloadCmd(df *downloadFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "download"}
	bindDownloadFlags(cmd, df)
	return cmd
}

func TestProcessCommandsRejectIgnoredFlags(t *testing.T) {
	local := filepath.Join(t.TempDir(), "in.wav")
	if err := os.WriteFile(local, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cases := []struct {
		cmd  *cobra.Command
		args []string
	}{
		{newCutCmd(), []string{"in.wav", "--cut-range", "0-1", "--bitrate", "128000"}},
		{newTranscodeCmd(), []string{"in.wav", "out.mp3", "--dir", "out"}},
		{newTranscodeCmd(), []string{"in.wav", "out.mp3", "--recursive"}},
		{newTranscodeCmd(), []string{"in.wav", "out.mp3", "--force"}},
		{newTranscodeCmd(), []string{"in.wav", "out.mp3", "--concurrency", "2"}},
		{newCutCmd(), []string{local, "--cut-range", "0-1", "--itag", "140"}},
		{newTranscodeCmd(), []string{local, "out.mp3", "--codec", "opus"}},
		{newNormalizeCmd(), []string{local, "--no-fallback"}},
		{newTranscodeCmd(), []string{dir, "--format", "mp3", "--source-policy", "best-native"}},
	}
	for _, tc := range cases {
		tc.cmd.SetArgs(tc.args)
		tc.cmd.SetOut(&bytes.Buffer{})
		tc.cmd.SetErr(&bytes.Buffer{})
		err := tc.cmd.Execute()
		if _, ok := errors.AsType[*usageError](err); !ok {
			t.Errorf("%s %v: err = %v (%T), want *usageError", tc.cmd.Name(), tc.args, err, err)
		}
	}
}

func TestSingleDownloadRejectsPlaylistOnlyFlags(t *testing.T) {
	cmd := newDownloadCmd()
	cmd.SetArgs([]string{"dummyVideo0", "--concurrency", "2"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if _, ok := errors.AsType[*usageError](err); !ok {
		t.Errorf("single download --concurrency: err = %v (%T), want *usageError", err, err)
	}
}
