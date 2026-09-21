package tests

import (
	"context"
	gocolor "image/color"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// A WebM delivery whose container cannot hold a picture is remuxed into its
// codec's own Ogg so the cover art can go in, and the result says so: the
// delivered format names the Ogg container, and the delivery is still a copy
// (the packets are the source's, under the source's itag).
func TestFacade_CoverArtRemuxReportsTheDeliveredContainer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A real Opus-in-WebM body, so the embed pass has something to remux.
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
	cover := mediatest.PNGBytes(mediatest.SolidCover(320, 180, gocolor.RGBA{B: 200, A: 255}))

	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "1" {
				return resp(http.StatusOK, []byte(sabrPlayerJSON)), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			return resp(http.StatusOK, umpBody), nil
		case strings.Contains(r.URL.Host, "ytimg") || strings.HasSuffix(r.URL.Path, ".jpg") || strings.HasSuffix(r.URL.Path, ".webp"):
			return resp(http.StatusOK, cover), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})

	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, POTokenProvider: fProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "e.opus")
	res, err := c.Download(ctx, waxtap.Request{
		URL:         "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out), EmbedThumbnail: true},
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if res.Transcoded {
		t.Error("a cover-art remux copies packets; it must not transcode")
	}
	if res.OutputFormat.Extension != "opus" {
		t.Errorf("OutputFormat.Extension = %q, want opus (the Ogg the file is really in)", res.OutputFormat.Extension)
	}
	if !strings.HasPrefix(res.OutputFormat.MIMEType, "audio/ogg") {
		t.Errorf("OutputFormat.MIMEType = %q, want an audio/ogg type", res.OutputFormat.MIMEType)
	}
	if res.OutputFormat.Itag != res.SourceFormat.Itag {
		t.Errorf("OutputFormat itag %d != SourceFormat itag %d; a remux keeps the delivery's identity", res.OutputFormat.Itag, res.SourceFormat.Itag)
	}
	if pr, perr := media.NewRunner(media.RunnerConfig{}).Probe(ctx, out); perr != nil {
		t.Fatalf("probe the delivered file: %v", perr)
	} else if pr.Format.Container != "ogg" {
		t.Errorf("delivered container = %q, want ogg", pr.Format.Container)
	}
}
