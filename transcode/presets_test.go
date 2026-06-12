package transcode

import "testing"

func TestPresetTable(t *testing.T) {
	cases := []struct {
		codec     Codec
		encoder   string
		extension string
		muxer     string
		lossless  bool
		name      string
	}{
		{CodecCopy, "copy", "", "", true, "copy"},
		{CodecFLAC, "flac", "flac", "flac", true, "flac"},
		{CodecALAC, "alac", "m4a", "ipod", true, "alac"},
		{CodecWAV, "pcm_s16le", "wav", "wav", true, "wav"},
		{CodecMP3, "libmp3lame", "mp3", "mp3", false, "mp3"},
		{CodecAAC, "aac", "m4a", "ipod", false, "aac"},
		{CodecOpus, "libopus", "opus", "opus", false, "opus"},
		{CodecVorbis, "libvorbis", "ogg", "ogg", false, "vorbis"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := presetFor(c.codec)
			if err != nil {
				t.Fatalf("presetFor(%v): %v", c.codec, err)
			}
			if p.encoder != c.encoder {
				t.Errorf("encoder = %q, want %q", p.encoder, c.encoder)
			}
			if p.muxer != c.muxer {
				t.Errorf("muxer = %q, want %q", p.muxer, c.muxer)
			}
			if c.codec.Extension() != c.extension {
				t.Errorf("Extension() = %q, want %q", c.codec.Extension(), c.extension)
			}
			if c.codec.IsLossless() != c.lossless {
				t.Errorf("IsLossless() = %v, want %v", c.codec.IsLossless(), c.lossless)
			}
			if c.codec.String() != c.name {
				t.Errorf("String() = %q, want %q", c.codec.String(), c.name)
			}
		})
	}
}

func TestWAVEncoder(t *testing.T) {
	cases := []struct {
		name string
		in   ProbeStream
		want string
	}{
		{"16-bit pcm", ProbeStream{CodecName: "pcm_s16le", BitsPerSample: 16, SampleFmt: "s16"}, "pcm_s16le"},
		{"24-bit flac (raw-sample depth)", ProbeStream{CodecName: "flac", BitsPerRawSample: 24, SampleFmt: "s32"}, "pcm_s24le"},
		{"24-bit pcm (coded depth)", ProbeStream{CodecName: "pcm_s24le", BitsPerSample: 24, BitsPerRawSample: 24, SampleFmt: "s32"}, "pcm_s24le"},
		{"32-bit int pcm", ProbeStream{CodecName: "pcm_s32le", BitsPerSample: 32, SampleFmt: "s32"}, "pcm_s32le"},
		{"lossy float decode falls back to 16-bit", ProbeStream{CodecName: "opus", SampleFmt: "fltp"}, "pcm_s16le"},
		// A real float WAV reports bits_per_sample=32, so float preservation must
		// win over the integer-depth mapping (regression: was returning pcm_s32le).
		{"32-bit float pcm preserved", ProbeStream{CodecName: "pcm_f32le", SampleFmt: "flt", BitsPerSample: 32}, "pcm_f32le"},
		{"64-bit double pcm preserved", ProbeStream{CodecName: "pcm_f64le", SampleFmt: "dbl", BitsPerSample: 64}, "pcm_f64le"},
		{"unknown depth and format", ProbeStream{CodecName: "aac", SampleFmt: ""}, "pcm_s16le"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wavEncoder(c.in); got != c.want {
				t.Errorf("wavEncoder(%+v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestPresetForUnknown(t *testing.T) {
	if _, err := presetFor(Codec(200)); err == nil {
		t.Fatal("presetFor(unknown) = nil error, want error")
	}
	// Accessor methods stay safe (zero values) on an unknown codec.
	if got := Codec(200).Extension(); got != "" {
		t.Errorf("unknown Extension() = %q, want \"\"", got)
	}
	if Codec(200).IsLossless() {
		t.Error("unknown IsLossless() = true, want false")
	}
	if got := Codec(200).String(); got != "codec(200)" {
		t.Errorf("unknown String() = %q, want codec(200)", got)
	}
}
