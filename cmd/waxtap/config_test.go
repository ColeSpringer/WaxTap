package main

import (
	"os"
	"path/filepath"
	"strings"
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

func TestReadConfigFileRejectsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"qps":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newConfigTestCmd()
	if err := cmd.Flags().Set("config", path); err != nil {
		t.Fatal(err)
	}
	_, err := readConfigFile(cmd)
	if err == nil || !isUsageError(err) {
		t.Fatalf("readConfigFile err = %v, want a usage error for the misspelled key", err)
	}
	if !strings.Contains(err.Error(), "qps") {
		t.Errorf("err = %q, want it to name the unknown field", err)
	}
}

// TestReadConfigFileTypeMismatchMessage checks that top-level shape errors and
// field type errors are reported in config-file terms, not Go struct terms.
func TestReadConfigFileTypeMismatchMessage(t *testing.T) {
	readWithConfig := func(t *testing.T, body string) error {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := newConfigTestCmd()
		if err := cmd.Flags().Set("config", path); err != nil {
			t.Fatal(err)
		}
		_, err := readConfigFile(cmd)
		return err
	}
	t.Run("top-level array", func(t *testing.T) {
		err := readWithConfig(t, `[]`)
		if err == nil || !isUsageError(err) {
			t.Fatalf("err = %v, want a usage error for the wrong-shape config", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, "expected a JSON object, got array") {
			t.Errorf("err = %q, want the clean type-mismatch message", msg)
		}
		if strings.Contains(msg, "fileConfig") || strings.Contains(msg, "Go value") {
			t.Errorf("err = %q, leaks the internal Go type", msg)
		}
	})
	t.Run("mistyped field is not reported as a wrong top-level shape", func(t *testing.T) {
		err := readWithConfig(t, `{"hl":123}`)
		if err == nil || !isUsageError(err) {
			t.Fatalf("err = %v, want a usage error for the mistyped field", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, `field "hl" has the wrong type`) {
			t.Errorf("err = %q, want a field-level type message", msg)
		}
		if strings.Contains(msg, "expected a JSON object") {
			t.Errorf("err = %q, a mistyped field is not a wrong top-level shape", msg)
		}
		if strings.Contains(msg, "fileConfig") || strings.Contains(msg, "Go struct field") {
			t.Errorf("err = %q, leaks the internal Go type", msg)
		}
	})
}

func TestReadConfigFileRejectsTrailingData(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"clean", `{"hl":"en"}`, true},
		{"trailing newline ok", "{\"hl\":\"en\"}\n", true},
		{"concatenated objects", `{"hl":"en"}{"hl":"ja"}`, false},
		{"trailing garbage", `{"hl":"en"} oops`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := newConfigTestCmd()
			if err := cmd.Flags().Set("config", path); err != nil {
				t.Fatal(err)
			}
			_, err := readConfigFile(cmd)
			if (err == nil) != tc.ok {
				t.Fatalf("readConfigFile err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !isUsageError(err) {
				t.Errorf("err = %#v, want a usage error", err)
			}
		})
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

func TestReadConfigFileMissingEnvIsSoft(t *testing.T) {
	t.Setenv("WAXTAP_CONFIG", filepath.Join(t.TempDir(), "nonexistent.json"))
	cmd := newConfigTestCmd()
	fc, err := readConfigFile(cmd)
	if err != nil {
		t.Fatalf("missing WAXTAP_CONFIG should be soft, got err = %v", err)
	}
	if fc.HL != nil {
		t.Errorf("expected an empty fileConfig, got %+v", fc)
	}
}

func TestReadConfigFileMalformedEnvErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXTAP_CONFIG", path)
	cmd := newConfigTestCmd()
	if _, err := readConfigFile(cmd); err == nil {
		t.Error("malformed WAXTAP_CONFIG should error")
	}
}

func TestExternalSessionBadCookiesIsUsageError(t *testing.T) {
	a := &appConfig{visitorData: "VD", cookiesPath: filepath.Join(t.TempDir(), "nope.txt")}
	_, _, err := a.externalSession()
	if err == nil {
		t.Fatal("a missing --cookies file should error")
	}
	if !isUsageError(err) {
		t.Errorf("err = %#v, want a usage error", err)
	}
	if got := exitCodeFor(err); got != 2 {
		t.Errorf("exit = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), "read cookies") {
		t.Errorf("err = %q, want it to identify the cookie read failure", err)
	}
}

func TestValidateLocale(t *testing.T) {
	cases := []struct {
		name   string
		hl, gl string
		ok     bool
	}{
		{"empty unset", "", "", true},
		{"plain language and region", "en", "US", true},
		{"language with region subtag", "pt-BR", "BR", true},
		{"multi-subtag language", "zh-Hans-CN", "CN", true},
		{"invalid region", "en", "ZZ123", false},
		{"invalid language", "e!", "US", false},
		{"region too long", "en", "USA", false},
		{"only gl set", "", "US", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLocale(tc.hl, tc.gl)
			if (err == nil) != tc.ok {
				t.Fatalf("validateLocale(%q,%q) = %v, want ok=%v", tc.hl, tc.gl, err, tc.ok)
			}
			if err != nil && !isUsageError(err) {
				t.Errorf("err = %#v, want a usage error (exit 2)", err)
			}
		})
	}
}

func TestFlagPtrOnlyWhenChanged(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.String("foo", "def", "")
	if flagPtr(fs, "foo") != nil {
		t.Error("unset flag should yield nil pointer")
	}
	if err := fs.Set("foo", "bar"); err != nil {
		t.Fatal(err)
	}
	if p := flagPtr(fs, "foo"); p == nil || *p != "bar" {
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
	fs.Int("chrome-major", 0, "")
	if flagIntPtr(fs, "chrome-major") != nil {
		t.Error("unset flag should yield nil pointer")
	}
	if err := fs.Set("chrome-major", "151"); err != nil {
		t.Fatal(err)
	}
	if p := flagIntPtr(fs, "chrome-major"); p == nil || *p != 151 {
		t.Errorf("set flag pointer = %v", p)
	}
}

// newConfigTestCmd exposes the flag that readConfigFile reads by name.
func newConfigTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("config", "", "")
	return cmd
}
