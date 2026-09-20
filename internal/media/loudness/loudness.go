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
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colespringer/waxflow"

	"github.com/colespringer/waxtap/v3/internal/cutrange"
	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/waxerr"
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

// MeasureAlbum measures a set of tracks as a group and individually. The album
// value is the EBU R128 result for the concatenated tracks, including gating and
// energy weighting; it is not a mean of per-track LUFS. perTrack follows input
// order.
//
// folds[i], when 1 or 2, folds track i's measurement to that width, the width
// the album's encode delivers for it (media.Runner.PlanOutputChannels); 0, or a
// nil slice, keeps the source layout. The folds name one width, and every
// member left unfolded is at most that wide: a lossy row folds every source
// wider than what it can hold to the same count and leaves the rest alone.
// widths[i] is track i's source channel count, which the caller has already
// probed; 0, or a short slice, means it could not be read, which counts as a
// width of its own and keeps such a set off the fold-once shape below.
//
// The group pass measures the album at the widths the encode delivers, and how
// depends on the set:
//
//   - Nothing folds: the Concat at source widths. A narrower member is placed
//     into the widest layout at unity with its missing positions silent, which
//     is loudness-neutral under BS.1770. The one exception is a mono member,
//     which is duplicated across the front pair and measures about 3 dB up in
//     the group; that predates this and is not new here.
//   - Every member folds, from one source width: the Concat folded once, by the
//     measurement. That fold is every member's own, and it is the raw mix with
//     no limiter (waxflow.AnalyzeOptions.Channels), the same fold the per-track
//     pass takes, so the group and the tracks agree on hot material.
//   - Anything else, the realistic case being a surround member folded to
//     stereo for a lossy target beside a stereo one: the timeline is built at
//     the fold's width (waxflow.ConcatOptions.Channels), so each wider member
//     is folded by its own chain before it meets its siblings and a narrower
//     one is placed as above, and the measurement folds nothing. The order is
//     the point: a fold applied to the assembled timeline would not be the
//     member's own, because the mixer normalizes each output row by the energy
//     of every source coefficient, silent positions included, so a 5.1 member
//     widened to 7.1 and then folded to stereo lands about 1 dB under its
//     direct fold. The fold inside a member's chain is the delivery fold,
//     limiter included, which is what the encode meters for that member.
func MeasureAlbum(ctx context.Context, r *media.Runner, inputs []string, folds, widths []int) (album Loudness, perTrack []Loudness, err error) {
	build, fold, err := groupPass(inputs, folds, widths)
	if err != nil {
		return Loudness{}, nil, err
	}
	perTrack, measured, err := measureTracks(ctx, r, inputs, folds)
	if err != nil {
		return Loudness{}, nil, err
	}

	med, closer, oerr := r.OpenAlbumConcat(ctx, inputs, measured, build)
	if oerr != nil {
		return Loudness{}, nil, oerr
	}
	defer closer()
	// The group read's own damage list is the members' again, each under a
	// member index; the per-track measurements above already carry them.
	//
	// "" names no single file: a concatenated album has several.
	ares, merr := r.AnalyzeMedia(ctx, med, "", fold)
	if merr != nil {
		return Loudness{}, nil, merr
	}
	return fromResult(ares), perTrack, nil
}

