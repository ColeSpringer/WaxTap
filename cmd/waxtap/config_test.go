package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"math"
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
	_, _, err := a.externalSession(sidecarProviders{})
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

// TestHTTPClientBuiltForEnvProxy covers the F7 env-proxy gap: without --proxy the
// CLI used to hand the facade its default transport, which has no CONNECT hook, so
// an HTTPS_PROXY answering 407 stayed unclassified at exit 1.
func TestHTTPClientBuiltForEnvProxy(t *testing.T) {
	for _, k := range proxyEnvVars {
		t.Setenv(k, "")
	}
	a := &appConfig{}
	c, err := a.httpClient()
	if err != nil {
		t.Fatalf("httpClient: %v", err)
	}
	if c != nil {
		t.Fatal("no proxy settings should yield a nil client (the facade installs its own)")
	}

	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	c, err = a.httpClient()
	if err != nil {
		t.Fatalf("httpClient with HTTPS_PROXY: %v", err)
	}
	if c == nil {
		t.Fatal("an env proxy should build a transport so the CONNECT hook is installed")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.OnProxyConnectResponse == nil {
		t.Error("OnProxyConnectResponse not installed")
	}
	// The hook must classify a non-200 and pass a 200 through untouched.
	if err := tr.OnProxyConnectResponse(context.Background(), nil, nil,
		&http.Response{StatusCode: http.StatusProxyAuthRequired}); err == nil {
		t.Error("407 CONNECT response should produce an error")
	} else if pse, ok := errors.AsType[*proxyStatusError](err); !ok || pse.status != http.StatusProxyAuthRequired {
		t.Errorf("hook err = %v (%T), want *proxyStatusError{407}", err, err)
	}
	if err := tr.OnProxyConnectResponse(context.Background(), nil, nil,
		&http.Response{StatusCode: http.StatusOK}); err != nil {
		t.Errorf("200 CONNECT response err = %v, want nil", err)
	}
}

// newConfigTestCmd exposes the flag that readConfigFile reads by name.
func newConfigTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("config", "", "")
	return cmd
}

// A --proxy Go cannot use is a settings error the run should refuse at parse,
// not one every request rediscovers. The scheme set is wider than the sidecar's
// http/https rule because proxies legitimately speak SOCKS.
func TestValidateProxyURL(t *testing.T) {
	for _, ok := range []string{
		"http://127.0.0.1:8080",
		"https://proxy.example:3128",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
		"http://user:pass@proxy.example:8080",
		"http://proxy.example:65535",
		// No port at all is the common form: the transport applies the scheme's
		// default, so there is nothing to range-check.
		"http://proxy.example",
	} {
		if _, err := validateProxyURL(ok); err != nil {
			t.Errorf("validateProxyURL(%q): %v", ok, err)
		}
	}
	for _, tc := range []struct {
		in   string
		want []string // substrings the message must carry
	}{
		{"ftp://p:1", []string{"ftp", "socks5"}},
		// url.Parse reads this as scheme "127.0.0.1", so the message has to show
		// the form rather than only naming the scheme it found.
		{"127.0.0.1:9", []string{"http://host:port"}},
		{"http://", []string{"missing host"}},
		{"://nope", []string{"invalid --proxy"}},
		// An out-of-range port used to be accepted here and rediscovered by every
		// request as a proxyconnect failure, which reports a typo as a network
		// problem (exit 9).
		{"http://proxy.example:0", []string{"port", "1-65535"}},
		{"http://proxy.example:65536", []string{"port", "1-65535"}},
		{"http://proxy.example:99999", []string{"port", "1-65535"}},
		// TestValidateProxyURLRedactsUserinfo covers the older rejections; the port
		// branch echoes the same redacted value.
		{"http://user:pass@proxy.example:99999", []string{"xxxxx", "1-65535"}},
		// url.Parse rejects a non-numeric port itself, so that form never reaches
		// the range check and carries the parse error's wording instead.
		{"http://proxy.example:http", []string{"invalid --proxy", "http://host:port"}},
	} {
		_, err := validateProxyURL(tc.in)
		if err == nil {
			t.Errorf("validateProxyURL(%q) = nil error, want a usage error", tc.in)
			continue
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("validateProxyURL(%q) = %q, want it to contain %q", tc.in, err, w)
			}
		}
	}
	// An empty proxy means "no proxy", not a bad one.
	if _, err := validateProxyURL(""); err != nil {
		t.Errorf("validateProxyURL(\"\"): %v", err)
	}
}

