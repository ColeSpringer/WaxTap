package media

import (
	"fmt"
	"slices"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
)

// Short-decode detection: a decode (or measurement) that delivers materially
// less audio than the source declares, from a file the probe passed clean.
// Mid-file corruption has exactly this shape - the byte count and headers are
// intact, so probe warnings stay empty, and the decoder reaching the bad frame
// is the first and only evidence.
//
// The threshold is absolute on purpose. Every legitimate declared-vs-decoded
// drift is bounded in absolute terms, not proportional ones: encoder framing
// (priming, padding, final-frame rounding) stays within tens of milliseconds
// whatever the length, and even a container's advisory duration is
// millisecond-granular. A relative floor would only silence real losses on
// long audio, where five missing seconds of an audiobook are no less missing
// for being under two percent of it.
const shortDecodeMinShortfall = 500 * time.Millisecond

// shortDecode reports whether got falls materially short of declared, past
// what the engine counted of that shortfall as truncation: audio the header
// declares and the file does not hold is a shortfall with its own number and
// its own line, so only what that leaves unexplained is a decode that ended
// early. Unknown durations (either side zero or negative) never trip: no claim
// without both numbers.
//
// counted is the truncation declared still includes, which is not always the
// read's whole count: see Loss.TruncatedPast. A negative one explains nothing.
func shortDecode(got, declared, counted time.Duration) bool {
	if got <= 0 || declared <= 0 {
		return false
	}
	return declared-got-max(counted, 0) > shortDecodeMinShortfall
}

// ShortDecodeNote returns a damage note when a rendered output holds materially
// less audio than the source declared, or "" when the lengths agree. It is the
// one wording every rendering path (pipeline output, album track) uses.
// counted is how much of declared the read counted as truncated, 0 when
// declared already has it out; see shortDecode.
func ShortDecodeNote(got, declared, counted time.Duration) string {
	if !shortDecode(got, declared, counted) {
		return ""
	}
	return fmt.Sprintf("the decode ended at %s of a declared %s; the remainder of the file did not read",
		got.Round(10*time.Millisecond), declared.Round(10*time.Millisecond))
}

// ShortMeasureNote is ShortDecodeNote for a measure-only run, where the meter's
// frame count stands in for an output file: a measurement covering part of a
// track is reported as exactly that.
func ShortMeasureNote(got, declared, counted time.Duration) string {
	if !shortDecode(got, declared, counted) {
		return ""
	}
	return fmt.Sprintf("the measurement covered %s of a declared %s; the remainder of the file did not read",
		got.Round(10*time.Millisecond), declared.Round(10*time.Millisecond))
}

// Loss is the engine's two counts of what a read's source lost, neither of
// which a length shows. A hole keeps the stream's length, with silence where
// the audio was (a FLAC, native or in Ogg, numbers its frames, so the engine
// knows the size of the gap), and a file cut short has its length restated to
// what it holds. Both are at Rate, on the timeline of the result they came
// from (waxflow.TranscodeResult, waxflow.AnalyzeResult): an encode that
// resamples states them at the output's rate.
//
// The zero value is a source that lost nothing by either count. That is also
// what damage that shortens the read reports, which is every hole outside
// FLAC; the short-decode check above is the one that finds those.
type Loss struct {
	// LostSamples counts the delivered samples that are silence standing in
	// for audio damage took.
	LostSamples int64
	// TruncatedSamples counts the samples the source's header declares past
	// the end of what the file holds. None of them was delivered.
	TruncatedSamples int64
	Rate             int
}

// Counted reports whether the source lost anything by either count.
func (l Loss) Counted() bool { return l.LostSamples > 0 || l.TruncatedSamples > 0 }

// Lost is how much of the delivered audio is silence where damage took it, 0
// when the rate is unknown.
func (l Loss) Lost() time.Duration { return l.time(l.LostSamples) }

// Truncated is how far the audio ends short of the length the source
// declares, 0 when the rate is unknown.
func (l Loss) Truncated() time.Duration { return l.time(l.TruncatedSamples) }

func (l Loss) time(n int64) time.Duration {
	if n <= 0 || l.Rate <= 0 {
		return 0
	}
	return SampleTime(n, l.Rate)
}

// TruncatedPast is how much of this read's truncation a length that already
// has settled's taken out of it still includes. It is what a short-decode
// check may credit against the length it holds the read to, and the two cases
// are opposite. A length opening the file restated (a FLAC, a WAV, a
// progressive MP4) already excludes the truncation, which settled counts in
// full, so nothing is left to credit and a read that ends short of it ended
// early. A length the headers only claimed (an MP3 on its Xing count, a
// fragmented MP4 on its index) still includes all of it, settled counts none,
// and the whole count is credited. settled is the probe's Loss, or a prior
// measurement's when that is what restated the length. Never negative: a cut
// that stops short of the source's end counts no truncation of its own.
func (l Loss) TruncatedPast(settled Loss) time.Duration {
	return max(l.Truncated()-settled.Truncated(), 0)
}

// Material reports whether the source is missing more audio than a short
// decode is allowed to come up short by, the two counts together: past it a
// read is of part of a track, however whole its length looks.
func (l Loss) Material() bool { return l.Lost()+l.Truncated() > shortDecodeMinShortfall }

// Note renders WaxFlow's one-line loss warning for these counts, or "" for a
// source that lost nothing: how much of the audio is silence standing in for
// a hole, and how far it ends short of the length its source declares. The
// wording is WaxFlow's own, reconstructed through its LossNote the way
// Levels.Note takes LevelNote's, so the two never drift.
func (l Loss) Note() string {
	r := waxflow.TranscodeResult{
		Format:           audio.Format{Rate: l.Rate},
		LostSamples:      l.LostSamples,
		TruncatedSamples: l.TruncatedSamples,
	}
	return r.LossNote()
}

// Lead puts the loss line (Note) at the head of a read's damage notes, or
// returns them untouched for a source that lost nothing. It leads because it
// is the line with the totals and callers cap the list they render: a FLAC
// damaged in a dozen places lists a finding per hole, and the sum is what says
// how much of the file is missing. notes is not modified.
func (l Loss) Lead(notes []string) []string {
	note := l.Note()
	if note == "" || slices.Contains(notes, note) {
		return notes
	}
	return append([]string{note}, notes...)
}

// lossOf extracts the loss counts from an engine transcode result.
func lossOf(r *waxflow.TranscodeResult) Loss {
	return Loss{LostSamples: r.LostSamples, TruncatedSamples: r.TruncatedSamples, Rate: r.Format.Rate}
}

// AnalysisLoss extracts the loss counts from an engine measurement. A group's
// own result carries its members' totals and no rate to time them at.
func AnalysisLoss(r *waxflow.AnalyzeResult) Loss {
	return Loss{LostSamples: r.LostSamples, TruncatedSamples: r.TruncatedSamples, Rate: r.Format.Rate}
}
