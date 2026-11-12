package waxtap

import (
	"time"

	"github.com/colespringer/waxtap/v3/internal/media"
)

// InputLoss counts the audio a local input is missing in the two ways a
// length does not show. It is the numbers behind the sentence a
// [WarnInputDamage] detail leads with, for a caller that computes with them:
// the detail is worded for a person, and its wording is the engine's to change.
//
// Both counts are WaxFlow's own, taken by the read that delivered or measured
// the audio. A result carries a nil *InputLoss when both are zero. That is not
// a certificate of health: damage that shortens a read instead (a lost Ogg
// page takes its packets with it) is in neither count, and is reported by the
// warning alone.
type InputLoss struct {
	// LostSamples counts the samples that are silence standing in for audio
	// damage took. A FLAC, native or in Ogg, numbers its frames, so a read
	// knows the size of a hole and fills it: the audio keeps its length, and
	// what follows the hole stays where the stream puts it. Zero for every
	// other format.
	LostSamples int64
	// TruncatedSamples counts the samples the input's header declares past
	// the end of what the file holds: what a file cut short lost off its end.
	// None of them was delivered or measured. Zero for a whole file, for a
	// header that states no length in samples to fall short of (raw ADTS,
	// ASF, Matroska), and for a cut or a split piece that stops short of the
	// input's end, which holds everything it asked for.
	TruncatedSamples int64
	// SampleRate is the rate both counts are at: that of the file the run
	// wrote, so an encode that resamples states them at its output's rate, a
	// track of an album and a piece of a split included, or the input's own
	// for a measurement, which writes none. The WarnInputDamage detail beside
	// a result states the same counts at the same rate.
	SampleRate int
}

// Lost is how much of the audio is silence where damage took it: LostSamples
// at SampleRate, 0 when the rate is unknown.
func (l InputLoss) Lost() time.Duration { return l.loss().Lost() }

// Truncated is how far the audio ends short of the length the input declares:
// TruncatedSamples at SampleRate, 0 when the rate is unknown.
func (l InputLoss) Truncated() time.Duration { return l.loss().Truncated() }

func (l InputLoss) loss() media.Loss {
	return media.Loss{LostSamples: l.LostSamples, TruncatedSamples: l.TruncatedSamples, Rate: l.SampleRate}
}

// inputLossOf is the engine's count as a result carries it: nil for a source
// that lost nothing by either count, so a caller tests one pointer.
func inputLossOf(l media.Loss) *InputLoss {
	if !l.Counted() {
		return nil
	}
	return &InputLoss{LostSamples: l.LostSamples, TruncatedSamples: l.TruncatedSamples, SampleRate: l.Rate}
}
