package media

import (
	"fmt"
	"strings"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
)

// Codec identifies an output audio encoding. CodecCopy rewrites the source
// packets into a new container without re-encoding (a remux). The lossless
// codecs (FLAC, ALAC, WAV, AIFF) decode and encode; they are not stream copies.
type Codec uint8

const (
	CodecCopy    Codec = iota // container rewrite / remux (the only no-re-encode path)
	CodecFLAC                 // lossless re-encode (.flac)
	CodecALAC                 // lossless re-encode (Apple Lossless in .m4a)
	CodecWAV                  // lossless PCM (.wav)
	CodecMP3                  // MP3, CBR 320 by default
	CodecAAC                  // AAC-LC in .m4a
	CodecOpus                 // Opus (.opus)
	CodecVorbis               // Vorbis (.ogg)
	CodecAIFF                 // lossless PCM (.aiff)
	CodecHEAAC                // HE-AAC v1 (SBR) in .m4a
	CodecWavPack              // lossless re-encode (.wv)
	CodecAPE                  // lossless re-encode (Monkey's Audio, .ape)
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
	case CodecAIFF:
		return "aiff"
	case CodecHEAAC:
		return "he-aac"
	case CodecWavPack:
		return "wavpack"
	case CodecAPE:
		return "ape"
	default:
		return fmt.Sprintf("codec(%d)", c)
	}
}

// Default lossy bitrates in bits per second, matching the pre-WaxFlow presets.
// MP3 is constant bit rate: LAME -V0 has no scalar equivalent under WaxFlow's
// MP3Bitrate anchor, so WaxTap encodes CBR 320 for predictable sizes.
const (
	defaultMP3Bitrate  = 320000
	defaultAACBitrate  = 256000
	defaultOpusBitrate = 192000
	// defaultHEAACBitrate is the classic HE-AAC v1 stereo operating point,
	// WaxFlow's own zero-value default, spelled out here so WaxTap's presets
	// stay explicit. WaxTap encodes v1 only.
	defaultHEAACBitrate = 64000
	// defaultVorbisQuality mirrors the old libvorbis -q:a 6 preset.
	defaultVorbisQuality = 6.0
)

// codecFormat maps a Codec to WaxFlow's output format name. CodecCopy has none;
// it is served by Engine.Remux.
func codecFormat(c Codec) (string, bool) {
	switch c {
	case CodecFLAC:
		return "flac", true
	case CodecALAC:
		return "alac", true
	case CodecWAV:
		return "wav", true
	case CodecMP3:
		return "mp3", true
	case CodecAAC:
		return "aac", true
	case CodecOpus:
		return "opus", true
	case CodecVorbis:
		return "vorbis", true
	case CodecAIFF:
		return "aiff", true
	case CodecHEAAC:
		return "he-aac", true
	case CodecWavPack:
		return "wavpack", true
	case CodecAPE:
		return "ape", true
	default:
		return "", false
	}
}

// Extension returns the canonical file extension (without a dot) for c, or "" for
// CodecCopy (whose container follows the source).
func (c Codec) Extension() string {
	switch c {
	case CodecFLAC:
		return "flac"
	case CodecALAC, CodecAAC, CodecHEAAC:
		return "m4a"
	case CodecWAV:
		return "wav"
	case CodecMP3:
		return "mp3"
	case CodecOpus:
		return "opus"
	case CodecVorbis:
		return "ogg"
	case CodecAIFF:
		return "aiff"
	case CodecWavPack:
		return "wv"
	case CodecAPE:
		return "ape"
	default:
		return ""
	}
}

// IsLossless reports whether c is a remux or a lossless encoder (FLAC, ALAC,
// WAV, AIFF, WavPack, APE). They keep the source bit depth unless Spec.BitDepth
// asks for a specific one, so by default they never narrow a higher-depth
// source. (WavPack and APE hold integer PCM only, so a float decode quantizes
// to 24 bits, which carries the whole float32 mantissa.)
func (c Codec) IsLossless() bool {
	switch c {
	case CodecCopy, CodecFLAC, CodecALAC, CodecWAV, CodecAIFF, CodecWavPack, CodecAPE:
		return true
	default:
		return false
	}
}

// encodeOptions builds the WaxFlow TranscodeOptions for an encoding spec. The
// transcode path routes CodecCopy to Engine.Remux, so a copy never reaches here.
// Tags is never set: every output's metadata comes from the WaxLabel
// post-pass on the finished file, which also carries pictures.
func encodeOptions(spec Spec) waxflow.TranscodeOptions {
	format, _ := codecFormat(spec.Codec)
	opts := waxflow.TranscodeOptions{
		Format:   format,
		Channels: spec.Channels, // 0 keeps the source layout; 1 or 2 downmixes
		GainDB:   spec.GainDB,   // 0 is a no-op; positive engages the true-peak limiter
		// 0 follows the decoded stream, so a lossy source yields float output.
		// WaxFlow dithers a narrowing conversion (TPDF) rather than truncating; the
		// lossy rows zero it in their adjust hooks, so it reaches wav/aiff/flac/alac.
		BitDepth: spec.BitDepth,
	}
	switch spec.Codec {
	case CodecMP3:
		opts.MP3Bitrate = defaultMP3Bitrate
		if spec.Bitrate > 0 {
			opts.MP3Bitrate = spec.Bitrate
		}
	case CodecAAC:
		opts.AACBitrate = defaultAACBitrate
		if spec.Bitrate > 0 {
			opts.AACBitrate = spec.Bitrate
		}
	case CodecHEAAC:
		opts.AACBitrate = defaultHEAACBitrate
		if spec.Bitrate > 0 {
			opts.AACBitrate = spec.Bitrate
		}
	case CodecOpus:
		opts.OpusBitrate = defaultOpusBitrate
		if spec.Bitrate > 0 {
			opts.OpusBitrate = spec.Bitrate
		}
	case CodecVorbis:
		// Vorbis is quality-driven; WaxFlow has no Vorbis ABR, so spec.Bitrate is
		// ignored (the CLI notes this) and the encode uses a fixed quality.
		opts.VorbisQuality = defaultVorbisQuality
	}
	return opts
}

// decodeOnlyCodecs maps the probe name of every codec WaxFlow decodes but does
// not encode to its display name. A copy of one has no output row to run as
// and a cut has no same-family encoder to fall back to, so the refusals that
// meet one name the reason and the escape. TestDecoderRegistryParity pins the
// table to the engine's decoder list.
var decodeOnlyCodecs = map[string]string{"wma": "WMA", "musepack": "Musepack"}

// DecodeOnlyCodec reports whether name (a probe codec name) is one WaxFlow
// only decodes, and its display name.
func DecodeOnlyCodec(name string) (string, bool) {
	display, ok := decodeOnlyCodecs[strings.ToLower(name)]
	return display, ok
}

// codecName maps a WaxFlow codec ID to the ffprobe-style name WaxTap's
// compatibility tables and public Format.Codec speak. Only AAC-LC needs
// translation ("aac-lc" -> "aac"); every other ID already matches ("he-aac",
// "wavpack", "ape", "wma", "musepack" included), and an unknown ID passes through so a
// container check fails cleanly rather than crashing.
func codecName(id codec.ID) string {
	if id == codec.AACLC {
		return "aac"
	}
	return string(id)
}
