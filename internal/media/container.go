package media

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// inferableContainers are output extensions that name a container WaxTap can
// produce. Extensions outside this set (codec names like ".alac", or unrelated
// names like ".out") are not usable as a copy target: a re-encode picks the
// container from the codec preset instead.
//
// Dropped versus the ffmpeg era: ".w64" and ".caf" have no WaxFlow muxer (".m4a"
// covers ".caf"'s ALAC).
var inferableContainers = map[string]bool{
	"mp3": true, "flac": true, "wav": true, "m4a": true, "m4b": true,
	"mp4": true, "aac": true, "ogg": true, "oga": true, "opus": true,
	"webm": true, "mka": true, "mkv": true,
	"aiff": true, "aif": true,
}

// needsForcedMuxer reports whether the output path does not name a container
// WaxTap can infer, so a copy has no container to target.
func needsForcedMuxer(output string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), "."))
	return !inferableContainers[ext]
}

// CanInferContainer reports whether path's extension names a container WaxTap can
// produce. When false (extensionless or codec-named paths such as ".alac"), a
// stream copy has no container to write into; callers encode instead.
func CanInferContainer(path string) bool {
	return !needsForcedMuxer(path)
}

// ContainerAccepts reports whether the container named by ext can hold the given
// codec unchanged (a copy/remux). Some extensions support several codecs, so this
// consults a compatibility table rather than comparing names; unknown extensions
// are permissive. codecName may be a codecName-mapped value ("opus", "aac",
// "pcm", "wav"); every PCM branch accepts both "pcm" and "wav".
func ContainerAccepts(ext, codecName string) bool {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	c := strings.ToLower(codecName)
	isPCM := c == "wav" || strings.HasPrefix(c, "pcm")
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
		// .aac selects the raw ADTS stream, which carries AAC only (not ALAC).
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
		switch c {
		case "opus", "vorbis", "aac", "flac":
			return true
		}
		return isPCM
	}
	return true
}

// ContainersFor returns a short list of conventional container extensions, each
// with a leading dot, that can hold codecName unchanged. The result is a subset
// of the extensions ContainerAccepts allows. Unknown codecs return nil.
func ContainersFor(codecName string) []string {
	c := strings.ToLower(codecName)
	switch {
	case c == "flac":
		return []string{".flac", ".mka"}
	case c == "wav" || strings.HasPrefix(c, "pcm"):
		return []string{".wav", ".mka"}
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
	if ContainerAccepts(ext, codec.String()) {
		return nil
	}
	return fmt.Errorf("%w: the output extension .%s cannot hold %s audio; use one of %s",
		waxerr.ErrIncompatibleSpec, ext, codec, strings.Join(ContainersFor(codec.String()), ", "))
}
