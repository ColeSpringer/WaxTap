package transcode

import (
	"testing"
	"time"
)

// A cover-art m4a: one audio stream plus an attached mjpeg "video" stream, which
// the audio selector must skip.
const probeJSONAudioVideo = `{
  "streams": [
    {"index":0,"codec_name":"aac","codec_type":"audio","sample_rate":"44100","channels":2,"bit_rate":"128000","duration":"10.005000"},
    {"index":1,"codec_name":"mjpeg","codec_type":"video","duration":"10.000000"}
  ],
  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"10.005000","size":"165000","bit_rate":"131000"}
}`

const probeJSONVideoOnly = `{
  "streams": [
    {"index":0,"codec_name":"h264","codec_type":"video","duration":"5.0"}
  ],
  "format": {"format_name":"mov,mp4","duration":"5.0","size":"1000","bit_rate":"1600"}
}`

func TestParseProbe(t *testing.T) {
	pr, err := parseProbe([]byte(probeJSONAudioVideo))
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	if pr.Format.FormatName != "mov,mp4,m4a,3gp,3g2,mj2" {
		t.Errorf("format name = %q", pr.Format.FormatName)
	}
	if pr.Format.Size != 165000 || pr.Format.BitRate != 131000 {
		t.Errorf("format size/bitrate = %d/%d, want 165000/131000", pr.Format.Size, pr.Format.BitRate)
	}
	if pr.Format.Duration != time.Duration(10.005*float64(time.Second)) {
		t.Errorf("format duration = %v, want ~10.005s", pr.Format.Duration)
	}
	if len(pr.Streams) != 2 {
		t.Fatalf("got %d streams, want 2", len(pr.Streams))
	}

	a, ok := pr.AudioStream()
	if !ok {
		t.Fatal("AudioStream() not found, want the aac stream")
	}
	if a.CodecName != "aac" || a.CodecType != "audio" {
		t.Errorf("audio stream = %s/%s, want aac/audio", a.CodecName, a.CodecType)
	}
	if a.SampleRate != 44100 || a.Channels != 2 || a.BitRate != 128000 {
		t.Errorf("audio = %dHz %dch %dbps, want 44100/2/128000", a.SampleRate, a.Channels, a.BitRate)
	}
}

func TestParseProbeNoAudio(t *testing.T) {
	pr, err := parseProbe([]byte(probeJSONVideoOnly))
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	if _, ok := pr.AudioStream(); ok {
		t.Fatal("AudioStream() found a stream in a video-only input, want none")
	}
}

func TestParseProbeBitDepth(t *testing.T) {
	// A 24-bit FLAC: bit depth is carried by bits_per_raw_sample (a JSON string),
	// while bits_per_sample (a JSON number) is 0 and sample_fmt is the s32 carrier.
	const j = `{"streams":[{"index":0,"codec_name":"flac","codec_type":"audio","sample_fmt":"s32","bits_per_sample":0,"bits_per_raw_sample":"24","channels":2}],"format":{"format_name":"flac"}}`
	pr, err := parseProbe([]byte(j))
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	a, ok := pr.AudioStream()
	if !ok {
		t.Fatal("AudioStream() not found")
	}
	if a.SampleFmt != "s32" {
		t.Errorf("SampleFmt = %q, want s32", a.SampleFmt)
	}
	if a.BitsPerSample != 0 || a.BitsPerRawSample != 24 {
		t.Errorf("bits = sample %d / raw %d, want 0 / 24", a.BitsPerSample, a.BitsPerRawSample)
	}
	if a.effectiveBits() != 24 {
		t.Errorf("effectiveBits() = %d, want 24", a.effectiveBits())
	}
}

func TestParseProbeMalformed(t *testing.T) {
	if _, err := parseProbe([]byte("{ this is not json")); err == nil {
		t.Fatal("parseProbe(garbage) = nil error, want error")
	}
}

func TestParseProbeMissingNumbers(t *testing.T) {
	// ffprobe emits "N/A" or omits numeric fields; those must parse as zero, not
	// error.
	const j = `{"streams":[{"index":0,"codec_name":"opus","codec_type":"audio","sample_rate":"N/A","channels":2}],"format":{"format_name":"ogg"}}`
	pr, err := parseProbe([]byte(j))
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	a, ok := pr.AudioStream()
	if !ok {
		t.Fatal("AudioStream() not found")
	}
	if a.SampleRate != 0 || a.BitRate != 0 || a.Duration != 0 {
		t.Errorf("unparseable numbers should be zero, got %dHz %dbps %v", a.SampleRate, a.BitRate, a.Duration)
	}
}