// negativeNumericKeys pairs each unvalidated numeric setting's JSON key with its
// environment variable, so both layers are covered by one list. procs is absent
// on purpose: a negative value there is documented API ("disables the limit",
// Options.Concurrency.Procs), not a typo.
var negativeNumericKeys = []struct{ jsonKey, envVar string }{
	{"chunkParallelism", "WAXTAP_CHUNKS"},
	{"downloadConcurrency", "WAXTAP_DOWNLOAD_CONCURRENCY"},
	{"cooldownSeconds", "WAXTAP_COOLDOWN"},
	{"extractionTimeoutSeconds", "WAXTAP_EXTRACTION_TIMEOUT"},
	{"resolveTimeoutSeconds", "WAXTAP_RESOLVE_TIMEOUT"},
	{"webContextTimeoutSeconds", "WAXTAP_WEB_CONTEXT_TIMEOUT"},
	{"sidecarTimeoutSeconds", "WAXTAP_SIDECAR_TIMEOUT"},
	{"sponsorBlockTimeoutSeconds", "WAXTAP_SPONSORBLOCK_TIMEOUT"},
	{"chunkTimeoutSeconds", "WAXTAP_CHUNK_TIMEOUT"},
}

// A negative procs is the documented way to disable the concurrency limit, and
// config/env is its only route (there is no --procs flag), so validation must
// let it through.
func TestConfigAllowsNegativeProcs(t *testing.T) {
	if err := readConfigJSON(t, `{"procs":-1}`); err != nil {
		t.Errorf("readConfigFile(procs:-1) = %v, want nil (negative disables the limit)", err)
	}
	t.Setenv("WAXTAP_PROCS", "-1")
	if _, err := envOverlay(); err != nil {
		t.Errorf("envOverlay(WAXTAP_PROCS=-1) = %v, want nil (negative disables the limit)", err)
	}
}

