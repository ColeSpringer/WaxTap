// Package loudness measures EBU R128 loudness with WaxFlow's analyzer and derives
// the scalar gain that normalizes a track to a target.
//
// Measurement returns integrated loudness, true peak, loudness range, and sample
// peak (ITU-R BS.1770-4 / EBU R128). Two gain policies derive from it: [GainFor]
// is the closed form of ffmpeg's linear-mode loudnorm, a single gain clamped so
// the true peak stays under the ceiling, and [RawGain] is the same gain
// unclamped, leaving the peaks to WaxFlow's limiter. The gain is handed to the
// media package as Spec.GainDB, fused into the encode.
//
// A measurement is taken at the width the encode delivers: the caller passes
// the fold (see the channels parameter on [Measure] and [MeasureCut]), since a
// lossy encoder folds a source wider than stereo itself and a fold moves both
// the integrated loudness and the true peak.
package loudness

import (
	"context"
	"math"
	"time"

	"github.com/colespringer/waxflow"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
)

// TruePeakCeilingDB is the true-peak ceiling (dBTP) normalization holds under. It
// leaves headroom for inter-sample peaks and matches WaxFlow's limiter default.
const TruePeakCeilingDB = -1.0

// ConvergeToleranceDB is how close to the target the limiter-backed gain search
// must land before it stops re-encoding. EBU R128 tooling reports integrated
// loudness to 0.1 dB, so a miss below this is barely expressible, let alone worth
// another encode pass.
//
// It is exported because the callers that assert on it live outside this package:
// the pipeline runs the search and the facade's peak-mode tests check that a run
// either converges inside it or warns.
const ConvergeToleranceDB = 0.3

// maxGainDB bounds the applied gain to a finite value WaxFlow accepts (its own
// limit is +-120 dB).
const maxGainDB = 120.0

// ClampGain bounds a gain to the range WaxFlow accepts. [GainFor] and [RawGain]
// apply it to their own results; the pipeline's iterative correction needs it
// too, since it walks the gain away from the value they returned.
func ClampGain(gainDB float64) float64 { return clamp(gainDB, -maxGainDB, maxGainDB) }

// Loudness is an EBU R128 measurement.
type Loudness struct {
	IntegratedLUFS float64 // integrated loudness, LUFS (-Inf for silence)
	TruePeakDBTP   float64 // true peak, dBTP (-Inf for silence)
	LRA            float64 // loudness range, LU
	SamplePeakDB   float64 // sample peak, dBFS (-Inf for silence)
	// Duration is how much audio the meter actually read (frames measured over
	// the source rate), 0 when the rate is unknown. It exists so a caller can
	// hold the measurement against the probed length: a decode that ends early
	// on a probe-clean file leaves this as the only evidence.
	Duration time.Duration
	// Warnings is the input damage the measurement's read found, in the
	// source's own terms (media.Result.InputWarnings), complete as of the
	// end of the read; nil for a clean source. A demuxer that walks its
	// payload lazily reports damage past the headers only from the read
	// that reaches it, and a measurement is such a read.
	Warnings []string
}

// Gainable reports whether an integrated loudness can drive a gain. It is the
// guard [GainFor] and [RawGain] both apply, exported so a caller reporting that
// normalization happened tests the same thing the gain functions did: below it
// they return 0 and the encode is a plain transcode.
//
// It is narrower than [Finite] on purpose. Finite also tests the true peak and
// the range, and GainFor returns a real gain when only the true peak is
// non-finite (it just skips the head clamp), so gating on Finite would deny a
// run that did apply gain.
func Gainable(integrated float64) bool { return finite(integrated) }

// finite reports whether v is a usable number: not NaN and not infinite. It is
// the one spelling of the check this package makes everywhere a measurement or
// target has to be trusted before arithmetic.
func finite(v float64) bool { return !math.IsInf(v, 0) && !math.IsNaN(v) }

// Finite reports whether the integrated loudness, true peak, and range are all
// finite. Silence reports -Inf for the loudness and peaks, which cannot seed a
// gain.
func (l Loudness) Finite() bool {
	return finite(l.IntegratedLUFS) && finite(l.TruePeakDBTP) && finite(l.LRA)
}

func fromResult(res *waxflow.AnalyzeResult) Loudness {
	l := Loudness{
		IntegratedLUFS: res.IntegratedLUFS,
		TruePeakDBTP:   res.TruePeakDB,
		LRA:            res.LoudnessRange,
		SamplePeakDB:   res.SamplePeakDB,
	}
	if res.Format.Rate > 0 {
		l.Duration = time.Duration(float64(res.Samples) / float64(res.Format.Rate) * float64(time.Second))
	}
	return l
}

