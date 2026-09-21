package media

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// inferableContainers are output extensions that name a container WaxTap
// recognizes, so the extension constrains the output rather than being
// force-muxed over. An extension outside this set (a codec name like ".alac",
// an unrelated name like ".out", or none at all) does not constrain the codec:
// the container comes from the format instead, and the write is force-muxed.
//
// Every one of these is a container WaxTap can produce. The containers it can
// only read are decodeOnlyContainers, consulted beside this table so an output
// named for one is refused rather than force-muxed.
//
// Dropped versus the ffmpeg era: ".w64" and ".caf" have no WaxFlow muxer (".m4a"
// covers ".caf"'s ALAC).
var inferableContainers = map[string]bool{
	"mp3": true, "flac": true, "wav": true, "m4a": true, "m4b": true,
	"mp4": true, "aac": true, "ogg": true, "oga": true, "opus": true,
	"webm": true, "mka": true, "mkv": true,
	"aiff": true, "aif": true, "aifc": true, "afc": true,
	"wv": true, "ape": true,
	// WaxFlow's wav row's own spellings, and its mp3 row's. Its RIFF muxer
	// writes the RF64 form only past 4 GiB, so a small file under either
	// name is a plain RIFF, which every RF64 reader accepts by chunk id.
	// Listed here so an output named with one is constrained by its name
	// rather than force-muxed under it, which is how out.rf64 collected
	// FLAC bytes.
	"wave": true, "rf64": true, "bw64": true, "mpga": true,
}

// decodeOnlyContainers maps every extension WaxFlow registers for a container
// it demuxes but never muxes (WMA in ASF, Musepack) to the container's name.
// Nothing WaxTap writes may carry these names: FLAC bytes under out.mpp would
// be a file every player reads as Musepack. All the engine's spellings are
// here, the legacy ones included, because the refusal is about what a name
// promises rather than how common the spelling is; which spellings a directory
// walk claims unasked is audioExts' separate choice.
//
// WaxFlow exports no per-driver extension list, so this is kept by hand against
// its format/registry.go. The other extension-keyed tables (needsForcedMuxer,
// ContainerAccepts, the batch planner's extPossiblyCodec, the facade's
// pictureCapableExt) consult it rather than restating it, and their tests walk
// DecodeOnlyExts.
var decodeOnlyContainers = map[string]string{
	"wma": "WMA", "asf": "WMA",
	"mpc": "Musepack", "mp+": "Musepack", "mpp": "Musepack",
}