// readConfigJSON runs readConfigFile over a one-key config file.
func readConfigJSON(t *testing.T, body string) error {
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

// A negative count or timeout is silently discarded by the consumers, which
// then use their built-in defaults. The run looks configured and behaves as if
// it were not, so the value is refused where it is read.
func TestReadConfigFileRejectsNegativeNumbers(t *testing.T) {
	for _, k := range negativeNumericKeys {
		t.Run(k.jsonKey, func(t *testing.T) {
			err := readConfigJSON(t, `{"`+k.jsonKey+`":-1}`)
			if err == nil || !isUsageError(err) {
				t.Fatalf("readConfigFile(-1) = %v, want a usage error", err)
			}
			if !strings.Contains(err.Error(), k.jsonKey) {
				t.Errorf("err = %q, want the JSON key named", err)
			}
			if strings.Contains(err.Error(), k.envVar) {
				t.Errorf("err = %q, want the file layer's vocabulary, not the env var", err)
			}
			// Zero stays legal: it is how a caller asks for the default.
			for _, ok := range []string{"0", "2"} {
				if err := readConfigJSON(t, `{"`+k.jsonKey+`":`+ok+`}`); err != nil {
					t.Errorf("readConfigFile(%s) = %v, want nil", ok, err)
				}
			}
		})
	}
}

func TestEnvOverlayRejectsNegativeNumbers(t *testing.T) {
	for _, k := range negativeNumericKeys {
		t.Run(k.envVar, func(t *testing.T) {
			t.Setenv(k.envVar, "-1")
			_, err := envOverlay()
			if err == nil || !isUsageError(err) {
				t.Fatalf("envOverlay(%s=-1) = %v, want a usage error", k.envVar, err)
			}
			if !strings.Contains(err.Error(), k.envVar) {
				t.Errorf("err = %q, want the environment variable named", err)
			}
			if strings.Contains(err.Error(), k.jsonKey) {
				t.Errorf("err = %q, want the env layer's vocabulary, not the JSON key", err)
			}
			t.Setenv(k.envVar, "0")
			if _, err := envOverlay(); err != nil {
				t.Errorf("envOverlay(%s=0) = %v, want nil", k.envVar, err)
			}
		})
	}

	// Timeouts are floats, so the non-finite shapes are reachable too. Cooldown
	// especially: +Inf seconds converts to a huge positive Duration that
	// waxtap.New's negative check would silently accept.
	t.Run("non-finite", func(t *testing.T) {
		for _, key := range []string{"WAXTAP_RESOLVE_TIMEOUT", "WAXTAP_COOLDOWN"} {
			for _, v := range []string{"NaN", "Inf", "-Inf"} {
				t.Setenv(key, v)
				if _, err := envOverlay(); err == nil {
					t.Errorf("envOverlay(%s=%s) = nil, want a usage error", key, v)
				}
			}
			t.Setenv(key, "0")
		}
	})

	// The already-validated settings keep failing in waxtap.New, not here.
	t.Run("qps still guarded downstream", func(t *testing.T) {
		t.Setenv("WAXTAP_QPS", "-5")
		if _, err := envOverlay(); err != nil {
			t.Fatalf("envOverlay(WAXTAP_QPS=-5) = %v, want it left to waxtap.New", err)
		}
		if _, err := waxtap.New(waxtap.Options{Politeness: waxtap.Politeness{PerHostQPS: -5}}); err == nil {
			t.Error("waxtap.New(PerHostQPS: -5) = nil error, want the downstream guard")
		}
	})
}

// A proxy URL with credentials can be rejected for an unrelated reason (a bad
// scheme, a missing host); the rejection must not print the password back into
// stderr or a --json document.
func TestValidateProxyURLRedactsUserinfo(t *testing.T) {
	for _, in := range []string{
		"ftp://alice:hunter2@proxy.example:3128",
		"http://alice:hunter2@",
	} {
		_, err := validateProxyURL(in)
		if err == nil {
			t.Fatalf("validateProxyURL(%q) = nil error, want a usage error", in)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("validateProxyURL(%q) leaks the password: %q", in, err)
		}
		if !strings.Contains(err.Error(), "alice") {
			t.Errorf("validateProxyURL(%q) = %q, want the username kept so the value stays recognizable", in, err)
		}
	}
	// A value that fails url.Parse outright cannot be redacted through the URL
	// type; the credential shape is stripped textually.
	_, err := validateProxyURL("http://alice:hunter2@[bad")
	if err == nil {
		t.Fatal("want a usage error for the unparseable proxy")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("parse-failure message leaks the password: %q", err)
	}
}

// A config timeout is a float64 of seconds, and the naive multiply into a
// Duration wraps negative past ~292 years: a 1e15-second timeout silently
// became sub-zero. Config has no per-key error seam, so the conversion
// saturates instead; the flag-side grammar rejects, but a config file is not an
// interactive surface.
func TestClampedSecondsSaturates(t *testing.T) {
	for _, tc := range []struct {
		sec  float64
		want time.Duration
	}{
		{30, 30 * time.Second},
		{0.5, 500 * time.Millisecond},
		{1e15, math.MaxInt64},
		{-1e15, math.MinInt64},
		{0, 0},
	} {
		if got := clampedSeconds(tc.sec); got != tc.want {
			t.Errorf("clampedSeconds(%g) = %d, want %d", tc.sec, got, tc.want)
		}
	}
	if d := clampedSeconds(1e15); d < 0 {
		t.Error("a huge timeout wrapped negative")
	}
}

// The defaults are constants, so pin them directly; the env layer is exercised
// through envOverlay and coalesceDuration, the two functions resolution uses.
func TestTimeoutDefaults(t *testing.T) {
	if defaultWebContextTimeout != 60*time.Second {
		t.Errorf("defaultWebContextTimeout = %v, want 60s: WaxSeal's first context after a relaunch costs up to 30s plus a 12s separation window, and one honoured Retry-After must fit", defaultWebContextTimeout)
	}
	if defaultSidecarTimeout != 60*time.Second {
		t.Errorf("defaultSidecarTimeout = %v, want 60s", defaultSidecarTimeout)
	}
	t.Setenv("WAXTAP_SIDECAR_TIMEOUT", "90")
	ec, err := envOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if got := coalesceDuration(defaultSidecarTimeout, nil, ec.SidecarTimeoutSec); got != 90*time.Second {
		t.Errorf("WAXTAP_SIDECAR_TIMEOUT=90 resolves to %v, want 90s", got)
	}
	if got := coalesceDuration(defaultSidecarTimeout, nil, nil); got != 60*time.Second {
		t.Errorf("unset resolves to %v, want the default", got)
	}
}

// TestSidecarProvidersPing pins which daemon doctor asks for its health: the
// one behind the most WaxSeal-specific URL configured (session, else
// player-context, else PO-token), once, with the run's key and timeout.
func TestSidecarProvidersPing(t *testing.T) {
	var got struct {
		path, key string
		strict    bool
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.key, got.strict = r.URL.Path, r.Header.Get("X-API-Key"), r.URL.Query().Get("strict") == "true"
		_, _ = io.WriteString(w, `{"ok":true,"probe":"tenant","reason":"ok"}`)
	}))
	defer srv.Close()

	cases := []struct {
		name     string
		cfg      appConfig
		wantPath string
		wantVia  string
	}{
		{"session outranks the rest", appConfig{sessionURL: srv.URL + "/a/session", playerContextURL: srv.URL + "/b/player-context", potokenURL: srv.URL + "/c/get_pot"}, "/a/ping", "session"},
		{"player-context outranks the token", appConfig{playerContextURL: srv.URL + "/b/player-context", potokenURL: srv.URL + "/c/get_pot"}, "/b/ping", "player-context"},
		{"token alone", appConfig{potokenURL: srv.URL + "/c/get_pot"}, "/c/ping", "po-token"},
		{"base URL", appConfig{potokenURL: srv.URL}, "/ping", "po-token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.cfg
			a.apiKey, a.sidecarTimeout = "K", time.Second
			sc, err := a.sidecarProviders()
			if err != nil {
				t.Fatal(err)
			}
			if sc.ping == nil {
				t.Fatal("a configured sidecar must give doctor a ping")
			}
			if sc.pingVia != tc.wantVia {
				t.Errorf("pingVia = %q, want %q, the sidecar whose URL the ping was derived from", sc.pingVia, tc.wantVia)
			}
			got.path = ""
			h, err := sc.ping(context.Background())
			if err != nil || !h.OK {
				t.Fatalf("ping = %+v, %v", h, err)
			}
			if got.path != tc.wantPath || got.key != "K" || !got.strict {
				t.Errorf("pinged %q (key %q, strict %v), want %q with the run's key under strict", got.path, got.key, got.strict, tc.wantPath)
			}
		})
	}

	// The run's sidecar timeout bounds the ping only when it was set: left
	// alone, the ping keeps the library's own allowance for a daemon that is
	// tearing down and relaunching its browser, which the 60 s default would
	// cut short.
	t.Run("timeout applies only when set", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(150 * time.Millisecond)
			_, _ = io.WriteString(w, `{"ok":true,"probe":"tenant","reason":"ok"}`)
		}))
		defer slow.Close()
		unset := appConfig{potokenURL: slow.URL, sidecarTimeout: 20 * time.Millisecond}
		sc, err := unset.sidecarProviders()
		if err != nil {
			t.Fatal(err)
		}
		if h, err := sc.ping(context.Background()); err != nil || !h.OK {
			t.Errorf("ping = %+v, %v; want the answer: an unset sidecar timeout leaves the ping its own bound", h, err)
		}
		set := appConfig{potokenURL: slow.URL, sidecarTimeout: 20 * time.Millisecond, sidecarTimeoutSet: true}
		sc, err = set.sidecarProviders()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sc.ping(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the set 20 ms sidecar timeout to cut the ping", err)
		}
	})

	none := appConfig{}
	sc, err := none.sidecarProviders()
	if err != nil {
		t.Fatal(err)
	}
	if sc.ping != nil {
		t.Error("no sidecar configured, yet a ping was built")
	}
}

