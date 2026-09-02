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
	switch ext {
	case "flac":
		return c == "flac"
	case "wav":
		return isPCM
	case "mp3":
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
