package tests

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// realOpusWebM returns the bytes of a real one-second Opus-in-WebM file, so the
// SABR fake can serve audio the pipeline can actually decode. The other facade
// tests serve placeholder strings, which is enough for byte-copy assertions but
// not for a download that transcodes.
func realOpusWebM(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(src, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "src.webm")
	r := media.NewRunner(media.RunnerConfig{})
	if _, err := r.Transcode(context.Background(), src, out, media.Spec{Codec: media.CodecOpus}); err != nil {
		t.Fatalf("synth opus: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// opusFmt251 is an itag 251 entry for sabrPlayerJSONForFmts, declaring the size of
// the served payload so the download is a complete delivery rather than a short
// one. A non-positive length omits contentLength entirely, which is how a player
// response that does not declare one arrives.
func opusFmt251(contentLength int) string {
	declared := ""
	if contentLength > 0 {
		declared = fmt.Sprintf(`"contentLength": "%d",`, contentLength)
	}
	return fmt.Sprintf(`{"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 130000,
       %s "audioQuality": "AUDIO_QUALITY_MEDIUM",
       "audioSampleRate": "48000", "audioChannels": 2,
       "lastModified": "1700000000000001"}`, declared)
}

// TestFacade_ContentLengthIsDeliveredSize pins the F8 invariant for every
// file-output download: OutputFormat.ContentLength, OutputBytes, and the size on
// disk all agree. Asserting the invariant rather than per-case sizes covers
// transcode, cut, measure-only, and keep-source in one place and cannot drift as
// the post-processing chain grows.
//
// The defect it guards: the pipeline's output probe (which fed ContentLength) runs
// before the embed post-pass rewrites the file, so ContentLength described a size
// that was no longer on disk.
func TestFacade_ContentLengthIsDeliveredSize(t *testing.T) {
	payload := realOpusWebM(t)
	umpBody := fSabrHappyBody(nil, payload)

	newRT := func(player string) roundTripFn {
		return func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/v1/player"):
				return resp(http.StatusOK, []byte(player)), nil
			case strings.Contains(r.URL.Path, "/videoplayback"):
				return resp(http.StatusOK, umpBody), nil
			default:
				return resp(http.StatusNotFound, nil), nil
			}
		}
	}
	declared := newRT(sabrPlayerJSONForFmts("ANDROID_VR", opusFmt251(len(payload))))

	cases := []struct {
		name string
		ext  string
		rt   roundTripFn // nil uses the declared-length player response
		spec func(out string) waxtap.ProcessSpec
	}{
		{"keep-source", ".webm", nil, func(out string) waxtap.ProcessSpec {
			return waxtap.ProcessSpec{Output: waxtap.ToFile(out)}
		}},
		// Keep-source copies the player's format straight through, so a response with
		// no contentLength is the case that reported zero for a file that delivered
		// fine. Nothing forces YouTube to declare it.
		{"keep-source-undeclared", ".webm", newRT(sabrPlayerJSONForFmts("ANDROID_VR", opusFmt251(0))),
			func(out string) waxtap.ProcessSpec {
				return waxtap.ProcessSpec{Output: waxtap.ToFile(out)}
			}},
		{"transcode", ".flac", nil, func(out string) waxtap.ProcessSpec {
			return waxtap.ProcessSpec{
				Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
				Output:    waxtap.ToFile(out),
			}
		}},
		{"measure-only", ".webm", nil, func(out string) waxtap.ProcessSpec {
			return waxtap.ProcessSpec{
				Loudness: &waxtap.LoudnessSpec{Mode: waxtap.LoudnessMeasureOnly},
				Output:   waxtap.ToFile(out),
			}
		}},
		{"normalize", ".flac", nil, func(out string) waxtap.ProcessSpec {
			return waxtap.ProcessSpec{
				Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
				Loudness:  &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: -14},
				Output:    waxtap.ToFile(out),
			}
		}},
		{"cut", ".flac", nil, func(out string) waxtap.ProcessSpec {
			return waxtap.ProcessSpec{
				Cut: &waxtap.CutSpec{Ranges: []waxtap.TimeRange{
					{Start: 200 * time.Millisecond, End: 400 * time.Millisecond},
				}},
				Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
				Output:    waxtap.ToFile(out),
			}
		}},
		{"embed-metadata", ".webm", nil, func(out string) waxtap.ProcessSpec {
			return waxtap.ProcessSpec{Output: waxtap.ToFile(out), IncludeMetadata: true}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := tc.rt
			if rt == nil {
				rt = declared
			}
			c, err := waxtap.New(waxtap.Options{
				HTTPClient:      &http.Client{Transport: rt},
				POTokenProvider: fProvider{},
			})
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "track"+tc.ext)
			req := waxtap.Request{URL: "dummyVideo0", ProcessSpec: tc.spec(out)}
			if tc.name == "embed-metadata" {
				req.EmbedMetadata = true
			}
			res, err := c.Download(context.Background(), req)
			if err != nil {
				t.Fatalf("download: %v", err)
			}

			fi, err := os.Stat(res.OutputPath)
			if err != nil {
				t.Fatalf("stat output: %v", err)
			}
			if res.OutputBytes != fi.Size() {
				t.Errorf("OutputBytes = %d, file = %d", res.OutputBytes, fi.Size())
			}
			if res.OutputFormat.ContentLength != fi.Size() {
				t.Errorf("OutputFormat.ContentLength = %d, file = %d",
					res.OutputFormat.ContentLength, fi.Size())
			}
		})
	}
}
