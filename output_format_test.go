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
// probe's rate/channels/duration/size supersede the baseline and the source, and
// the bitrate the audio stream itself does not carry is estimated from them.
func TestNewProcessResultProbeOverlay(t *testing.T) {
	src := Format{Codec: "opus", Extension: "webm", Duration: 600 * time.Second}
	probe := &media.ProbeResult{
		Format: media.ProbeFormat{Duration: 500 * time.Second, Size: 8_000_000},
		Streams: []media.ProbeStream{
			{CodecType: "audio", SampleRate: 48000, Channels: 2, Duration: 500 * time.Second},
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
	if res.OutputFormat.Duration != 500*time.Second {
		t.Errorf("OutputFormat.Duration = %s, want the probe's 500s (supersedes the baseline)", res.OutputFormat.Duration)
	}
	wantBitrate := int(float64(8_000_000) * 8 / 500)
	if res.OutputFormat.SampleRate != 48000 || res.OutputFormat.Channels != 2 || res.OutputFormat.Bitrate != wantBitrate {
		t.Errorf("OutputFormat overlay = %+v, want 48000Hz/2ch/%dbps (size/duration estimate)", res.OutputFormat, wantBitrate)
	}
	if res.OutputFormat.ContentLength != 8_000_000 {
		t.Errorf("OutputFormat.ContentLength = %d, want the probe size 8000000", res.OutputFormat.ContentLength)
	}
}

// TestNewProcessResultBitrateFallback covers VBR/lossless outputs whose audio
// stream reports no bitrate: the result falls back to a size*8/duration estimate.
func TestNewProcessResultBitrateFallback(t *testing.T) {
	src := Format{Codec: "opus", Extension: "webm", Duration: 10 * time.Second}

	probe := &media.ProbeResult{
		Format:  media.ProbeFormat{Duration: 10 * time.Second, Size: 1_000_000},
		Streams: []media.ProbeStream{{CodecType: "audio", SampleRate: 44100, Channels: 2}},
	}
	res := newProcessResult(SourceYouTube, pipeline.Result{Transcoded: true, OutputCodec: media.CodecFLAC, OutputProbe: probe}, src, 0)
	if want := int(float64(1_000_000) * 8 / 10); res.OutputFormat.Bitrate != want {
		t.Errorf("Bitrate = %d, want the size/duration estimate %d", res.OutputFormat.Bitrate, want)
	}
}