// Measure measures the loudness of a whole local file. channels (1 or 2) folds
// the measurement to a downmix target so the gain matches a downmixing encode; 0
// keeps the source layout.
func Measure(ctx context.Context, r *media.Runner, input string, channels int) (Loudness, error) {
	res, found, err := r.AnalyzeFile(ctx, input, channels)
	if err != nil {
		return Loudness{}, err
	}
	l := fromResult(res)
	l.Warnings = found
	return l, nil
}

// MeasureCut measures the loudness of the cut-composed audio, so the gain matches
// the bytes a fused cut+encode will produce. keeps are the retained spans on the
// source timeline; total is the source duration; channels folds the measurement
// to a downmix target (0 keeps the source layout). sourceSamples is 0 or the
// count a prior media.Runner.MeasureLength(input) delivered, when the source's
// headers only claim a length; see media.CutSpec.SourceSamples.
func MeasureCut(ctx context.Context, r *media.Runner, input string, keeps []cutrange.Range, total, crossfade time.Duration, channels int, sourceSamples int64) (Loudness, error) {
	med, closer, err := r.OpenComposed(ctx, input, keeps, total, crossfade, sourceSamples)
	if err != nil {
		return Loudness{}, err
	}
	defer closer()
	res, err := r.AnalyzeMedia(ctx, med, input, channels)
	if err != nil {
		return Loudness{}, err
	}
	l := fromResult(res)
	l.Warnings = media.InputWarnings(med)
	return l, nil
}

// MeasureAlbum measures a set of tracks as a group and individually, from
// one decode per track: each member at folds[i], the width its own encode
// delivers (media.Runner.PlanEncode; 0 or a short slice keeps the source
// layout), and the album as EBU R128's gates run over every member's blocks
// at once. The album value is not a mean of per-track LUFS, and it is not a
// concatenation either: a concatenation is built at one width, and a mono
// member widened into it reads 3 dB hot. perTrack follows input order.
//
// The members are decoded one after another inside the engine, so an album of
// N tracks costs N serial decodes on one concurrency slot. The Concat pass
// this replaces cost N parallel decodes for the per-track figures plus N more
// for the group, so the wall clock is at worst what the old group pass alone
// cost.
func MeasureAlbum(ctx context.Context, r *media.Runner, inputs []string, folds []int) (album Loudness, perTrack []Loudness, err error) {
	group, members, warnings, err := r.AnalyzeGroup(ctx, inputs, folds)
	if err != nil {
		return Loudness{}, nil, err
	}
	perTrack = make([]Loudness, len(members))
	var total time.Duration
	for i := range members {
		perTrack[i] = fromResult(&members[i])
		// The two slices are built from the same input list and should be the
		// same length; the bound is here so a future engine answering with a
		// different count reports short figures rather than panicking in a
		// caller's process.
		if i < len(warnings) {
			perTrack[i].Warnings = warnings[i]
		}
		total += perTrack[i].Duration
	}
	album = fromResult(group)
	album.Duration = total // the group carries no format, so its length is the members'
	return album, perTrack, nil
}

// GainFor is the closed form of ffmpeg's linear-mode loudnorm: the gain that
// moves the integrated loudness to target, clamped so the true peak stays under
// the ceiling.
//
// Silence (IntegratedLUFS -Inf) yields zero gain, the no-op equivalent: this is a
// guard, not a nicety. target-(-Inf) is +Inf, and WaxFlow rejects a non-finite
// GainDB, so an unguarded silent track would turn a working no-op into an error.
//
// The head-clamp is intentional, not a double-attenuation. When a source already
// peaks above the ceiling, peak protection wins over hitting the exact LUFS,
// exactly as linear-mode loudnorm does. It does not stack with WaxFlow's limiter:
// the limiter engages only for a positive gain, so on an attenuating gain the
// head-clamp is the sole peak control, and on a boosting gain GainFor has already
// clamped under the ceiling. The cost is that a loud source can land well short
// of the target; [RawGain] is the other policy.
func GainFor(target float64, m Loudness) float64 {
	if !Gainable(m.IntegratedLUFS) {
		return 0
	}
	g := target - m.IntegratedLUFS
	if finite(m.TruePeakDBTP) {
		if head := TruePeakCeilingDB - m.TruePeakDBTP; g > head {
			g = head
		}
	}
	return clamp(g, -maxGainDB, maxGainDB)
}

// PeakShortfall reports how many dB of gain the true-peak clamp held back: the
// raw target offset minus the head-clamped gain, or 0 when the clamp did not
// bind (an attenuating or already-headroom-fitting gain).
//
// It is deliberately not "target - integrated minus GainFor". GainFor also bounds
// its result to +-maxGainDB, so that subtraction would attribute a maxGainDB
// clamp to the peak ceiling and report a cause that did not apply. Only the head
// clamp belongs to the peak ceiling, so only it is measured here. The two cannot
// currently both bind on a real measurement (WaxFlow gates integrated loudness at
// -70 LUFS and callers bound the target to -5, so the offset cannot reach 120),
// but the caller states a cause in user-facing text and should not depend on
// three constants staying where they are.
func PeakShortfall(target float64, m Loudness) float64 {
	if !m.Finite() || !finite(target) {
		return 0
	}
	want := target - m.IntegratedLUFS
	head := TruePeakCeilingDB - m.TruePeakDBTP
	if want <= head {
		return 0
	}
	return want - head
}

