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
		// An itag names an exact encoding, so the layout preference does not apply.
		if i, ok := bestWith(candidates, func(f Format) bool { return eligible(f) && f.Itag == s.itag }, "", LayoutAny); ok {
			return i, nil
		}
		return -1, fmt.Errorf("%w: itag %d", ErrNoMatch, s.itag)
	case selCodec:
		// The codec filter restricts the set; the layout refines within it.
		if i, ok := bestWith(candidates, func(f Format) bool { return eligible(f) && codecMatches(s.codec, f.Codec) }, "", s.layout); ok {
			return i, nil
		}
		return -1, fmt.Errorf("%w: codec %q", ErrNoMatch, s.codec)
	default: // selBestAudio
		return bestForTarget(candidates, policy, target, s.layout)
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
//
// BestForTarget is layout-neutral; the layout-aware path is reached through
// AudioSelector.WithChannels.
func BestForTarget(candidates []Format, policy SourcePolicy, target Target) (int, error) {
	return bestForTarget(candidates, policy, target, LayoutAny)
}

// bestForTarget is BestForTarget with an explicit channel-layout preference.
func bestForTarget(candidates []Format, policy SourcePolicy, target Target, layout ChannelLayout) (int, error) {
	eligible := eligibleAudio(candidates)

	// MinimizeLoss codec matching does not affect copies or lossless targets, but
	// an explicit prefer:<codec> policy must still influence source selection.
	if target.none() || target.Lossless {
		prefCodec := ""
		if policy.kind == polPreferCodec {
			prefCodec = policy.codec
		}
		return pick(candidates, eligible, prefCodec, layout)
	}

	switch policy.kind {
	case polBestNative:
		return pick(candidates, eligible, "", layout)
	case polPreferCodec:
		return pick(candidates, eligible, policy.codec, layout)
	default: // polMinimizeLoss
		return pick(candidates, eligible, target.Codec, layout)
	}
}

// pick returns the best eligible index, or ErrNoMatch when none are eligible.
func pick(candidates []Format, eligible func(Format) bool, prefCodec string, layout ChannelLayout) (int, error) {
	if i, ok := bestWith(candidates, eligible, prefCodec, layout); ok {
		return i, nil
	}
	return -1, ErrNoMatch
}

// bestWith returns the highest-ranked eligible candidate. It decides whether to
// use quality tiers once so the comparator remains consistent.
func bestWith(c []Format, keep func(Format) bool, prefCodec string, layout ChannelLayout) (int, bool) {
	return bestAmong(c, keep, betterThan(prefCodec, layout, tierUsable(c, keep, prefCodec, layout)))
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
//  2. exact requested channel layout, when one was requested
//  3. ability to downmix to a mono or stereo request
//  4. preferred codec family, when one was requested
//  5. non-DRC audio
//  6. fewer channels among sources downmixable to mono or stereo
//  7. higher reported quality tier, when useTier is true
//  8. Opus over other codecs, when useTier is true
//  9. higher effective bitrate
//
// Original-language selection takes precedence over layout, so a dubbed track
// cannot win only because it matches the requested layout. If tier metadata is
// incomplete, tier and Opus preference are skipped and bitrate decides between
// candidates otherwise tied on the earlier rules. Equal candidates retain their
// original order.
func betterThan(prefCodec string, layout ChannelLayout, useTier bool) func(a, b Format) bool {
	return func(a, b Format) bool {
		if ra, rb := originalRank(a.IsOriginal), originalRank(b.IsOriginal); ra != rb {
			return ra > rb
		}
		if layout != LayoutAny {
			if am, bm := channelMatches(layout, a.Channels), channelMatches(layout, b.Channels); am != bm {
				return am // a matches the requested layout and b does not
			}
			// Prefer a source that can be downmixed to the requested layout over
			// one that would require upmixing.
			if ra, rb := downmixRank(layout, a.Channels), downmixRank(layout, b.Channels); ra != rb {
				return ra > rb
			}
		}
		if prefCodec != "" {
			if am, bm := codecMatches(prefCodec, a.Codec), codecMatches(prefCodec, b.Codec); am != bm {
				return am // a matches the preferred codec and b does not
			}
		}
		if ra, rb := nonDRCRank(a.IsDRC), nonDRCRank(b.IsDRC); ra != rb {
			return ra > rb
		}
		// Among otherwise equal downmixable sources, prefer fewer channels.
		if layout != LayoutAny {
			if ra, rb := fewerChannelsRank(layout, a.Channels), fewerChannelsRank(layout, b.Channels); ra != rb {
				return ra > rb
			}
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

// layoutTarget returns the fixed channel count for mono and stereo requests.
// Other layouts return 0 because they do not use downmix ranking.
func layoutTarget(layout ChannelLayout) int {
	switch layout {
	case LayoutMono:
		return 1
	case LayoutStereo:
		return 2
	default:
		return 0
	}
}

// downmixRank ranks sources by whether they can satisfy a mono or stereo
// request without upmixing. A downmixable source ranks above one below the
// requested count, and a known count ranks above an unknown count.
func downmixRank(layout ChannelLayout, ch int) int {
	target := layoutTarget(layout)
	switch {
	case target == 0:
		return 0
	case ch <= 0:
		return -2
	case ch < target:
		return -1
	default:
		return 0
	}
}

// fewerChannelsRank prefers fewer channels among sources that can satisfy a mono
// or stereo request without upmixing. Other sources tie because downmixRank orders
// them first. betterThan and tierUsable share both ranks to keep their orderings
// aligned.
func fewerChannelsRank(layout ChannelLayout, ch int) int {
	if target := layoutTarget(layout); target == 0 || ch < target {
		return 0
	}
	return -ch
}

// channelMatches reports whether a stream's channel count satisfies layout.
// Unknown counts and LayoutAny do not match, leaving the layout ranking step
// inactive when there is no usable preference.
func channelMatches(layout ChannelLayout, ch int) bool {
	if ch <= 0 {
		return false
	}
	switch layout {
	case LayoutMono:
		return ch == 1
	case LayoutStereo:
		return ch == 2
	case LayoutSurround:
		return ch > 2
	default: // LayoutAny
		return false
	}
}

// codecPreferenceRank prefers Opus when candidates share a reported tier.
func codecPreferenceRank(codec string) int {
	if codecFamily(codec) == "opus" {
		return 1
	}
	return 0
}

// tierUsable reports whether every eligible candidate tied on all higher-priority
// criteria has a known quality tier. Its key mirrors betterThan through
// fewerChannelsRank; lower-ranked candidates do not affect the decision.
func tierUsable(c []Format, keep func(Format) bool, prefCodec string, layout ChannelLayout) bool {
	key := func(f Format) [6]int {
		lm := 0
		if channelMatches(layout, f.Channels) {
			lm = 1
		}
		cm := 0
		if prefCodec != "" && codecMatches(prefCodec, f.Codec) {
			cm = 1
		}
		return [6]int{originalRank(f.IsOriginal), lm, downmixRank(layout, f.Channels), cm, nonDRCRank(f.IsDRC), fewerChannelsRank(layout, f.Channels)}
	}
	var best [6]int
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
func lessKey(a, b [6]int) bool {
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