// DecodeOnlyContainer reports whether ext (with or without a leading dot, any
// case) names a container WaxTap can only read, and that container's name.
func DecodeOnlyContainer(ext string) (string, bool) {
	name, ok := decodeOnlyContainers[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return name, ok
}

// DecodeOnlyExts lists the decode-only extensions, undotted and sorted, for the
// tests that pin the other extension-keyed tables to this one.
func DecodeOnlyExts() []string {
	return slices.Sorted(maps.Keys(decodeOnlyContainers))
}

// IsAIFFExt reports whether ext names an AIFF container. WaxFlow's aiff row
// registers four spellings for one container; .aifc/.afc name the
// compressed-capable variant, which the muxer selects from the sample format
// rather than the filename.
//
// The list lives here rather than in each table because the output extension is
// what picks between PCM's two containers, so a spelling missed by one table gets
// RIFF bytes in an AIFF file. ext must be lowercased and undotted, which every
// caller already guarantees.
func IsAIFFExt(ext string) bool {
	switch ext {
	case "aiff", "aif", "aifc", "afc":
		return true
	}
	return false
}

// IsWAVExt reports whether ext names a RIFF WAV container. WaxFlow's wav row
// registers four spellings for one container; .rf64/.bw64 name the 64-bit
// variants, which its muxer writes only past 4 GiB, so a smaller file under
// either name is a plain RIFF and every reader of those formats accepts it by
// chunk id rather than by filename.
//
// The list lives here for the reason IsAIFFExt's does: the output extension
// picks between PCM's two containers, so a spelling missed by one table gets
// the wrong bytes under the right name. ext must be lowercased and undotted,
// which every caller already guarantees.
func IsWAVExt(ext string) bool {
	switch ext {
	case "wav", "wave", "rf64", "bw64":
		return true
	}
	return false
}

// needsForcedMuxer reports whether the output path does not name a container
// WaxTap can infer, so the container comes from the format rather than the
// filename.
func needsForcedMuxer(output string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	if _, decodeOnly := decodeOnlyContainers[ext]; decodeOnly {
		return false // the name constrains the output: refused, not muxed over
	}
	return !inferableContainers[ext]
}

// ContainerAccepts reports whether the container named by ext can hold the given
// codec unchanged (a copy/remux). Some extensions support several codecs, so this
// consults a compatibility table rather than comparing names; unknown extensions
// are permissive. codecName may be a codecName-mapped value ("opus", "aac",
// "pcm", "wav", "aiff").
//
// PCM has two container-defining names: "wav" and "aiff" are Codec.String()
// values naming a container, while "pcm" is what a probe reports for a source.
// isPCM covers RIFF and probed sources only; AIFF has its own branch, and
// Matroska takes PCM through WaxFlow's wav row.
func ContainerAccepts(ext, codecName string) bool {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if _, decodeOnly := decodeOnlyContainers[ext]; decodeOnly {
		return false // nothing WaxTap writes may carry the name
	}
	c := strings.ToLower(codecName)
	// HE-AAC lives in exactly the AAC family's containers (WaxFlow's he-aac row
	// shares the aac row's container set by design), so one fold here answers
	// for every arm instead of an "|| he-aac" in each.
	if c == "he-aac" {
		c = "aac"
	}
	isPCM := c == "wav" || strings.HasPrefix(c, "pcm")
	if IsAIFFExt(ext) {
		// Ahead of the switch so all four spellings share one answer. Not isPCM:
		// "wav" means CodecWAV, which is RIFF and cannot go in an AIFF file.
		return c == "aiff" || strings.HasPrefix(c, "pcm")
	}
	if IsWAVExt(ext) {
		// The same, for the wav row's four spellings.
		return isPCM
	}
	switch ext {
	case "flac":
		return c == "flac"
	case "mp3", "mpga":
		return c == "mp3"
	case "m4a", "mp4", "m4b":
		return c == "aac" || c == "alac"
	case "aac":
		// .aac selects the raw ADTS stream, which carries the AAC family only
		// (not ALAC). HE-AAC rides it with implicit signalling; WaxFlow declines
		// the one shape ADTS cannot legally carry (downsampled SBR) at plan time.
		return c == "aac"
	case "ogg", "oga":
		return c == "vorbis" || c == "opus" || c == "flac"
	case "opus":
		return c == "opus"
	case "webm":
		return c == "opus" || c == "vorbis"
	case "mka", "mkv":
		// Matroska carries the codecs WaxFlow can mux into it. MP3 and ALAC have no
		// Matroska form in WaxFlow (mp3 has no alternate container; alac only maps to
		// progressive MP4), so they are excluded to keep this in step with the engine.
		// WavPack has a Matroska form in the wild (A_WAVPACK4) but WaxFlow does not
		// write it, so it is excluded for the same reason.
		switch c {
		case "opus", "vorbis", "aac", "flac":
			return true
		}
		return isPCM
	case "wv":
		return c == "wavpack"
	case "ape":
		return c == "ape"
	}
	return true
}

// ContainersFor returns a short list of conventional container extensions, each
// with a leading dot, that can hold codecName unchanged. The result is a subset
// of the extensions ContainerAccepts allows. Unknown codecs return nil.
func ContainersFor(codecName string) []string {
	c := strings.ToLower(codecName)
	if c == "he-aac" {
		c = "aac" // one container family; see ContainerAccepts
	}
	switch {
	case c == "flac":
		return []string{".flac", ".mka"}
	case c == "wav":
		return []string{".wav", ".mka"}
	case c == "aiff":
		return []string{".aiff", ".aif"}
	case strings.HasPrefix(c, "pcm"):
		// A probed source stream, which any of the three can hold.
		return []string{".wav", ".aiff", ".mka"}
	case c == "mp3":
		return []string{".mp3"}
	case c == "aac":
		return []string{".m4a", ".aac", ".mka"}
	case c == "alac":
		return []string{".m4a"}
	case c == "opus":
		return []string{".opus", ".webm", ".ogg", ".mka"}
	case c == "vorbis":
		return []string{".ogg", ".webm", ".mka"}
	case c == "wavpack":
		return []string{".wv"}
	case c == "ape":
		return []string{".ape"}
	}
	return nil
}

// CheckOutputContainer reports whether output's extension can hold codec. Copy
// passes (its container follows the source), as does an extensionless or
// codec-named output (force-muxed, so the extension does not constrain it). A
// recognized extension that cannot hold codec returns waxerr.ErrIncompatibleSpec
// with suggested containers.
func CheckOutputContainer(codec Codec, output string) error {
	if codec == CodecCopy || needsForcedMuxer(output) {
		return nil
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	if name, decodeOnly := decodeOnlyContainers[ext]; decodeOnly {
		return fmt.Errorf("%w: cannot write a .%s file: WaxTap reads %s but does not write it; use one of %s",
			waxerr.ErrIncompatibleSpec, ext, name, strings.Join(ContainersFor(codec.String()), ", "))
	}
	if ContainerAccepts(ext, codec.String()) {
		return nil
	}
	return fmt.Errorf("%w: the output extension .%s cannot hold %s audio; use one of %s",
		waxerr.ErrIncompatibleSpec, ext, codec, strings.Join(ContainersFor(codec.String()), ", "))
}

// ContainerCodec returns the encoder a container extension usually takes when
// nothing else names one: the codec a file of that name is expected to hold.
// It reports false for an extension that names no container WaxTap writes.
func ContainerCodec(ext string) (Codec, bool) {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if IsAIFFExt(ext) {
		return CodecAIFF, true
	}
	if IsWAVExt(ext) {
		return CodecWAV, true
	}
	switch ext {
	case "flac":
		return CodecFLAC, true
	case "mp3", "mpga":
		return CodecMP3, true
	case "m4a", "mp4", "m4b", "aac":
		return CodecAAC, true
	case "ogg", "oga":
		return CodecVorbis, true
	case "opus":
		return CodecOpus, true
	case "webm", "mka", "mkv":
		return CodecOpus, true
	case "wv":
		return CodecWavPack, true
	case "ape":
		return CodecAPE, true
	}
	return CodecCopy, false
}

// SourceFamilyCodec maps a probed codec name to the encoder of its own family,
// for a request that keeps the source codec but has to re-encode (a downmix, a
// gain). outExt picks between PCM's two containers. It reports false for a
// codec WaxTap has no encoder for.
func SourceFamilyCodec(name, outExt string) (Codec, bool) {
	switch strings.ToLower(name) {
	case "opus":
		return CodecOpus, true
	case "aac":
		return CodecAAC, true
	case "he-aac":
		return CodecHEAAC, true
	case "vorbis":
		return CodecVorbis, true
	case "mp3":
		return CodecMP3, true
	case "flac":
		return CodecFLAC, true
	case "alac":
		return CodecALAC, true
	case "wavpack":
		return CodecWavPack, true
	case "ape":
		return CodecAPE, true
	}
	if strings.HasPrefix(strings.ToLower(name), "pcm") {
		if IsAIFFExt(strings.ToLower(strings.TrimPrefix(outExt, "."))) {
			return CodecAIFF, true
		}
		return CodecWAV, true
	}
	return CodecCopy, false
}

// SourceMatches reports whether an encode to target would deliver the codec
// the source already has, so a copy is that encode with nothing lost: the
// source's own family encoder is target (SourceFamilyCodec, with outExt
// picking PCM's container), or target is AAC-LC and the source is HE-AAC,
// which WaxFlow copies under its own identity (its aac remux redirects to
// the he-aac row). The reverse, an HE-AAC target on an AAC-LC source, is a
// real encode. An unknown source ("") matches nothing, and neither does a
// copy target, which has no encoder to compare against.
func SourceMatches(sourceCodec string, target Codec, outExt string) bool {
	if sourceCodec == "" || target == CodecCopy {
		return false
	}
	if target == CodecAAC && strings.EqualFold(sourceCodec, "he-aac") {
		return true
	}
	c, ok := SourceFamilyCodec(sourceCodec, outExt)
	return ok && c == target
}

// OutputCodecFor is the container rule for an output the request named by
// extension alone: the codec the output takes and whether it is the source's
// own.
//
// A format-named extension (.flac, .mp3, .opus, .wav, .aiff, .wv, .ape) names
// its format. A container that holds several codecs (.ogg/.oga, .mka/.mkv,
// .webm, .mp4/.m4a/.m4b, .aac) keeps sourceCodec when it can carry it, else
// takes the container's usual encoder (ContainerCodec). sourceCodec is a probe
// name ("opus", "pcm", ...); "" means unknown, which keeps nothing.
//
// An extension WaxTap does not write is an error naming the choices; a
// decode-only source codec into a container that cannot hold it falls to the
// container's encoder like any other.
func OutputCodecFor(ext, sourceCodec string) (c Codec, kept bool, err error) {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	if name, decodeOnly := DecodeOnlyContainer(ext); decodeOnly {
		return CodecCopy, false, fmt.Errorf("%w: cannot write a .%s file: WaxTap reads %s but does not write it", waxerr.ErrIncompatibleSpec, ext, name)
	}
	container, ok := ContainerCodec(ext)
	if !ok {
		return CodecCopy, false, fmt.Errorf("%w: .%s names no container WaxTap writes (choose one of %s)",
			waxerr.ErrIncompatibleSpec, ext, strings.Join(OutputContainerExts(), ", "))
	}
	if sourceCodec != "" && ContainerAccepts(ext, sourceCodec) {
		if c, ok := SourceFamilyCodec(sourceCodec, ext); ok {
			return c, true, nil
		}
	}
	return container, false, nil
}

// OutputContainerExts lists the container extensions WaxTap writes, dotted and
// sorted, for a refusal that names the choices.
func OutputContainerExts() []string {
	out := make([]string, 0, len(inferableContainers))
	for ext := range inferableContainers {
		out = append(out, "."+ext)
	}
	slices.Sort(out)
	return out
}

// containerDropsTrims reports whether a WaxFlow output container accepts a
// gapless trim and then states none, so a copy into it plays the source's
// encoder delay and padding as audio. Raw ADTS is the one: it has no field
// for a trim or a length, and its muxer discards the trailer rather than
// refuse it. Every other trim-less container WaxTap writes (FLAC, WAV, AIFF,
// WavPack, APE) refuses a nonzero trim outright, and a lossless source
// carries none, so a copy into those never reaches this question.
func containerDropsTrims(container string) bool { return container == "adts" }
