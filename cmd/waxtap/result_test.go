package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v2"
)

// TestResultJSONFormatOmission verifies that sourceFormat is always present and
// non-transcoded local results omit outputFormat.
func TestResultJSONFormatOmission(t *testing.T) {
	marshal := func(res *waxtap.Result) map[string]any {
		b, err := json.Marshal(resultToJSON(res))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	has := func(m map[string]any, k string) bool { _, ok := m[k]; return ok }

	t.Run("youtube keeps both", func(t *testing.T) {
		m := marshal(&waxtap.Result{SourceKind: waxtap.SourceYouTube,
			SourceFormat: waxtap.Format{Itag: 251}, OutputFormat: waxtap.Format{Itag: 251}})
		if !has(m, "sourceFormat") || !has(m, "outputFormat") {
			t.Errorf("YouTube result should keep both format objects: %v", m)
		}
	})
	t.Run("local measure-only keeps source codec, drops output", func(t *testing.T) {
		// The local pipeline records only the source codec and extension.
		m := marshal(&waxtap.Result{SourceKind: waxtap.SourceLocalFile, LoudnessMeasured: true,
			SourceFormat: waxtap.Format{Codec: "flac", Extension: "flac"},
			OutputFormat: waxtap.Format{Codec: "flac", Extension: "flac"}})
		sf, ok := m["sourceFormat"].(map[string]any)
		if !ok {
			t.Fatalf("local sourceFormat should be present with the codec: %v", m)
		}
		if sf["codec"] != "flac" {
			t.Errorf("sourceFormat.codec = %v, want flac (must not be lost from JSON)", sf["codec"])
		}
		if sf["extension"] != "flac" {
			t.Errorf("sourceFormat.extension = %v, want flac", sf["extension"])
		}
		// Local formats expose only codec and extension. Network-only fields would be
		// zero, so they stay out of the JSON shape.
		for _, k := range []string{"itag", "sampleRate", "channels", "bitrate", "contentLength", "mimeType"} {
			if _, ok := sf[k]; ok {
				t.Errorf("local sourceFormat should omit the always-zero %q field: %v", k, sf)
			}
		}
		if has(m, "outputFormat") {
			t.Errorf("a local result that was not re-encoded should omit the redundant outputFormat: %v", m)
		}
	})
	t.Run("local transcode keeps both", func(t *testing.T) {
		m := marshal(&waxtap.Result{SourceKind: waxtap.SourceLocalFile, Transcoded: true,
			SourceFormat: waxtap.Format{Codec: "flac", Extension: "flac"},
			OutputFormat: waxtap.Format{Codec: "mp3", Extension: "mp3"}})
		if !has(m, "sourceFormat") || !has(m, "outputFormat") {
			t.Errorf("a transcoded local result should keep both format objects: %v", m)
		}
	})
}

// TestRenderResultHumanWarningDedup verifies that human output does not repeat
// warnings already printed by the progress reporter.
func TestRenderResultHumanWarningDedup(t *testing.T) {
	res := &waxtap.Result{
		SourceKind: waxtap.SourceLocalFile,
		InputPath:  "/in.flac",
		OutputPath: "/out.flac",
		Warnings:   []waxtap.Warning{{Code: waxtap.WarnFallbackProfile, Detail: "served WEB"}},
	}

	t.Run("non-quiet omits summary warnings", func(t *testing.T) {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		renderResultHuman(env, res)
		if strings.Contains(out.String(), "warning:") {
			t.Errorf("non-quiet summary should not repeat live warnings:\n%s", out.String())
		}
	})

	t.Run("quiet prints only the path; warnings to stderr", func(t *testing.T) {
		var out, errOut bytes.Buffer
		env := &appEnv{out: &out, errOut: &errOut, cfg: &appConfig{quiet: true}}
		renderResultHuman(env, res)
		if got := strings.TrimRight(out.String(), "\n"); got != "/out.flac" {
			t.Errorf("quiet stdout = %q, want exactly the output path", got)
		}
		if e := errOut.String(); !strings.Contains(e, "served WEB") || !strings.Contains(e, "fallback-profile") {
			t.Errorf("quiet warnings should go to stderr with their code:\n%s", e)
		}
	})

	t.Run("quiet measure-only prints nothing", func(t *testing.T) {
		measure := &waxtap.Result{SourceKind: waxtap.SourceLocalFile, InputPath: "/in.flac", LoudnessMeasured: true}
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{quiet: true}}
		renderResultHuman(env, measure)
		if out.Len() != 0 {
			t.Errorf("quiet measure-only (no OutputPath) should print nothing, got %q", out.String())
		}
	})
}

