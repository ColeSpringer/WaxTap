package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func newSummaryEnv(jsonMode bool) (*syncWriter, *bytes.Buffer) {
	var buf bytes.Buffer
	env := &appEnv{out: &buf, errOut: io.Discard, cfg: &appConfig{json: jsonMode}}
	return &syncWriter{env: env}, &buf
}

func TestEmitSummaryJSONAdditiveFields(t *testing.T) {
	sw, buf := newSummaryEnv(true)
	if err := sw.emitSummary(playlistSummary{
		total: 10, ok: 3, skipped: 1, remaining: 6, capReached: true,
	}); err != nil {
		t.Fatalf("emitSummary returned an error for a cap-limited run: %v", err)
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("summary JSON: %v\n%s", err, buf.String())
	}
	// Additive fields do not change the schema version.
	if rec["schemaVersion"].(float64) != schemaVersion {
		t.Errorf("schemaVersion = %v, want %d", rec["schemaVersion"], schemaVersion)
	}
	if rec["total"].(float64) != 10 || rec["ok"].(float64) != 3 || rec["skipped"].(float64) != 1 {
		t.Errorf("summary fields = %v, want total=10 ok=3 skipped=1", rec)
	}
	if rec["remaining"].(float64) != 6 {
		t.Errorf("remaining = %v, want 6", rec["remaining"])
	}
	if rec["capReached"] != true {
		t.Errorf("capReached = %v, want true", rec["capReached"])
	}
}

func TestEmitSummaryJSONFailureSplit(t *testing.T) {
	sw, buf := newSummaryEnv(true)
	err := sw.emitSummary(playlistSummary{
		total: 5, ok: 1, resolveFailed: 1, downloadFailed: 2,
	})
	if err == nil {
		t.Error("emitSummary returned nil for failed items")
	}
	var rec map[string]any
	if jerr := json.Unmarshal(buf.Bytes(), &rec); jerr != nil {
		t.Fatalf("summary JSON: %v", jerr)
	}
	if rec["failed"].(float64) != 3 {
		t.Errorf("failed = %v, want 3 (resolve+download)", rec["failed"])
	}
	if rec["resolveFailed"].(float64) != 1 || rec["downloadFailed"].(float64) != 2 {
		t.Errorf("failure fields = %v, want resolveFailed=1 downloadFailed=2", rec)
	}
}

func TestEmitSummaryHumanCapNote(t *testing.T) {
	sw, buf := newSummaryEnv(false)
	if err := sw.emitSummary(playlistSummary{total: 10, ok: 3, remaining: 7, capReached: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out := buf.String(); !strings.Contains(out, "7 remaining (max-downloads reached)") {
		t.Errorf("summary missing cap note: %q", out)
	}
}

func TestEmitSummaryHumanRemainingNoCap(t *testing.T) {
	sw, buf := newSummaryEnv(false)
	// Cancellation reports remaining items without a max-downloads note.
	_ = sw.emitSummary(playlistSummary{total: 10, ok: 3, remaining: 7})
	out := buf.String()
	if !strings.Contains(out, "7 remaining") || strings.Contains(out, "max-downloads") {
		t.Errorf("summary = %q, want remaining count without max-downloads note", out)
	}
}

func TestDownloadFlagsResolveSleepValidation(t *testing.T) {
	cases := []struct {
		name     string
		sleep    string
		maxSleep string
		maxDl    string
		wantErr  bool
	}{
		{"no pacing", "", "", "", false},
		{"sleep only", "5s", "", "", false},
		{"valid range", "5s", "9s", "", false},
		{"max without sleep", "", "9s", "", true},
		{"max below sleep", "9s", "5s", "", true},
		{"negative sleep", "-5s", "", "", true},
		{"negative max-downloads", "", "", "-1", true},
		{"valid max-downloads", "", "", "3", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			df := &downloadFlags{}
			cmd := &cobra.Command{Use: "download"}
			bindDownloadFlags(cmd, df)
			if tt.sleep != "" {
				mustSet(t, cmd, "sleep-interval", tt.sleep)
			}
			if tt.maxSleep != "" {
				mustSet(t, cmd, "max-sleep-interval", tt.maxSleep)
			}
			if tt.maxDl != "" {
				mustSet(t, cmd, "max-downloads", tt.maxDl)
			}
			err := df.resolve(cmd, testResolveEnv())
			if (err != nil) != tt.wantErr {
				t.Errorf("resolve() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResolveRejectsNegativeMaxItems(t *testing.T) {
	df := &downloadFlags{}
	cmd := &cobra.Command{Use: "download"}
	bindDownloadFlags(cmd, df)
	mustSet(t, cmd, "max-items", "-1")
	if err := df.resolve(cmd, testResolveEnv()); err == nil {
		t.Error("--max-items -1 should be rejected (it would otherwise page the whole playlist)")
	}
}

// testResolveEnv returns an appEnv that discards informational output.
func testResolveEnv() *appEnv {
	return &appEnv{cfg: &appConfig{}, out: io.Discard, errOut: io.Discard}
}

func TestResolveConcurrency(t *testing.T) {
	t.Run("negative rejected", func(t *testing.T) {
		df := &downloadFlags{}
		cmd := &cobra.Command{Use: "download"}
		bindDownloadFlags(cmd, df)
		mustSet(t, cmd, "concurrency", "-1")
		if err := df.resolve(cmd, testResolveEnv()); err == nil {
			t.Error("--concurrency -1 should be rejected")
		}
	})
	t.Run("clamped above the ceiling", func(t *testing.T) {
		df := &downloadFlags{}
		cmd := &cobra.Command{Use: "download"}
		bindDownloadFlags(cmd, df)
		mustSet(t, cmd, "concurrency", "99999")
		if err := df.resolve(cmd, testResolveEnv()); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if df.concurrency != maxConcurrency {
			t.Errorf("concurrency = %d, want clamp to %d", df.concurrency, maxConcurrency)
		}
	})
}

func mustSet(t *testing.T, cmd *cobra.Command, name, val string) {
	t.Helper()
	if err := cmd.Flags().Set(name, val); err != nil {
		t.Fatalf("set --%s=%s: %v", name, val, err)
	}
}

func TestEnvOverlayCooldown(t *testing.T) {
	t.Setenv("WAXTAP_COOLDOWN", "8")
	ec, err := envOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if ec.CooldownSec == nil || *ec.CooldownSec != 8 {
		t.Errorf("WAXTAP_COOLDOWN overlay = %v, want 8", ec.CooldownSec)
	}
}

func TestFlagDurationPtr(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.Duration("cooldown", 0, "")
	if flagDurationPtr(fs, "cooldown") != nil {
		t.Error("unset flag should yield nil pointer")
	}
	if err := fs.Set("cooldown", "8s"); err != nil {
		t.Fatal(err)
	}
	if p := flagDurationPtr(fs, "cooldown"); p == nil || *p != 8 {
		t.Errorf("set flag pointer = %v, want 8 seconds", p)
	}
}

func TestOptionsCarriesCooldown(t *testing.T) {
	a := &appConfig{cooldown: 8 * time.Second}
	opts, err := a.options(slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if opts.Politeness.Cooldown != 8*time.Second {
		t.Errorf("Politeness.Cooldown = %v, want 8s", opts.Politeness.Cooldown)
	}
}
