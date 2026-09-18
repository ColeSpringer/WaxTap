package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// probePlayerJSON is rotationPlayerJSON without lengthSeconds or
// approxDurationMs: a manifest that names no length anywhere, so the probed
// length is the only one.
func probePlayerJSON(vd string, clen int, extraQuery string) string {
	return fmt.Sprintf(`{
		"responseContext": {},
		"playabilityStatus": {"status": "OK"},
		"streamingData": {
			"expiresInSeconds": "21540",
			"adaptiveFormats": [{
				"itag": 251,
				"mimeType": "audio/webm; codecs=\"opus\"",
				"bitrate": 160000,
				"averageBitrate": 130000,
				"contentLength": "%d",
				"audioSampleRate": "48000",
				"audioChannels": 2,
				"url": "https://rr1---sn-test.googlevideo.com/videoplayback?itag=251&vd=%s%s"
			}]
		},
		"videoDetails": {"videoId": "dummyVideo0", "title": "Probe Test", "author": "T"}
	}`, clen, vd, extraQuery)
}

// webmOpusFixture encodes a synthetic tone as WebM Opus, the row YouTube serves
// by default and the one a probe of a staged stream reads.
func webmOpusFixture(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	wav := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "src.webm")
	if _, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatOpus},
	}}); err != nil {
		t.Fatalf("webm opus fixture: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasInfoWarning(ws []Warning, code WarningCode) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

// The probe is the only measurement of a manifest that names no length, and it
// lands on both the row and the video.
func TestInfoProbeBackfillsDurationFromTheProbe(t *testing.T) {
	webm := webmOpusFixture(t)
	w := &rotationWorld{media: string(webm), player: probePlayerJSON, uncapped: true}
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed || res.BestIndex < 0 {
		t.Fatalf("Probed=%v BestIndex=%d", res.Probed, res.BestIndex)
	}
	f := res.Video.Formats[res.BestIndex]
	if f.SampleRate != 48000 || f.Channels != 2 || f.Duration != 2*time.Second || f.ContentLength != int64(len(webm)) {
		t.Errorf("probed row = rate %d ch %d dur %v len %d", f.SampleRate, f.Channels, f.Duration, f.ContentLength)
	}
	if res.Video.Duration != 2*time.Second {
		t.Errorf("Video.Duration = %v, want the probed 2s", res.Video.Duration)
	}
}

// The probe downloads the whole stream, so it is the read most likely to
// outlive a signed URL; it refreshes and rotates the way a download does.
func TestInfoProbeRefreshesACappedURL(t *testing.T) {
	w := &rotationWorld{media: string(webmOpusFixture(t)), player: probePlayerJSON} // first identity capped
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed || w.homepageHits != 2 {
		t.Fatalf("Probed=%v homepageHits=%d, want a rotation and a probe", res.Probed, w.homepageHits)
	}
	if !hasInfoWarning(res.Warnings, WarnSessionRotated) {
		t.Errorf("warnings = %+v, want a session-rotated warning", res.Warnings)
	}
}