// measureTracks runs the per-track pass and returns each track's measurement
// with the frame count its read delivered. The counts are what the group
// timeline needs for a member whose headers state its length only
// approximately; see media.Runner.OpenAlbumConcat.
//
// The tracks are measured across the runner's own concurrency budget rather
// than one after another. Each is a full decode of a separate file and they
// share nothing, so an album of N tracks was N decodes deep on one core while
// the runner stood ready to admit Concurrency of them; it is the dominant cost
// of measuring an album. The runner bounds the engine work either way, so what
// the fan-out adds is the descriptors and buffers of the calls in flight,
// which is the same budget the caller already set.
//
// A failure stops the dispatch, not the reads already running. The album is
// measured to derive one gain, so once a member cannot be read there is no
// album figure and the tracks not yet started are wasted work; the ones in
// flight are already paid for, and letting them finish is what keeps the
// answer stable. Cancelling them instead would record a cancellation against
// a track that was itself about to fail, and the reported failure would then
// depend on which decode lost the race.
//
// The error reported is the lowest-numbered track's, which is the one a pass
// down the list would have hit first. That is the global one: indexes go out
// in order, so every track left undispatched sits above every track that ran.
func measureTracks(ctx context.Context, r *media.Runner, inputs []string, folds []int) ([]Loudness, []int64, error) {
	perTrack := make([]Loudness, len(inputs))
	measured := make([]int64, len(inputs))
	errs := make([]error, len(inputs))

	var failed atomic.Bool
	next := make(chan int)
	go func() {
		defer close(next)
		for i := range inputs {
			if failed.Load() {
				return
			}
			select {
			case next <- i:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range min(r.Concurrency(), len(inputs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The loop runs to the end of the channel even after a failure:
			// a worker that returned early could leave the dispatch with no
			// reader and the last index never sent.
			for i := range next {
				res, found, aerr := r.AnalyzeFile(ctx, inputs[i], foldAt(folds, i))
				if aerr != nil {
					errs[i] = aerr
					failed.Store(true)
					continue
				}
				perTrack[i] = fromResult(res)
				perTrack[i].Warnings = found
				measured[i] = res.Samples
			}
		}()
	}
	wg.Wait()

	for i, aerr := range errs {
		if aerr != nil {
			// The album has many inputs, so the failure names its file, the
			// way a timeline error is named after its member.
			return nil, nil, fmt.Errorf("track %s: %w", filepath.Base(inputs[i]), aerr)
		}
	}
	return perTrack, measured, nil
}

// groupPass decides how the group measurement runs: build is the width the
// timeline is built at (0 keeps the envelope) and fold the width the
// measurement folds to (0 keeps the timeline's). See MeasureAlbum for the
// three shapes.
//
// It holds the caller to the one-width rule rather than trusting it. Two
// different fold widths cannot come from one encode, and an unfolded member
// wider than the fold delivers a second width that one timeline cannot carry:
// built at the fold it would fold a member whose encode does not, and built
// wider it would place the folded ones. Both are refused as a spec conflict
// naming the track, since a timeline error names members by index and this
// one never reaches the timeline.
//
// Neither shape is reachable today, and the reason is not in this package:
// every encoder folds each source above its cap to the same count, so one
// codec and one spec name one width (media.TestPlanOutputChannelsCapsEvery
// CodecAtOneWidth pins that, and is where a bump that stopped it would fail).
// The refusals stay because the alternative to them is not a wrong error but
// a silent one: an album gain derived from a member measured at a width its
// own encode never delivers describes no file that was written.
func groupPass(inputs []string, folds, widths []int) (build, fold int, err error) {
	foldOf := func(i int) int { return foldAt(folds, i) }
	widthOf := func(i int) int {
		if i < len(widths) {
			return widths[i]
		}
		return 0
	}
	width, allFold, oneWidth := 0, len(inputs) > 0, true
	for i := range inputs {
		switch f := foldOf(i); {
		case f <= 0:
			allFold = false
		case width == 0:
			width = f
		case f != width:
			return 0, 0, fmt.Errorf("track %s: %w: folds to %s where the album folds to %s; one encode delivers one width",
				filepath.Base(inputs[i]), waxerr.ErrIncompatibleSpec, channelCount(f), channelCount(width))
		}
		if w := widthOf(i); w <= 0 || w != widthOf(0) {
			oneWidth = false
		}
	}
	if width == 0 {
		return 0, 0, nil
	}
	for i := range inputs {
		if w := widthOf(i); foldOf(i) <= 0 && w > width {
			return 0, 0, fmt.Errorf("track %s: %w: delivers %s beside a fold to %s; the album's members do not share a delivered width",
				filepath.Base(inputs[i]), waxerr.ErrIncompatibleSpec, channelCount(w), channelCount(width))
		}
	}
	if allFold && oneWidth {
		return 0, width, nil
	}
	return width, 0, nil
}

// channelCount names a width the way a refusal has to read: a mono fold is
// "1 channel", not "1 channels".
func channelCount(n int) string {
	if n == 1 {
		return "1 channel"
	}
	return fmt.Sprintf("%d channels", n)
}

// foldAt is track i's fold width, the one reading both passes take: a slice too
// short to reach i, and any entry that is not a width, mean the track keeps its
// source layout. MeasureAlbum's contract is 1 or 2, and a caller outside it
// must not have the group and the tracks disagree about what it asked for.
func foldAt(folds []int, i int) int {
	if i < len(folds) && folds[i] > 0 {
		return folds[i]
	}
	return 0
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
