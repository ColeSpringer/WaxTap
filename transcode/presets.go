package transcode

import (
	"fmt"
	"strings"
)

// Codec identifies an output audio encoding. CodecCopy stream-copies the source
// audio into a container. The lossless codecs (FLAC, ALAC, WAV) still decode and
// encode; they are not stream copies.
type Codec uint8

const (
	CodecCopy   Codec = iota // stream copy / remux (the only no-re-encode path)
	CodecFLAC                // lossless re-encode (.flac)
	CodecALAC                // lossless re-encode (Apple Lossless in .m4a)
	CodecWAV                 // lossless PCM (.wav)
	CodecMP3                 // libmp3lame (V0 by default, CBR when a bitrate is set)
	CodecAAC                 // native AAC in .m4a
	CodecOpus                // libopus (.opus)
	CodecVorbis              // libvorbis (.ogg)
)

func (c Codec) String() string {
	switch c {
	case CodecCopy:
		return "copy"
	case CodecFLAC:
		return "flac"
	case CodecALAC:
		return "alac"
	case CodecWAV:
		return "wav"
	case CodecMP3:
		return "mp3"
	case CodecAAC:
		return "aac"
	case CodecOpus:
		return "opus"
	case CodecVorbis:
		return "vorbis"
	default:
		return fmt.Sprintf("codec(%d)", c)
	}
}

// preset holds the immutable ffmpeg encoding parameters for a Codec.
type preset struct {
	encoder     string   // -c:a value ("copy" for a stream copy)
	extension   string   // canonical output extension without a dot ("" for copy)
	lossless    bool     // true for copy and the lossless re-encoders
	defaultRate int      // default -b:a in bits/sec for lossy codecs (0 if N/A)
	qualityArgs []string // VBR-quality args used for lossy codecs when no bitrate is given
}

// presets is the single source of truth for codec parameters. CodecCopy's
// container follows the source, so it carries no extension.
var presets = map[Codec]preset{
	CodecCopy: {encoder: "copy", lossless: true},
	CodecFLAC: {encoder: "flac", extension: "flac", lossless: true},
	CodecALAC: {encoder: "alac", extension: "m4a", lossless: true},
	// CodecWAV uses pcm_s16le only as a fallback. Runner.Transcode probes the
	// input and passes a depth-matched encoder when the source reports one.
	CodecWAV:    {encoder: "pcm_s16le", extension: "wav", lossless: true},
	CodecMP3:    {encoder: "libmp3lame", extension: "mp3", qualityArgs: []string{"-q:a", "0"}},
	CodecAAC:    {encoder: "aac", extension: "m4a", defaultRate: 256000},
	CodecOpus:   {encoder: "libopus", extension: "opus", defaultRate: 192000},
	CodecVorbis: {encoder: "libvorbis", extension: "ogg", qualityArgs: []string{"-q:a", "6"}},
}

func presetFor(c Codec) (preset, error) {
	p, ok := presets[c]
	if !ok {
		return preset{}, fmt.Errorf("transcode: unknown codec %d", c)
	}
	return p, nil
}

// Extension returns the canonical file extension (without a dot) for c, or "" for
// CodecCopy (whose container follows the source).
func (c Codec) Extension() string { return presets[c].extension }

// IsLossless reports whether c uses a stream copy or a lossless encoder. For
// CodecWAV, Transcode probes the source and picks the PCM encoder with wavEncoder;
// sources that do not report a usable depth use the 16-bit fallback.
func (c Codec) IsLossless() bool { return presets[c].lossless }

// wavEncoder picks the PCM encoder for a WAV transcode. Float PCM stays float;
// integer PCM and lossless integer sources use the closest matching little-endian
// PCM encoder. Unknown depth falls back to 16-bit PCM.
func wavEncoder(s ProbeStream) string {
	base := strings.TrimSuffix(s.SampleFmt, "p") // drop any planar suffix
	// Float WAV files also report bits_per_sample, so handle them before the
	// integer-depth mapping.
	if strings.HasPrefix(s.CodecName, "pcm_") {
		switch base {
		case "flt":
			return "pcm_f32le"
		case "dbl":
			return "pcm_f64le"
		}
	}
	switch depth := s.effectiveBits(); {
	case depth >= 25:
		return "pcm_s32le"
	case depth >= 17:
		return "pcm_s24le"
	case depth > 0:
		return "pcm_s16le"
	}
	// Some decoders expose a 32-bit integer carrier even without a source depth.
	if base == "s32" {
		return "pcm_s32le"
	}
	return "pcm_s16le"
}