// TestRenderResultHumanMeasureOnly verifies that a measure-only run with no output
// path names the measurement instead of printing a phantom write, while a
// measure-only run that wrote an unaltered copy is left unchanged.
func TestRenderResultHumanMeasureOnly(t *testing.T) {
	render := func(res *waxtap.Result) string {
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}}
		renderResultHuman(env, res)
		return out.String()
	}

	t.Run("no output path names the measurement", func(t *testing.T) {
		// normalize --measure-loudness sinks to io.Discard: empty OutputPath, but
		// OutputBytes equals the input size. The old shared printer reported that as a
		// real write.
		out := render(&waxtap.Result{
			SourceKind: waxtap.SourceLocalFile, InputPath: "/in.flac", LoudnessMeasured: true,
			SourceFormat: waxtap.Format{Codec: "flac", Extension: "flac"},
			SourceBytes:  86700, OutputBytes: 86700,
		})
		if !strings.Contains(out, "Output:   none (measurement only)") {
			t.Errorf("want the measurement-only output line:\n%s", out)
		}
		if !strings.Contains(out, "analyzed") {
			t.Errorf("want a Size line marked analyzed:\n%s", out)
		}
		if strings.Contains(out, " in, ") {
			t.Errorf("must not print a phantom in/out size:\n%s", out)
		}
		if strings.Contains(out, "(streamed)") {
			t.Errorf("a measure-only local run is not a stream:\n%s", out)
		}
	})

	t.Run("download measure-only with an unaltered copy is unchanged", func(t *testing.T) {
		// download --measure-loudness writes the copy, so OutputPath is set and the
		// normal Output/Size lines still apply.
		out := render(&waxtap.Result{
			SourceKind: waxtap.SourceYouTube, VideoID: "dummyVideo0", LoudnessMeasured: true,
			OutputPath:   "/out.opus",
			SourceFormat: waxtap.Format{Itag: 251, Codec: "opus"},
			SourceBytes:  86700, OutputBytes: 86700,
		})
		if !strings.Contains(out, "Output:   /out.opus") {
			t.Errorf("a written copy should still show its path:\n%s", out)
		}
		if !strings.Contains(out, " in, ") {
			t.Errorf("a written copy should still show in/out sizes:\n%s", out)
		}
		if strings.Contains(out, "measurement only") {
			t.Errorf("a written copy is not measurement-only output:\n%s", out)
		}
	})

	t.Run("download measure-only streaming to stdout shows streamed, not measurement-only", func(t *testing.T) {
		// download --measure-loudness -o - streams the audio to stdout (a real writer
		// sink), leaving OutputPath empty like a discarded measurement. audioStream
		// distinguishes it so the summary does not claim nothing was output.
		var out bytes.Buffer
		env := &appEnv{out: &out, errOut: io.Discard, cfg: &appConfig{}, audioStream: true}
		renderResultHuman(env, &waxtap.Result{
			SourceKind: waxtap.SourceYouTube, VideoID: "dummyVideo0", LoudnessMeasured: true,
			SourceFormat: waxtap.Format{Itag: 251, Codec: "opus"},
			SourceBytes:  86700, OutputBytes: 86700,
		})
		got := out.String()
		if !strings.Contains(got, "(streamed)") {
			t.Errorf("a stdout stream should render as streamed:\n%s", got)
		}
		if !strings.Contains(got, " in, ") {
			t.Errorf("a stdout stream should still show in/out sizes:\n%s", got)
		}
		if strings.Contains(got, "measurement only") || strings.Contains(got, "analyzed") {
			t.Errorf("delivered audio must not be labeled measurement-only:\n%s", got)
		}
	})
}
