package format

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoMatch reports that selection found no audio candidate satisfying the
// request. Selector-specific errors wrap it, so callers should check with
// errors.Is.
var ErrNoMatch = errors.New("format: no matching audio format")

// Target describes the transcode output BestForTarget is selecting for. A zero
// Target means keep or copy the source, so normal best-audio ranking applies.
type Target struct {
	// Codec is the normalized output codec id, such as "aac" or "opus".
	// MinimizeLoss uses it only when a YouTube source may exist in the same
	// family. Leave it empty for targets with no native source equivalent, such
	// as MP3.
	Codec string
	// Lossless is true for FLAC, ALAC, and WAV targets. Since these preserve the
	// decoded samples, source codec matching does not help.
	Lossless bool
}

// none reports the zero Target.
func (t Target) none() bool { return t.Codec == "" && !t.Lossless }

// Select resolves an AudioSelector against candidates and returns the chosen
// index. The index matters because an itag can repeat across language and DRC
// variants.
//
// Itag and Codec first filter to matching audio candidates, then use normal
// audio ranking within that set. BestAudio, including the zero selector,
// delegates to BestForTarget so SourcePolicy and Target can influence the
// source. Select returns ErrNoMatch when no audio candidate satisfies the
// selector.
func (s AudioSelector) Select(candidates []Format, policy SourcePolicy, target Target) (int, error) {
	eligible := eligibleAudio(candidates)
	switch s.kind {
	case selItag:
		if i, ok := bestWith(candidates, func(f Format) bool { return eligible(f) && f.Itag == s.itag }, ""); ok {
			return i, nil
		}
		return -1, fmt.Errorf("%w: itag %d", ErrNoMatch, s.itag)
	case selCodec:
		if i, ok := bestWith(candidates, func(f Format) bool { return eligible(f) && codecMatches(s.codec, f.Codec) }, ""); ok {
			return i, nil
		}
		return -1, fmt.Errorf("%w: codec %q", ErrNoMatch, s.codec)
	default: // selBestAudio
		return BestForTarget(candidates, policy, target)
	}
}

// BestForTarget chooses the source audio for a transcode target and returns its
// index in candidates. It is the BestAudio path:
//
//   - zero Target or a lossless target: use normal best-audio ranking
//   - MinimizeLoss: prefer a source in the target codec family
//   - BestNative: ignore the target codec and use normal best-audio ranking
//   - PreferCodec: prefer the policy codec family
//
// Codec preference sits below original-track selection, so it never chooses a
// dubbed track over the original for codec compatibility. When no candidate
// matches the preferred codec, selection falls back to normal ranking. It
// returns ErrNoMatch only when there are no selectable audio candidates.
func BestForTarget(candidates []Format, policy SourcePolicy, target Target) (int, error) {
	eligible := eligibleAudio(candidates)

	// MinimizeLoss codec matching does not affect copies or lossless targets, but
	// an explicit prefer:<codec> policy must still influence source selection.
	if target.none() || target.Lossless {
		prefCodec := ""
		if policy.kind == polPreferCodec {
			prefCodec = policy.codec
		}
		return pick(candidates, eligible, prefCodec)
	}

	switch policy.kind {
	case polBestNative:
		return pick(candidates, eligible, "")
	case polPreferCodec:
		return pick(candidates, eligible, policy.codec)
	default: // polMinimizeLoss
		return pick(candidates, eligible, target.Codec)
	}
}

// pick returns the best eligible index, or ErrNoMatch when none are eligible.
func pick(candidates []Format, eligible func(Format) bool, prefCodec string) (int, error) {
	if i, ok := bestWith(candidates, eligible, prefCodec); ok {
		return i, nil
	}
	return -1, ErrNoMatch
}

// bestWith returns the highest-ranked eligible candidate. It decides whether to
// use quality tiers once so the comparator remains consistent.
func bestWith(c []Format, keep func(Format) bool, prefCodec string) (int, bool) {
	return bestAmong(c, keep, betterThan(prefCodec, tierUsable(c, keep, prefCodec)))
}

// bestAmong returns the highest-ranked candidate kept by keep. Ties resolve to
// the earliest index so selection is deterministic.
func bestAmong(candidates []Format, keep func(Format) bool, better func(a, b Format) bool) (idx int, ok bool) {
	best := -1
	for i := range candidates {
		if !keep(candidates[i]) {
			continue
		}
		if best < 0 || better(candidates[i], candidates[best]) {
			best = i
		}
	}
	return best, best >= 0
}

