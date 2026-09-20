package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/download"
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

// probeFixtureSeconds is the length of the fixtures these tests probe. It has
// to exceed one DefaultRangeBlock of encoded audio, or "the probe read less
// than the file" cannot hold: a two-second Opus file is smaller than one block,
// so a ranged read of it fetches the whole thing and proves nothing.
const probeFixtureSeconds = 60

// m4aPlayerJSON is probePlayerJSON for the itag-140 AAC row.
func m4aPlayerJSON(vd string, clen int, extraQuery string) string {
	return fmt.Sprintf(`{
		"responseContext": {},
		"playabilityStatus": {"status": "OK"},
		"streamingData": {
			"expiresInSeconds": "21540",
			"adaptiveFormats": [{
				"itag": 140,
				"mimeType": "audio/mp4; codecs=\"mp4a.40.2\"",
				"bitrate": 130000,
				"averageBitrate": 128000,
				"contentLength": "%d",
				"audioSampleRate": "48000",
				"audioChannels": 2,
				"url": "https://rr1---sn-test.googlevideo.com/videoplayback?itag=140&vd=%s%s"
			}]
		},
		"videoDetails": {"videoId": "dummyVideo0", "title": "Probe Test", "author": "T"}
	}`, clen, vd, extraQuery)
}

// lengthlessPlayerJSON is probePlayerJSON with no contentLength: nothing states
// the resource's size, which is what a ranged reader needs to align blocks.
func lengthlessPlayerJSON(vd string, _ int, extraQuery string) string {
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
				"audioSampleRate": "48000",
				"audioChannels": 2,
				"url": "https://rr1---sn-test.googlevideo.com/videoplayback?itag=251&vd=%s%s"
			}]
		},
		"videoDetails": {"videoId": "dummyVideo0", "title": "Probe Test", "author": "T"}
	}`, vd, extraQuery)
}

// webmOpusFixture encodes a synthetic tone as WebM Opus, the row YouTube serves
// by default and the one a probe reads the headers of.
func webmOpusFixture(t *testing.T) []byte {
	t.Helper()
	return encodedProbeFixture(t, FormatOpus, ".webm")
}

// m4aFixture encodes the same tone as AAC in an MP4. Its moov sits at the end,
// so a probe reads the head and then seeks to the tail: two blocks, not a
// sweep.
func m4aFixture(t *testing.T) []byte {
	t.Helper()
	return encodedProbeFixture(t, FormatAAC, ".m4a")
}

func encodedProbeFixture(t *testing.T, f TranscodeFormat, ext string) []byte {
	t.Helper()
	dir := t.TempDir()
	wav := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(probeFixtureSeconds, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "src"+ext)
	if _, err := newOfflineClient(t).Process(context.Background(), ProcessRequest{Input: wav, ProcessSpec: ProcessSpec{
		Output: ToFile(out), Transcode: &TranscodeSpec{Format: f},
	}}); err != nil {
		t.Fatalf("%s fixture: %v", ext, err)
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
	want := probeFixtureSeconds * time.Second
	if f.SampleRate != 48000 || f.Channels != 2 || f.Duration != want || f.ContentLength != int64(len(webm)) {
		t.Errorf("probed row = rate %d ch %d dur %v len %d, want 48000/2/%v/%d", f.SampleRate, f.Channels, f.Duration, f.ContentLength, want, len(webm))
	}
	if res.Video.Duration != want {
		t.Errorf("Video.Duration = %v, want the probed %v", res.Video.Duration, want)
	}
}

// A probe outlives a signed URL as readily as a download does, and it refreshes
// and rotates the same way. The refresh happens on the ranged path: the first
// block's request is the one that 403s, before a demuxer has read anything.
func TestInfoProbeRefreshesACappedURL(t *testing.T) {
	media := webmOpusFixture(t)
	w := &rotationWorld{media: string(media), player: probePlayerJSON} // first identity capped
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
	if got := w.mediaCodes; len(got) != 2 || got[0] != 403 || got[1] != 200 {
		t.Errorf("media status codes = %v, want [403 200] on the ranged path", got)
	}
	if w.bytesServed >= len(media) {
		t.Errorf("served %d bytes of a %d-byte file; the probe read the whole stream", w.bytesServed, len(media))
	}
}

// TestInfoProbeReadsAWebMHeadByRange: a Matroska open reads to the first
// cluster and no further, so one range request answers the probe.
func TestInfoProbeReadsAWebMHeadByRange(t *testing.T) {
	media := webmOpusFixture(t)
	w := &rotationWorld{media: string(media), player: probePlayerJSON, uncapped: true}
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed {
		t.Fatal("Probed = false")
	}
	if got := res.Video.Formats[res.BestIndex].Duration; got != probeFixtureSeconds*time.Second {
		t.Errorf("Duration = %v, want the container's declared %ds", got, probeFixtureSeconds)
	}
	if len(w.mediaRanges) != 1 {
		t.Errorf("media requests = %v, want exactly one range", w.mediaRanges)
	}
	if w.bytesServed > download.DefaultRangeBlock {
		t.Errorf("served %d bytes, want at most one %d-byte block", w.bytesServed, download.DefaultRangeBlock)
	}
}

// TestInfoProbeReadsAnM4AMoovByRange: an MP4's moov is at one end or the other,
// so a probe of one reads the head and, for a moov-at-end file like this
// fixture, the tail. Two blocks is a property of where this muxer puts its
// moov, not a universal bound; what is universal is that a probe reads the
// ends rather than the file.
func TestInfoProbeReadsAnM4AMoovByRange(t *testing.T) {
	media := m4aFixture(t)
	w := &rotationWorld{media: string(media), player: m4aPlayerJSON, uncapped: true}
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed {
		t.Fatal("Probed = false")
	}
	f := res.Video.Formats[res.BestIndex]
	// The AAC encode keeps the WAV fixture's rate; Opus resamples to 48 kHz.
	if f.SampleRate != 44100 || f.Channels != 2 {
		t.Errorf("probed row = rate %d ch %d, want 44100/2", f.SampleRate, f.Channels)
	}
	if d := f.Duration - probeFixtureSeconds*time.Second; d > 50*time.Millisecond || d < -50*time.Millisecond {
		t.Errorf("Duration = %v, want within 50ms of %ds", f.Duration, probeFixtureSeconds)
	}
	if w.bytesServed > 2*download.DefaultRangeBlock {
		t.Errorf("served %d bytes, want at most two %d-byte blocks", w.bytesServed, download.DefaultRangeBlock)
	}
	if w.bytesServed >= len(media)/2 {
		t.Errorf("served %d bytes of a %d-byte file; a probe reads the ends, not the file", w.bytesServed, len(media))
	}
}

// TestInfoProbeReadsAFragmentedM4AHeadByRange: YouTube's itag 140 is a
// fragmented MP4 with no edit list, its length stated by the segment index
// behind the moov. The open stops at the moov and reads ahead only to the
// first moof, so the probe answers from the first block with the duration the
// sidx sums. This row used to read to the end of the fragments looking for a
// length that was never there, and fell to the staging path once the budget
// stopped it.
func TestInfoProbeReadsAFragmentedM4AHeadByRange(t *testing.T) {
	media, err := mediatest.FragmentAAC(m4aFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	w := &rotationWorld{media: string(media), player: m4aPlayerJSON, uncapped: true}
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed {
		t.Fatal("Probed = false")
	}
	f := res.Video.Formats[res.BestIndex]
	if f.SampleRate != 44100 || f.Channels != 2 {
		t.Errorf("probed row = rate %d ch %d, want 44100/2", f.SampleRate, f.Channels)
	}
	// The sidx counts the fragments' raw frames, encoder priming and padding
	// included, so the head states a little over the tone's length.
	if d := f.Duration - probeFixtureSeconds*time.Second; d > 100*time.Millisecond || d < -50*time.Millisecond {
		t.Errorf("Duration = %v, want within 100ms of %ds", f.Duration, probeFixtureSeconds)
	}
	if len(w.mediaRanges) != 1 {
		t.Errorf("media requests = %v, want one: the head is in the first block", w.mediaRanges)
	}
	if w.bytesServed > download.DefaultRangeBlock {
		t.Errorf("served %d bytes, want at most one %d-byte block", w.bytesServed, download.DefaultRangeBlock)
	}
}

// TestInfoProbeStagesWhenTheOriginIgnoresRanges: an origin that answers a
// bounded request with the whole body cannot be read in ranges, so the probe
// stages the stream. It does that with one open-ended request rather than
// through ToFile, whose parallel bounded chunks would meet the same refusal
// again on any stream past the chunk size; this fixture is well under it, so
// what the request count pins here is the single sequential fetch.
func TestInfoProbeStagesWhenTheOriginIgnoresRanges(t *testing.T) {
	media := webmOpusFixture(t)
	w := &rotationWorld{media: string(media), player: probePlayerJSON, uncapped: true, ignoreRange: true}
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed {
		t.Fatal("Probed = false")
	}
	// One bounded attempt, which the origin ignored, then one open-ended fetch.
	// The bounded one is not retried: an origin that ignored the range will
	// ignore the retry too.
	if len(w.mediaRanges) != 2 {
		t.Fatalf("media requests = %v, want the bounded attempt then the staged fetch", w.mediaRanges)
	}
	if w.mediaRanges[1] != "0-" {
		t.Errorf("staging request range = %q, want the open-ended %q", w.mediaRanges[1], "0-")
	}
	if w.bytesServed < len(media) {
		t.Errorf("served %d bytes of a %d-byte file, want the whole stream staged", w.bytesServed, len(media))
	}
}

// TestInfoProbeStagesWhenTheHeadersOutrunTheBudget: a container whose head
// outruns the budget would turn a ranged read into the whole download in
// round-trip pieces. The budget stops it and the probe stages instead, so the
// answer is the same and the cost is bounded. The budget is lowered here rather
// than shipping a fixture large enough to trip the real one.
func TestInfoProbeStagesWhenTheHeadersOutrunTheBudget(t *testing.T) {
	// The m4a fixture needs its head and its tail, so a budget under one block
	// lets the eager first block through and refuses the second.
	media := m4aFixture(t)
	saved := probeRangeBudget
	t.Cleanup(func() { probeRangeBudget = saved })
	probeRangeBudget = 1

	w := &rotationWorld{media: string(media), player: m4aPlayerJSON, uncapped: true}
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed {
		t.Fatal("Probed = false, want the staged probe to have answered")
	}
	if d := res.Video.Formats[res.BestIndex].Duration - probeFixtureSeconds*time.Second; d > 50*time.Millisecond || d < -50*time.Millisecond {
		t.Errorf("Duration = %v, want the staged probe's %ds", res.Video.Formats[res.BestIndex].Duration, probeFixtureSeconds)
	}
	if len(w.mediaRanges) < 2 || w.mediaRanges[len(w.mediaRanges)-1] != "0-" {
		t.Errorf("media requests = %v, want the ranged attempt then an open-ended staging fetch", w.mediaRanges)
	}
	if w.bytesServed < len(media) {
		t.Errorf("served %d bytes of a %d-byte file, want the whole stream staged", w.bytesServed, len(media))
	}
}

// TestInfoProbeStagesWithoutALength: a row with no contentLength gives the
// reader no size to align blocks against, so the probe stages instead.
func TestInfoProbeStagesWithoutALength(t *testing.T) {
	media := webmOpusFixture(t)
	w := &rotationWorld{media: string(media), player: lengthlessPlayerJSON, uncapped: true}
	c := rotationClient(t, w)

	res, err := c.InfoResult(context.Background(), "dummyVideo0", InfoProbe)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Probed {
		t.Fatal("Probed = false")
	}
	if w.bytesServed < len(media) {
		t.Errorf("served %d bytes of a %d-byte file, want the whole stream staged", w.bytesServed, len(media))
	}
}
