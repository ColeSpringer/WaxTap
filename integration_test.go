//go:build integration

// Live integration tests for the facade. They are excluded from the default
// build; run them with:
//
//	go test -tags=integration .
//
// These make real YouTube requests and are more brittle than the offline tests.
// Useful environment variables:
//   - WAXTAP_TEST_VIDEO=<id>  override the default video.
//
// They prove the end-to-end MVP path: extract -> select -> resolve -> download.
// When the current network requires a PO token but none is configured, they skip
// (an expected environment condition, not a failure).
package waxtap_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v2"
	"github.com/colespringer/waxtap/v2/waxerr"
)

func liveURL() string {
	if v := os.Getenv("WAXTAP_TEST_VIDEO"); v != "" {
		return v
	}
	return "rFejpH_tAHM" // dotGo 2015, Rob Pike; stable, public, not age-gated
}

func skipIfTokenGated(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, waxerr.ErrNeedsPOToken) || errors.Is(err, waxerr.ErrURLExpired) {
		t.Skipf("network requires a PO token / URL expired with no provider: %v", err)
	}
}

// TestLive_DownloadBestAudio downloads the best audio stream to a file with no
// re-encode, the default keep-source path, and checks bytes landed.
func TestLive_DownloadBestAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out := filepath.Join(t.TempDir(), "track")
	res, err := client.Download(ctx, waxtap.Request{
		URL:         liveURL(),
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
	})
	if err != nil {
		skipIfTokenGated(t, err)
		t.Fatalf("Download: %v", err)
	}
	if res.OutputBytes <= 0 || fileSizeAt(t, res.OutputPath) != res.OutputBytes {
		t.Errorf("OutputBytes=%d, file=%d", res.OutputBytes, fileSizeAt(t, res.OutputPath))
	}
	t.Logf("downloaded %q (%d bytes, %s)", res.Title, res.OutputBytes, res.SourceFormat)
}

// TestLive_DownloadTranscodeFLAC exercises the fused pipeline end to end: download
// then transcode to FLAC. Requires ffmpeg.
func TestLive_DownloadTranscodeFLAC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	out := filepath.Join(t.TempDir(), "track.flac")
	res, err := client.Download(ctx, waxtap.Request{
		URL: liveURL(),
		ProcessSpec: waxtap.ProcessSpec{
			Transcode: &waxtap.TranscodeSpec{Format: waxtap.FormatFLAC},
			Output:    waxtap.ToFile(out),
		},
	})
	if err != nil {
		skipIfTokenGated(t, err)
		if errors.Is(err, waxerr.ErrFFmpegNotFound) {
			t.Skip("ffmpeg not installed")
		}
		t.Fatalf("Download+transcode: %v", err)
	}
	if !res.Transcoded || res.OutputFormat.Codec != "flac" {
		t.Errorf("result = %+v, want transcoded FLAC", res)
	}
}

// TestLive_InfoProbe resolves and ffprobes the best-audio format, filling its
// authoritative sample rate and channel count. Requires ffmpeg.
func TestLive_InfoProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	client, err := waxtap.New(waxtap.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	video, err := client.Info(ctx, liveURL(), waxtap.InfoProbe)
	if err != nil {
		skipIfTokenGated(t, err)
		if errors.Is(err, waxerr.ErrFFmpegNotFound) {
			t.Skip("ffmpeg not installed")
		}
		t.Fatalf("Info(InfoProbe): %v", err)
	}
	// At least one format should now carry probed sample rate and channels.
	probed := false
	for _, f := range video.Formats {
		if f.SampleRate > 0 && f.Channels > 0 {
			probed = true
			break
		}
	}
	if !probed {
		t.Error("InfoProbe filled no probed sample rate/channels on any format")
	}
}

func fileSizeAt(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}