// The ping keeps its own default bound only while sidecarTimeoutSeconds is
// left alone; set through either layer, the value applies to it too.
func TestLoadConfigRecordsSidecarTimeoutSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXTAP_CONFIG", path)
	newCmd := func() *cobra.Command {
		cmd := &cobra.Command{Use: "test"}
		bindConfigFlags(cmd.Flags())
		bindNetworkFlags(cmd.Flags())
		bindPlayerExtractionFlags(cmd.Flags())
		return cmd
	}

	cfg, err := loadConfig(newCmd())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.sidecarTimeoutSet || cfg.sidecarTimeout != defaultSidecarTimeout {
		t.Errorf("unset: set = %v, timeout = %v; want the default, recorded as not set", cfg.sidecarTimeoutSet, cfg.sidecarTimeout)
	}

	if err := os.WriteFile(path, []byte(`{"sidecarTimeoutSeconds": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(newCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.sidecarTimeoutSet || cfg.sidecarTimeout != 7*time.Second {
		t.Errorf("file: set = %v, timeout = %v; want 7s, recorded as set", cfg.sidecarTimeoutSet, cfg.sidecarTimeout)
	}

	t.Setenv("WAXTAP_SIDECAR_TIMEOUT", "5")
	cfg, err = loadConfig(newCmd())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.sidecarTimeoutSet || cfg.sidecarTimeout != 5*time.Second {
		t.Errorf("env: set = %v, timeout = %v; want 5s, recorded as set", cfg.sidecarTimeoutSet, cfg.sidecarTimeout)
	}
}
