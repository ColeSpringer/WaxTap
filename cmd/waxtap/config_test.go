package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestCoalescePrecedence(t *testing.T) {
	file, env, flag := "file", "env", "flag"
	if got := coalesceString("def"); got != "def" {
		t.Errorf("no layers = %q, want def", got)
	}
	if got := coalesceString("def", &file); got != "file" {
		t.Errorf("file layer = %q", got)
	}
	if got := coalesceString("def", &file, &env); got != "env" {
		t.Errorf("env over file = %q", got)
	}
	if got := coalesceString("def", &file, &env, &flag); got != "flag" {
		t.Errorf("flag over env = %q", got)
	}
	// A nil higher-priority layer does not clobber a lower one.
	if got := coalesceString("def", &file, nil, nil); got != "file" {
		t.Errorf("nil layers should keep file = %q", got)
	}
}

func TestCoalesceDuration(t *testing.T) {
	def := 5 * time.Second
	if got := coalesceDuration(def); got != def {
		t.Errorf("default = %v", got)
	}
	secs := 2.5
	if got := coalesceDuration(def, &secs); got != 2500*time.Millisecond {
		t.Errorf("seconds layer = %v", got)
	}
}

func TestEnvOverlay(t *testing.T) {
	t.Setenv("WAXTAP_QPS", "1.5")
	t.Setenv("WAXTAP_HL", "de")
	t.Setenv("WAXTAP_NO_CACHE", "true")
	ec, err := envOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if ec.PerHostQPS == nil || *ec.PerHostQPS != 1.5 {
		t.Errorf("qps overlay = %v", ec.PerHostQPS)
	}
	if ec.HL == nil || *ec.HL != "de" {
		t.Errorf("hl overlay = %v", ec.HL)
	}
	if ec.NoCache == nil || !*ec.NoCache {
		t.Errorf("no-cache overlay = %v", ec.NoCache)
	}
}

func TestEnvOverlayMalformed(t *testing.T) {
	t.Setenv("WAXTAP_QPS", "not-a-number")
	if _, err := envOverlay(); err == nil {
		t.Error("malformed WAXTAP_QPS should error")
	}
}

func TestReadConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"hl":"ja","perHostQPS":2}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newConfigTestCmd()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatal(err)
	}
	fc, err := readConfigFile(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if fc.HL == nil || *fc.HL != "ja" {
		t.Errorf("file hl = %v", fc.HL)
	}
	if fc.PerHostQPS == nil || *fc.PerHostQPS != 2 {
		t.Errorf("file qps = %v", fc.PerHostQPS)
	}
}

func TestReadConfigFileMissingExplicitErrors(t *testing.T) {
	cmd := newConfigTestCmd()
	if err := cmd.Flags().Set("config", filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFile(cmd); err == nil {
		t.Error("explicitly named missing config should error")
	}
}

func TestFlagPtrOnlyWhenChanged(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	var v string
	fs.StringVar(&v, "foo", "def", "")
	if flagPtr(fs, "foo", v) != nil {
		t.Error("unset flag should yield nil pointer")
	}
	if err := fs.Set("foo", "bar"); err != nil {
		t.Fatal(err)
	}
	if p := flagPtr(fs, "foo", v); p == nil || *p != "bar" {
		t.Errorf("set flag pointer = %v", p)
	}
}

// newConfigTestCmd builds a command exposing just the --config flag bound to the
// global rootFlagsValue, as readConfigFile expects.
func newConfigTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringVar(&rootFlagsValue.config, "config", "", "")
	return cmd
}
