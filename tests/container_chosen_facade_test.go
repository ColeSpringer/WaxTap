package tests

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// The container rule against a real delivery: the format an output extension
// implies is a fallback, settled once the download is staged and probed. An
// Opus-in-WebM delivery into .mka is a copy, because Matroska carries Opus,
// and into .m4a it is the encode the container forces, reported as
// implicit-lossy.
func TestFacade_ContainerChosenKeepsTheDeliveredCodec(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	wav := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	webm := filepath.Join(dir, "src.webm")
	if _, err := media.NewRunner(media.RunnerConfig{}).Transcode(ctx, wav, webm, media.Spec{Codec: media.CodecOpus}); err != nil {
		t.Fatalf("synth webm: %v", err)
	}
	body, err := os.ReadFile(webm)
	if err != nil {
		t.Fatal(err)
	}
	umpBody := fSabrHappyBody(body[:len(body)/2], body[len(body)/2:])

	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "1" {
				return resp(http.StatusOK, []byte(sabrPlayerJSON)), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			return resp(http.StatusOK, umpBody), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, POTokenProvider: fProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	download := func(out string, ts waxtap.TranscodeSpec) *waxtap.Result {
		t.Helper()
		res, derr := c.Download(ctx, waxtap.Request{
			URL: "dummyVideo0",
			ProcessSpec: waxtap.ProcessSpec{
				Output:    waxtap.ToFile(filepath.Join(dir, out)),
				Transcode: &ts,
			},
		})
		if derr != nil {
			t.Fatalf("download to %s: %v", out, derr)
		}
		return res
	}
	warned := func(ws []waxtap.Warning) bool {
		for _, w := range ws {
			if w.Code == waxtap.WarnImplicitLossy {
				return true
			}
		}
		return false
	}

	// Matroska's usual encoder is Opus, and the delivery is Opus: nothing to
	// re-encode, so the packets move into the .mka the caller named.
	kept := download("out.mka", waxtap.TranscodeSpec{Format: waxtap.FormatOpus, FromContainer: true})
	if kept.Transcoded {
		t.Errorf("Transcoded = true, want a copy: Matroska carries Opus")
	}
	if warned(kept.Warnings) {
		t.Errorf("warnings = %v, want no implicit-lossy on a copy", kept.Warnings)
	}
	if kept.OutputFormat.Extension != "mka" || kept.OutputFormat.MIMEType != "audio/x-matroska" {
		t.Errorf("OutputFormat = %+v, want the .mka the packets landed in", kept.OutputFormat)
	}
	pr, err := media.NewRunner(media.RunnerConfig{}).Probe(ctx, filepath.Join(dir, "out.mka"))
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := pr.AudioStream(); pr.Format.Container != "mka" || a.CodecName != "opus" {
		t.Errorf("delivered %s/%s, want mka/opus", pr.Format.Container, a.CodecName)
	}

	// Flat MP4 cannot carry Opus, so the container's own encoder runs and the
	// generation nobody asked for is reported.
	encoded := download("out.m4a", waxtap.TranscodeSpec{Format: waxtap.FormatAAC, FromContainer: true})
	if !encoded.Transcoded {
		t.Errorf("Transcoded = false, want the encode .m4a forces")
	}
	if !strings.Contains(strings.ToLower(encoded.OutputFormat.Codec), "aac") {
		t.Errorf("OutputFormat.Codec = %q, want aac", encoded.OutputFormat.Codec)
	}
	if !warned(encoded.Warnings) {
		t.Errorf("warnings = %v, want implicit-lossy for the forced encode", encoded.Warnings)
	}

	// A named format is the codec to deliver. The delivery is already Opus,
	// so .opus is a remux of the packets into Ogg, reported as the file on
	// disk; Force is the caller asking for the encoder anyway; and a format
	// the delivery is not in encodes without implicit-lossy, since the
	// request named it.
	named := download("out.opus", waxtap.TranscodeSpec{Format: waxtap.FormatOpus})
	if named.Transcoded {
		t.Errorf("Transcoded = true, want a copy: the delivery is already opus")
	}
	if warned(named.Warnings) {
		t.Errorf("warnings = %v, want none on a copy", named.Warnings)
	}
	if named.OutputFormat.Extension != "opus" || named.OutputFormat.MIMEType != `audio/ogg; codecs="opus"` {
		t.Errorf("OutputFormat = %+v, want the .opus the packets landed in", named.OutputFormat)
	}
	if pr, err := media.NewRunner(media.RunnerConfig{}).Probe(ctx, filepath.Join(dir, "out.opus")); err != nil {
		t.Fatal(err)
	} else if a, _ := pr.AudioStream(); pr.Format.Container != "ogg" || a.CodecName != "opus" {
		t.Errorf("delivered %s/%s, want ogg/opus", pr.Format.Container, a.CodecName)
	}
	forced := download("forced.opus", waxtap.TranscodeSpec{Format: waxtap.FormatOpus, Force: true})
	if !forced.Transcoded || !strings.EqualFold(forced.OutputFormat.Codec, "opus") {
		t.Errorf("Transcoded = %v codec %q, want the encode Force asks for", forced.Transcoded, forced.OutputFormat.Codec)
	}
	mp3 := download("out.mp3", waxtap.TranscodeSpec{Format: waxtap.FormatMP3})
	if !mp3.Transcoded || warned(mp3.Warnings) {
		t.Errorf("Transcoded = %v warnings %v, want a named encode with no implicit-lossy", mp3.Transcoded, mp3.Warnings)
	}
}
