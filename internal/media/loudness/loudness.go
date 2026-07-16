// Package loudness measures EBU R128 loudness with WaxFlow's analyzer and derives
// the scalar gain that normalizes a track to a target.
//
// Measurement returns integrated loudness, true peak, loudness range, and sample
// peak (ITU-R BS.1770-4 / EBU R128). [GainFor] is the closed form of ffmpeg's
// linear-mode loudnorm: a single gain, clamped so the true peak stays under the
// ceiling. The gain is handed to the media package as TranscodeOptions.GainDB,
// fused into the encode.
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

// maxGainDB bounds the applied gain to a finite value WaxFlow accepts (its own
// limit is +-120 dB).
const maxGainDB = 120.0

// Loudness is an EBU R128 measurement.
type Loudness struct {
	IntegratedLUFS float64 // integrated loudness, LUFS (-Inf for silence)
	TruePeakDBTP   float64 // true peak, dBTP (-Inf for silence)
	LRA            float64 // loudness range, LU
	SamplePeakDB   float64 // sample peak, dBFS (-Inf for silence)
}

// Finite reports whether the integrated loudness, true peak, and range are all
// finite. Silence reports -Inf for the loudness and peaks, which cannot seed a
// gain.
func (l Loudness) Finite() bool {
	for _, v := range []float64{l.IntegratedLUFS, l.TruePeakDBTP, l.LRA} {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return false
		}
	}
	return true
}

func fromResult(res *waxflow.AnalyzeResult) Loudness {
	return Loudness{
		IntegratedLUFS: res.IntegratedLUFS,
		TruePeakDBTP:   res.TruePeakDB,
		LRA:            res.LoudnessRange,
		SamplePeakDB:   res.SamplePeakDB,
	}
}

// Measure measures the loudness of a whole local file. channels (1 or 2) folds
// the measurement to a downmix target so the gain matches a downmixing encode; 0
// keeps the source layout.
func Measure(ctx context.Context, r *media.Runner, input string, channels int) (Loudness, error) {
	res, err := r.AnalyzeFile(ctx, input, channels)
	if err != nil {
		return Loudness{}, err
	}
	return fromResult(res), nil
}

// MeasureCut measures the loudness of the cut-composed audio, so the gain matches
// the bytes a fused cut+encode will produce. keeps are the retained spans on the
// source timeline; total is the source duration; channels folds the measurement
// to a downmix target (0 keeps the source layout).
func MeasureCut(ctx context.Context, r *media.Runner, input string, keeps []cutrange.Range, total, crossfade time.Duration, channels int) (Loudness, error) {
	med, closer, err := r.OpenComposed(input, keeps, total, crossfade)
	if err != nil {
		return Loudness{}, err
	}
	defer closer()
	res, err := r.AnalyzeMedia(ctx, med, channels)
	if err != nil {
		return Loudness{}, err
	}
	return fromResult(res), nil
}

// MeasureAlbum measures a set of tracks as a group and individually. The album
// value is the EBU R128 result for the concatenated tracks, including gating and
// energy weighting; it is not a mean of per-track LUFS. perTrack follows input
// order.
func MeasureAlbum(ctx context.Context, r *media.Runner, inputs []string) (album Loudness, perTrack []Loudness, err error) {
	// Album measurement never downmixes; each track is measured at its own layout.
	perTrack = make([]Loudness, len(inputs))
	for i, in := range inputs {
		res, aerr := r.AnalyzeFile(ctx, in, 0)
		if aerr != nil {
			return Loudness{}, nil, aerr
		}
		perTrack[i] = fromResult(res)
	}
	med, closer, oerr := r.OpenAlbumConcat(inputs)
	if oerr != nil {
		return Loudness{}, nil, oerr
	}
	defer closer()
	ares, merr := r.AnalyzeMedia(ctx, med, 0)
	if merr != nil {
		return Loudness{}, nil, merr
	}
	return fromResult(ares), perTrack, nil
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
// clamped under the ceiling.
func GainFor(target float64, m Loudness) float64 {
	if math.IsInf(m.IntegratedLUFS, 0) || math.IsNaN(m.IntegratedLUFS) {
		return 0
	}
	g := target - m.IntegratedLUFS
	if !math.IsInf(m.TruePeakDBTP, 0) && !math.IsNaN(m.TruePeakDBTP) {
		if head := TruePeakCeilingDB - m.TruePeakDBTP; g > head {
			g = head
		}
	}
	return clamp(g, -maxGainDB, maxGainDB)
}

// AlbumGain is the raw target - albumIntegrated offset applied to every track,
// preserving inter-track loudness differences. Unlike GainFor it is not
// peak-clamped: WaxFlow's limiter guards each track's peaks at encode time. A
// silent album (non-finite integrated) yields zero gain.
func AlbumGain(target, albumIntegrated float64) float64 {
	if math.IsInf(albumIntegrated, 0) || math.IsNaN(albumIntegrated) ||
		math.IsInf(target, 0) || math.IsNaN(target) {
		return 0
	}
	return clamp(target-albumIntegrated, -maxGainDB, maxGainDB)
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
