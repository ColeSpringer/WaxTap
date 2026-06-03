// Package format models audio (and incidental video) stream formats and
// provides audio-first selection over them. It is YouTube-agnostic in shape:
// the youtube package populates Format values from a player response, the facade
// surfaces them, and the selectors here choose among candidates.
//
// A Format is only a candidate. It does not carry a signed URL or expiry; those
// live in youtube.ResolvedStream. This keeps "what audio is available" separate
// from "how to fetch it right now".
package format

import (
	"fmt"
	"time"
)

// Tri is a three-valued flag for metadata YouTube does not always expose.
// Unknown should stay distinct from Yes and No.
type Tri uint8

const (
	Unknown Tri = iota
	Yes
	No
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

// Format describes a single playable audio rendition (audio-first; incidental
// video formats may also be represented but are not the focus).
type Format struct {
	Itag int

	// Codec / container.
	MIMEType  string // raw mimeType, e.g. `audio/webm; codecs="opus"`
	Codec     string // normalized codec id, e.g. "opus", "mp4a.40.2"
	Extension string // canonical file extension without dot, e.g. "webm", "m4a"

	// Audio characteristics.
	Bitrate        int // declared / peak bits per second
	AverageBitrate int // average bits per second (preferred for comparison)
	SampleRate     int // Hz
	Channels       int

	// Multi-language / dubbed audio metadata.
	Language   string      // audioTrack language tag, "" if single-track
	AudioTrack *AudioTrack // raw audioTrack metadata, nil if none

	// Tri-state quality hints (YouTube is inconsistent about exposing these).
	IsDRC      Tri // dynamic-range-compressed rendition
	IsOriginal Tri // original (non-dubbed) audio

	// Size / length when known (0 == unknown).
	ContentLength int64
	Duration      time.Duration
}

// AudioTrack holds the raw audioTrack metadata YouTube attaches to dubbed or
// multi-language renditions.
type AudioTrack struct {
	ID          string
	DisplayName string
	IsOriginal  Tri
}

// EffectiveBitrate returns AverageBitrate when known, else the declared
// Bitrate. It is the value selectors compare on.
func (f Format) EffectiveBitrate() int {
	if f.AverageBitrate > 0 {
		return f.AverageBitrate
	}
	return f.Bitrate
}

func (f Format) String() string {
	return fmt.Sprintf("itag=%d codec=%s ext=%s bitrate=%d", f.Itag, f.Codec, f.Extension, f.EffectiveBitrate())
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
	kind  selectorKind
	itag  int
	codec string
}

// BestAudio selects the highest-quality audio-only stream (preferring non-DRC,
// original audio, then highest effective bitrate).
func BestAudio() AudioSelector { return AudioSelector{kind: selBestAudio} }

// Itag selects the stream with the exact itag.
func Itag(itag int) AudioSelector { return AudioSelector{kind: selItag, itag: itag} }

// Codec selects the best stream whose codec matches (e.g. "opus", "aac").
func Codec(codec string) AudioSelector { return AudioSelector{kind: selCodec, codec: codec} }

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
	polMinimizeLoss policyKind = iota // zero value: minimize generational loss
	polBestNative
	polPreferCodec
)

// SourcePolicy controls the source-stream tradeoff when a transcode target is
// set. The zero value is MinimizeLoss().
type SourcePolicy struct {
	kind  policyKind
	codec string
}

// MinimizeLoss prefers a source whose codec is copy-compatible with the target
// to avoid generational loss (e.g. pick AAC for an AAC target).
func MinimizeLoss() SourcePolicy { return SourcePolicy{kind: polMinimizeLoss} }

// BestNative always picks the highest-bitrate source, then transcodes.
func BestNative() SourcePolicy { return SourcePolicy{kind: polBestNative} }

// PreferCodec biases source selection toward a specific codec.
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
