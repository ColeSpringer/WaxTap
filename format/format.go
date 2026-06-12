// Package format defines WaxTap's stream-format model and the rules for picking
// an audio source from a candidate list.
//
// The package keeps YouTube details at the edge. The youtube package fills
// Format values from player responses, while this package owns comparison and
// selection. A Format is only a candidate; signed media URLs and expiry metadata
// live in youtube.ResolvedStream.
package format

import (
	"fmt"
	"strings"
	"time"
)

// Tri is a three-valued flag for metadata YouTube does not always expose.
// Unknown should stay distinct from Yes and No.
type Tri uint8

const (
	Unknown Tri = iota // the source did not provide the value
	Yes                // the source explicitly reported true
	No                 // the source explicitly reported false
)

func (t Tri) String() string {
	switch t {
	case Yes:
		return "yes"
	case No:
		return "no"
	default:
		return "unknown"
	}
}

// AudioQualityTier identifies the quality tier reported in YouTube's player
// response. Higher values represent higher tiers. QualityUnknown means the
// response omitted the tier or supplied an unrecognized value.
//
// A tier is a selection hint, not an objective comparison of lossy encodings.
type AudioQualityTier uint8

const (
	QualityUnknown  AudioQualityTier = iota // no recognized quality tier
	QualityUltraLow                         // AUDIO_QUALITY_ULTRALOW, distinct from QualityLow
	QualityLow                              // AUDIO_QUALITY_LOW
	QualityMedium                           // AUDIO_QUALITY_MEDIUM
	QualityHigh                             // AUDIO_QUALITY_HIGH
)

func (q AudioQualityTier) String() string {
	switch q {
	case QualityUltraLow:
		return "ultralow"
	case QualityLow:
		return "low"
	case QualityMedium:
		return "medium"
	case QualityHigh:
		return "high"
	default:
		return "unknown"
	}
}

// Format describes a playable stream candidate. Most WaxTap callers deal with
// audio, but video formats can appear in unfiltered player responses and are
// excluded by audio selectors.
type Format struct {
	Itag int // YouTube format identifier

	// Codec / container.
	MIMEType  string // raw mimeType, e.g. `audio/webm; codecs="opus"`
	Codec     string // normalized codec id, e.g. "opus", "mp4a.40.2"
	Extension string // canonical file extension without dot, e.g. "webm", "m4a"

	// Audio characteristics.
	Bitrate        int // declared / peak bits per second
	AverageBitrate int // average bits per second (preferred for comparison)
	SampleRate     int // Hz
	Channels       int // audio channel count

	// AudioQuality is the tier reported by YouTube. QualityUnknown means no
	// usable tier was reported.
	AudioQuality AudioQualityTier

	// Multi-language / dubbed audio metadata.
	Language   string      // audioTrack language tag, "" if single-track
	AudioTrack *AudioTrack // raw audioTrack metadata, nil if none

	// Tri-state quality hints (YouTube is inconsistent about exposing these).
	IsDRC      Tri // dynamic-range-compressed rendition
	IsOriginal Tri // original (non-dubbed) audio

	// Size / length when known (0 == unknown).
	ContentLength int64
	Duration      time.Duration // media duration, or 0 when unknown
}

// AudioTrack holds the raw audioTrack metadata YouTube attaches to dubbed or
// multi-language renditions.
type AudioTrack struct {
	ID          string // YouTube audio-track identifier
	DisplayName string // localized track label
	IsOriginal  Tri    // whether this is the video's original-language track
}

// EffectiveBitrate returns AverageBitrate when known, otherwise Bitrate.
func (f Format) EffectiveBitrate() int {
	if f.AverageBitrate > 0 {
		return f.AverageBitrate
	}
	return f.Bitrate
}

// IsAudio reports whether MIMEType is explicitly audio/*.
func (f Format) IsAudio() bool {
	return strings.HasPrefix(f.MIMEType, "audio/")
}

func (f Format) String() string {
	return fmt.Sprintf("itag=%d codec=%s ext=%s bitrate=%d", f.Itag, f.Codec, f.Extension, f.EffectiveBitrate())
}

// ChannelLayout identifies a preferred channel-count class. LayoutAny, the zero
// value, leaves source selection unchanged.
type ChannelLayout uint8

const (
	LayoutAny      ChannelLayout = iota // no preference (zero value)
	LayoutMono                          // one channel
	LayoutStereo                        // two channels
	LayoutSurround                      // more than two channels
)

func (l ChannelLayout) String() string {
	switch l {
	case LayoutMono:
		return "mono"
	case LayoutStereo:
		return "stereo"
	case LayoutSurround:
		return "surround"
	default:
		return "any"
	}
}

type selectorKind uint8

const (
	selBestAudio selectorKind = iota // zero value: best available audio
	selItag
	selCodec
)

// AudioSelector picks one Format from a candidate list. The zero value selects
// the best available audio (equivalent to BestAudio()).
//
// The selector stores caller intent; concrete selection is performed by the
// extraction/download pipeline.
type AudioSelector struct {
	kind   selectorKind
	itag   int
	codec  string
	layout ChannelLayout
}

// BestAudio selects the best audio stream. It prefers the original track,
// non-DRC audio, higher reported quality tiers, Opus within a tier, and finally
// higher effective bitrate.
func BestAudio() AudioSelector { return AudioSelector{kind: selBestAudio} }

// Itag selects the stream with the exact itag.
func Itag(itag int) AudioSelector { return AudioSelector{kind: selItag, itag: itag} }

// Codec selects the best stream whose codec matches (e.g. "opus", "aac").
func Codec(codec string) AudioSelector { return AudioSelector{kind: selCodec, codec: codec} }

// WithChannels returns a copy of the selector that prefers the given channel
// layout. For BestAudio, the preference ranks below original-language selection
// and above source-policy codec preferences and bitrate. Codec selectors apply
// the layout preference within the matching codec family. Itag selectors ignore
// it. If no candidate matches the layout, normal ranking applies.
func (s AudioSelector) WithChannels(layout ChannelLayout) AudioSelector {
	s.layout = layout
	return s
}

func (s AudioSelector) String() string {
	switch s.kind {
	case selItag:
		return fmt.Sprintf("itag(%d)", s.itag)
	case selCodec:
		return fmt.Sprintf("codec(%s)", s.codec)
	default:
		return "best-audio"
	}
}

type policyKind uint8

const (
	polMinimizeLoss policyKind = iota // zero value: prefer target codec family
	polBestNative
	polPreferCodec
)

// SourcePolicy controls the source-stream tradeoff when a transcode target is
// set. The zero value is MinimizeLoss().
type SourcePolicy struct {
	kind  policyKind
	codec string
}

// MinimizeLoss prefers a source in the target codec family, avoiding a
// cross-codec transcode when possible.
func MinimizeLoss() SourcePolicy { return SourcePolicy{kind: polMinimizeLoss} }

// BestNative ignores target codec matching and uses normal best-audio ranking.
func BestNative() SourcePolicy { return SourcePolicy{kind: polBestNative} }

// PreferCodec prefers a source in the named codec family when policy is active.
func PreferCodec(codec string) SourcePolicy { return SourcePolicy{kind: polPreferCodec, codec: codec} }

func (p SourcePolicy) String() string {
	switch p.kind {
	case polBestNative:
		return "best-native"
	case polPreferCodec:
		return fmt.Sprintf("prefer-codec(%s)", p.codec)
	default:
		return "minimize-loss"
	}
}
