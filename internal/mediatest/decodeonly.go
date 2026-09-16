package mediatest

import (
	_ "embed"
	"slices"
)

// The fixtures below are WaxFlow's own, checked in here because WaxTap can
// write none of them: each holds a codec the engine only decodes, and in the
// first two the container is one WaxTap does write, so the file's name says
// nothing about what is inside. Nothing here is a real recording.

// alawWAV is WaxFlow's G.711 fixture: one second of a synthesized sine as
// A-law in a WAV (format tag 6), 8 kHz mono, written by ffmpeg's pcm_alaw
// encoder; the engine's differential oracle decodes it bit for bit.
//
//go:embed testdata/alaw.wav
var alawWAV []byte

// ALawWAV returns a copy of the A-law WAV fixture.
func ALawWAV() []byte { return slices.Clone(alawWAV) }

// The A-law fixture's shape: what a probe reports and what a decode delivers,
// since a byte-linear payload counts its own length.
const (
	ALawWAVRate          = 8000
	ALawWAVSamples int64 = 8000
)

// mp3WAV is WaxFlow's MP3-in-WAV fixture: the frames of a small MP3
// (22.05 kHz mono, about a second of synthesized sine) wrapped in a WAV data
// chunk by ffmpeg's WAV muxer (format tag 0x0055). Its fact chunk counts
// everything the frames decode to, delay and padding included, and the
// MPEGLAYER3WAVEFORMAT's nCodecDelay is the constant ffmpeg writes for every
// MP3 it wraps, which the engine reads and declines to apply: the note it
// leaves about that is the one remark on a well-formed file WaxTap's tests
// can trigger from a fixture.
//
//go:embed testdata/mp3.wav
var mp3WAV []byte

// MP3WAV returns a copy of the MP3-in-WAV fixture.
func MP3WAV() []byte { return slices.Clone(mp3WAV) }

// The MP3-in-WAV fixture's shape: the fact chunk's count, which a probe
// reports as an advisory length and a decode of the WAV delivers whole (no
// trim applies inside the WAV); MP3WAVFrameSamples is what the same frames
// report once copied out into a bare .mp3, whose own header states the
// 529-sample decoder delay the WAV could not.
const (
	MP3WAVRate               = 22050
	MP3WAVSamples      int64 = 23616
	MP3WAVFrameSamples int64 = 23087
)

// losslessWMA is WaxFlow's WMA Lossless demuxer cell: 8114 samples of a
// synthesized signal at 44.1 kHz stereo, 16-bit, written by Windows' own
// encoder (the only one that exists), in ASF. It carries no tags.
//
//go:embed testdata/lossless.wma
var losslessWMA []byte

// LosslessWMA returns a copy of the WMA Lossless fixture.
func LosslessWMA() []byte { return slices.Clone(losslessWMA) }

// The WMA Lossless fixture's shape: ASF states the length in milliseconds and
// the codec carries no padding count, so LosslessWMASamples is the header's
// advisory count and a decode runs to the frame boundary past it (frames of
// LosslessWMAFrame samples at this rate); pin a decode with >=.
const (
	LosslessWMARate           = 44100
	LosslessWMASamples  int64 = 8114
	LosslessWMAFrame    int64 = 2048
	LosslessWMAChannels       = 2
)