// betterThan builds the audio-ranking comparator. Candidates are ranked by:
//
//  1. original track, because the wrong language is a content error
//  2. preferred codec family, when one was requested
//  3. non-DRC audio
//  4. higher reported quality tier, when useTier is true
//  5. Opus over other codecs, when useTier is true
//  6. higher effective bitrate
//
// If tier metadata is incomplete, tier and Opus preference are skipped and
// bitrate decides between candidates otherwise tied on the first three rules.
// Equal candidates retain their original order.
func betterThan(prefCodec string, useTier bool) func(a, b Format) bool {
	return func(a, b Format) bool {
		if ra, rb := originalRank(a.IsOriginal), originalRank(b.IsOriginal); ra != rb {
			return ra > rb
		}
		if prefCodec != "" {
			if am, bm := codecMatches(prefCodec, a.Codec), codecMatches(prefCodec, b.Codec); am != bm {
				return am // a matches the preferred codec and b does not
			}
		}
		if ra, rb := nonDRCRank(a.IsDRC), nonDRCRank(b.IsDRC); ra != rb {
			return ra > rb
		}
		if useTier {
			if ra, rb := int(a.AudioQuality), int(b.AudioQuality); ra != rb {
				return ra > rb
			}
			if ra, rb := codecPreferenceRank(a.Codec), codecPreferenceRank(b.Codec); ra != rb {
				return ra > rb
			}
		}
		return a.EffectiveBitrate() > b.EffectiveBitrate()
	}
}

// codecPreferenceRank prefers Opus when candidates share a reported tier.
func codecPreferenceRank(codec string) int {
	if codecFamily(codec) == "opus" {
		return 1
	}
	return 0
}

// tierUsable reports whether every eligible candidate tied on original track,
// requested codec, and DRC status has a known quality tier. Lower-ranked
// candidates do not affect the decision.
func tierUsable(c []Format, keep func(Format) bool, prefCodec string) bool {
	key := func(f Format) [3]int {
		m := 0
		if prefCodec != "" && codecMatches(prefCodec, f.Codec) {
			m = 1
		}
		return [3]int{originalRank(f.IsOriginal), m, nonDRCRank(f.IsDRC)}
	}
	var best [3]int
	have := false
	for i := range c {
		if !keep(c[i]) {
			continue
		}
		if k := key(c[i]); !have || lessKey(best, k) {
			best, have = k, true
		}
	}
	if !have {
		return false
	}
	for i := range c {
		if keep(c[i]) && key(c[i]) == best && c[i].AudioQuality == QualityUnknown {
			return false
		}
	}
	return true
}

// lessKey compares ranking keys lexicographically.
func lessKey(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// originalRank sorts known-original tracks first and known dubs last. Unknown
// stays between them because older player responses may not label the default
// track.
func originalRank(t Tri) int {
	switch t {
	case Yes:
		return 2
	case No:
		return 0
	default:
		return 1
	}
}

// nonDRCRank sorts full-dynamic-range audio ahead of DRC variants. Unknown stays
// between them because the flag is not always present.
func nonDRCRank(t Tri) int {
	switch t {
	case No:
		return 2
	case Yes:
		return 0
	default:
		return 1
	}
}

// eligibleAudio returns a predicate for audio selection. If any candidate is
// explicitly audio/*, only audio/* formats are eligible. Otherwise, unlabeled
// formats remain eligible but explicit video/* formats do not, which keeps old
// incomplete metadata working without selecting a video-only stream.
func eligibleAudio(candidates []Format) func(Format) bool {
	for i := range candidates {
		if candidates[i].IsAudio() {
			return Format.IsAudio
		}
	}
	return func(f Format) bool { return !isVideo(f) }
}

// isVideo reports whether MIMEType is explicitly video/*.
func isVideo(f Format) bool {
	return strings.HasPrefix(f.MIMEType, "video/")
}

// codecMatches reports whether two codec ids belong to the same codec family.
// Matching is family-based because output codecs and source codec ids are often
// spelled differently, as with "aac" and "mp4a.40.2".
func codecMatches(want, have string) bool {
	wf := codecFamily(want)
	return wf != "" && wf == codecFamily(have)
}

// codecFamily normalizes a codec id, container name, or user-facing alias to a
// coarse family. Unknown codecs pass through unchanged so exact ids still match.
func codecFamily(codec string) string {
	c := strings.ToLower(strings.TrimSpace(codec))
	switch {
	case c == "":
		return ""
	case strings.HasPrefix(c, "opus"):
		return "opus"
	case strings.HasPrefix(c, "vorbis"):
		return "vorbis"
	case strings.HasPrefix(c, "flac"):
		return "flac"
	case strings.HasPrefix(c, "mp4a"), c == "aac", c == "m4a", c == "mp4":
		return "aac"
	case strings.HasPrefix(c, "mp3"), c == "mpeg":
		return "mp3"
	default:
		return c
	}
}
