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

func TestCoalesceIntPrecedence(t *testing.T) {
	file, env, flag := 10, 20, 30
	if got := coalesceInt(0); got != 0 {
		t.Errorf("no layers = %d, want 0", got)
	}
	if got := coalesceInt(0, &file); got != 10 {
		t.Errorf("file layer = %d", got)
	}
	if got := coalesceInt(0, &file, &env); got != 20 {
		t.Errorf("env over file = %d", got)
	}
	if got := coalesceInt(0, &file, &env, &flag); got != 30 {
		t.Errorf("flag over env = %d", got)
	}
	// An explicit 0 still overrides environment and file values, selecting the
	// built-in default. waxtap.New validates the resolved value.
	zero := 0
	if got := coalesceInt(0, &file, &env, &zero); got != 0 {
		t.Errorf("explicit flag 0 = %d, want 0 (overrides lower layers)", got)
	}
}

func TestEnvOverlayChromeMajor(t *testing.T) {
	t.Setenv("WAXTAP_CHROME_MAJOR", "151")
	ec, err := envOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if ec.ChromeMajor == nil || *ec.ChromeMajor != 151 {
		t.Errorf("chrome-major overlay = %v, want 151", ec.ChromeMajor)
	}
}

func TestEnvOverlayChromeMajorMalformed(t *testing.T) {
	t.Setenv("WAXTAP_CHROME_MAJOR", "abc")
	if _, err := envOverlay(); err == nil {
		t.Error("malformed WAXTAP_CHROME_MAJOR should error")
	}
}

func TestFlagIntPtrOnlyWhenChanged(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	var v int
	fs.IntVar(&v, "chrome-major", 0, "")
	if flagIntPtr(fs, "chrome-major", v) != nil {
		t.Error("unset flag should yield nil pointer")
	}
	if err := fs.Set("chrome-major", "151"); err != nil {
		t.Fatal(err)
	}
	if p := flagIntPtr(fs, "chrome-major", v); p == nil || *p != 151 {
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