// AlbumGain is the gain album normalization applies to every track. It is the
// [GainFor] / [RawGain] pair lifted to a whole album: clampPeaks holds the
// loudest track's true peak under the ceiling, which leaves the limiter nothing
// to do and so reproduces the input spacing exactly; without it the gain aims
// straight at the target and the limiter guards the peaks per track, which is
// what pulls louder tracks down harder and compresses that spacing.
//
// The clamp is album-wide, taken from the least headroom across the tracks,
// because a per-track clamp is the one thing album mode cannot do: different
// gains per track are exactly the inter-track differences it exists to preserve.
//
// One consequence is worth stating where users will read it, not only here: a
// single hot master sets the headroom for the whole album, so twelve tracks can
// land 6 dB below target because one of them peaks at 0 dBTP. That is [GainFor]'s
// policy applied album-wide, and it is why the mode is opt-in.
//
// The clamp is only as good as the peaks it is given. [MeasureAlbum] measures
// each track at the width its own encode delivers when the caller passes the
// folds, so the peaks a lossy encoder's fold produces are the peaks clamped
// for; with no folds the measurement is at the source layout and an album the
// encoder then folds can peak higher than was clamped for and re-engage the
// limiter, which is the spacing drift the clamp exists to avoid. The fold is
// reported separately either way (waxtap's implicit-downmix warning).
func AlbumGain(target float64, album Loudness, perTrack []Loudness, clampPeaks bool) float64 {
	// Silence is a no-op in both modes, and the guard has to come before the clamp,
	// not after RawGain alone: a gated-silent album whose tracks still carry a peak
	// over the ceiling would otherwise be attenuated by a clamp with no loudness to
	// protect, and the two modes would disagree on an album neither can move.
	if !Gainable(album.IntegratedLUFS) {
		return 0
	}
	g := RawGain(target, album.IntegratedLUFS)
	if !clampPeaks {
		return g
	}
	if head, ok := albumHeadroom(perTrack); ok && g > head {
		g = head
	}
	return ClampGain(g)
}

// AlbumPeakShortfall reports how many dB of gain the album-wide true-peak clamp
// held back, or 0 when the clamp did not bind. It is [PeakShortfall] for an
// album, and it is deliberately derived from the clamp rather than from the
// achieved loudness, for the reason that function documents.
func AlbumPeakShortfall(target float64, album Loudness, perTrack []Loudness) float64 {
	if !album.Finite() || !finite(target) {
		return 0
	}
	want := target - album.IntegratedLUFS
	head, ok := albumHeadroom(perTrack)
	if !ok || want <= head {
		return 0
	}
	return want - head
}

// albumHeadroom returns the least true-peak headroom across the tracks, and
// whether any track had a peak to measure.
//
// A silent track reports a -Inf true peak, which would make its headroom +Inf and
// silently drop out of a min; it is skipped explicitly instead, the same guard
// [GainFor] applies to its own measurement. An album of nothing but silent tracks
// reports false, so the caller applies no clamp rather than an infinite one.
func albumHeadroom(perTrack []Loudness) (float64, bool) {
	head, ok := math.Inf(1), false
	for _, t := range perTrack {
		if math.IsInf(t.TruePeakDBTP, 0) || math.IsNaN(t.TruePeakDBTP) {
			continue
		}
		if h := TruePeakCeilingDB - t.TruePeakDBTP; h < head {
			head, ok = h, true
		}
	}
	return head, ok
}

// RawGain is the plain target - integrated offset, with no peak clamp: WaxFlow's
// limiter guards the peaks at encode time. It gets far closer to the target than
// GainFor can, at the cost of transparency.
//
// It is the *starting* gain for the PeakLimit path and, through [AlbumGain], the
// *final* gain for a limiting album. The difference is that the limiter gives
// back part of whatever gain it is handed, by an amount that depends on the
// material, so a single RawGain pass lands short. The pipeline measures the
// encode and corrects; album mode cannot, because one uniform gain is the point
// and a per-track correction would destroy the inter-track spacing it exists to
// preserve. A capping album takes [AlbumGain]'s clamp instead, which leaves the
// limiter idle and the spacing intact.
//
// A non-finite integrated loudness (silence) yields zero gain, for the same
// reason GainFor guards it: WaxFlow rejects a non-finite GainDB.
func RawGain(target, integrated float64) float64 {
	if !Gainable(integrated) || !Gainable(target) {
		return 0
	}
	return clamp(target-integrated, -maxGainDB, maxGainDB)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
