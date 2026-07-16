package waxtap

import (
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
)

// TestNewProcessResultCopyCutDurationBaseline covers a copy-mode cut whose output
// probe is unavailable: OutputFormat keeps the source codec, but its duration must
// fall back to SourceDuration-Removed rather than the stale uncut source duration.
func TestNewProcessResultCopyCutDurationBaseline(t *testing.T) {
	src := Format{Codec: "opus", Extension: "webm", Duration: 600 * time.Second, ContentLength: 9_000_000}
	p := pipeline.Result{
		Cut:            true,
		Removed:        90 * time.Second,
		SourceDuration: 600 * time.Second,
		OutputCodec:    media.CodecCopy,
		// OutputProbe nil: the best-effort probe failed.
	}
	res := newProcessResult(SourceYouTube, p, src, 0)
	if want := 510 * time.Second; res.OutputFormat.Duration != want {
		t.Errorf("OutputFormat.Duration = %s, want %s (SourceDuration-Removed)", res.OutputFormat.Duration, want)
	}
	// The cut output is smaller than the source; without a probe the exact size is
	// unknown, so the stale source ContentLength must be cleared rather than reported.
	if res.OutputFormat.ContentLength != 0 {
		t.Errorf("OutputFormat.ContentLength = %d, want 0 (unknown post-cut size)", res.OutputFormat.ContentLength)
	}
}

// TestNewProcessResultProbeOverlay covers the authoritative overlay: the output
// probe's rate/channels/duration/size supersede the baseline and the source.
func TestNewProcessResultProbeOverlay(t *testing.T) {
	src := Format{Codec: "opus", Extension: "webm", Duration: 600 * time.Second}
	probe := &media.ProbeResult{
		Format: media.ProbeFormat{Duration: 505 * time.Second, Size: 8_000_000, BitRate: 126000},
		Streams: []media.ProbeStream{
			{CodecType: "audio", SampleRate: 48000, Channels: 2, BitRate: 126000, Duration: 505 * time.Second},
		},
	}
	p := pipeline.Result{
		Cut:            true,
		Removed:        90 * time.Second,
		SourceDuration: 600 * time.Second,
		OutputCodec:    media.CodecCopy,
		OutputProbe:    probe,
	}
	res := newProcessResult(SourceYouTube, p, src, 0)
	if res.OutputFormat.Duration != 505*time.Second {
		t.Errorf("OutputFormat.Duration = %s, want the probe's 505s (supersedes the baseline)", res.OutputFormat.Duration)
	}
	if res.OutputFormat.SampleRate != 48000 || res.OutputFormat.Channels != 2 || res.OutputFormat.Bitrate != 126000 {
		t.Errorf("OutputFormat overlay = %+v, want 48000Hz/2ch/126000bps", res.OutputFormat)
	}
	if res.OutputFormat.ContentLength != 8_000_000 {
		t.Errorf("OutputFormat.ContentLength = %d, want the probe size 8000000", res.OutputFormat.ContentLength)
	}
}

// TestNewProcessResultBitrateFallback covers VBR/lossless outputs whose audio
// stream reports no bitrate: the result falls back to the container bitrate, then
// to a size*8/duration estimate.
func TestNewProcessResultBitrateFallback(t *testing.T) {
	src := Format{Codec: "opus", Extension: "webm", Duration: 10 * time.Second}

	containerOnly := &media.ProbeResult{
		Format:  media.ProbeFormat{Duration: 10 * time.Second, Size: 1_000_000, BitRate: 705000},
		Streams: []media.ProbeStream{{CodecType: "audio", SampleRate: 44100, Channels: 2 /* BitRate 0 */}},
	}
	res := newProcessResult(SourceYouTube, pipeline.Result{Transcoded: true, OutputCodec: media.CodecFLAC, OutputProbe: containerOnly}, src, 0)
	if res.OutputFormat.Bitrate != 705000 {
		t.Errorf("Bitrate = %d, want the container fallback 705000", res.OutputFormat.Bitrate)
	}

	noBitrate := &media.ProbeResult{
		Format:  media.ProbeFormat{Duration: 10 * time.Second, Size: 1_000_000 /* BitRate 0 */},
		Streams: []media.ProbeStream{{CodecType: "audio", SampleRate: 44100, Channels: 2}},
	}
	res2 := newProcessResult(SourceYouTube, pipeline.Result{Transcoded: true, OutputCodec: media.CodecFLAC, OutputProbe: noBitrate}, src, 0)
	if want := int(float64(1_000_000) * 8 / 10); res2.OutputFormat.Bitrate != want {
		t.Errorf("Bitrate = %d, want the size/duration estimate %d", res2.OutputFormat.Bitrate, want)
	}
}
