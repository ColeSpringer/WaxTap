// Package normalize measures EBU R128 loudness with ffmpeg's loudnorm filter and
// builds the matching apply filter for a later encode.
//
// [Measure] runs an analysis pass and returns integrated loudness, true peak,
// and loudness range without altering the input. [ApplyFilter] uses that
// measurement to build a linear loudnorm filter for transcode.Spec.Filters, so
// normalization can run in the same encode as the format conversion.
//
// The package reports loudness measurements (LUFS, dBTP, LU), not ReplayGain tag
// values. Callers choose the target loudness and handle any tag conversion.
//
// Process execution stays in the transcode package; loudnorm filter syntax and
// JSON parsing live here.
package normalize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/colespringer/waxtap/transcode"
)

// defaultTruePeak is the true-peak ceiling (dBTP) used when applying
// normalization. -1.0 dBTP leaves headroom for inter-sample peaks introduced by
// lossy re-encoding.
const defaultTruePeak = -1.0

// measureFilter is the loudnorm filter used for an analysis-only pass: it prints
// the EBU R128 measurement as JSON to stderr and produces no normalized output.
const measureFilter = "loudnorm=print_format=json"

// passthroughFilter is the no-op audio filter ApplyFilter returns for silent or
// unmeasurable input.
const passthroughFilter = "anull"

// Loudness is an EBU R128 measurement parsed from a loudnorm analysis pass.
type Loudness struct {
	IntegratedLUFS float64 // integrated loudness (loudnorm input_i)
	TruePeakDBTP   float64 // true peak, dBTP (input_tp)
	LRA            float64 // loudness range, LU (input_lra)
	Threshold      float64 // relative gate threshold, LUFS (input_thresh)
	// Offset is loudnorm's target_offset from the analysis pass. It seeds the
	// linear apply pass and is not part of WaxTap's reported measurements.
	Offset float64
}

// Finite reports whether every measured value is finite. loudnorm reports
// non-finite values, such as input_i=-inf and target_offset=inf, for silence and
// other unmeasurable input. Those values cannot be fed back into an apply pass.
func (l Loudness) Finite() bool {
	for _, v := range []float64{l.IntegratedLUFS, l.TruePeakDBTP, l.LRA, l.Threshold, l.Offset} {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return false
		}
	}
	return true
}

// Measure runs a loudnorm analysis pass over input and returns its EBU R128
// measurement. pre lists audio filters applied before the measurement (e.g. cut
// filters), so the measured loudness matches the audio that will be encoded. It
// writes no output file. A positive threads value limits ffmpeg's worker threads;
// zero leaves thread selection to ffmpeg.
func Measure(ctx context.Context, r *transcode.Runner, input string, pre []string, threads int) (Loudness, error) {
	filters := append(append([]string{}, pre...), measureFilter)
	args := []string{"-hide_banner", "-nostdin"}
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	args = append(args,
		"-i", input,
		"-vn", "-map", "0:a:0",
		"-af", strings.Join(filters, ","),
		"-f", "null", "-",
	)
	// Default (info) log level is required: loudnorm prints its JSON at info, so
	// -loglevel error would suppress the very output we parse.
	res, err := r.Run(ctx, transcode.Command{Args: args})
	if err != nil {
		return Loudness{}, err
	}
	l, err := parseLoudness(res.Stderr)
	if err != nil {
		return Loudness{}, err
	}
	return l, nil
}

// MeasureComplex runs a loudnorm analysis pass after a -filter_complex graph.
// graph must read [0:a:0] and write the named sink label; MeasureComplex appends
// loudnorm to that label and maps the result to a null output. It writes no
// output file.
//
// Use it when the pre-processing cannot be expressed as a linear -af chain, such
// as a multi-segment cut whose atrim/concat graph only fits -filter_complex. The
// measured loudness then matches the post-cut audio a fused encode will produce.
// Use Measure for the linear case. A positive threads value limits ffmpeg's
// worker threads; zero leaves thread selection to ffmpeg.
func MeasureComplex(ctx context.Context, r *transcode.Runner, input, graph, sink string, threads int) (Loudness, error) {
	full := graph + ";[" + sink + "]" + measureFilter + "[out]"
	args := []string{"-hide_banner", "-nostdin"}
	if threads > 0 {
		args = append(args, "-threads", strconv.Itoa(threads))
	}
	args = append(args,
		"-i", input,
		"-filter_complex", full,
		"-map", "[out]",
		"-f", "null", "-",
	)
	// As in Measure, the default (info) log level must stay: loudnorm prints its
	// JSON at info, so -loglevel error would suppress the output we parse.
	res, err := r.Run(ctx, transcode.Command{Args: args})
	if err != nil {
		return Loudness{}, err
	}
	return parseLoudness(res.Stderr)
}

// MeasureAlbum measures a set of tracks as a group and individually. The album
// value is the EBU R128 result for the concatenated tracks, including normal
// gating and energy weighting; it is not an average of per-track LUFS. perTrack
// follows input order.
//
// Inputs may differ in sample rate and channel layout; each is conformed to a
// common measurement format before concatenation so the group pass does not fail
// on a layout mismatch.
func MeasureAlbum(ctx context.Context, r *transcode.Runner, inputs []string) (album Loudness, perTrack []Loudness, err error) {
	if len(inputs) == 0 {
		return Loudness{}, nil, fmt.Errorf("normalize: MeasureAlbum: no inputs")
	}
	perTrack = make([]Loudness, len(inputs))
	for i, in := range inputs {
		l, err := Measure(ctx, r, in, nil, 0)
		if err != nil {
			return Loudness{}, nil, fmt.Errorf("normalize: measure track %d: %w", i, err)
		}
		perTrack[i] = l
	}
	album, err = measureConcat(ctx, r, inputs)
	if err != nil {
		return Loudness{}, nil, err
	}
	return album, perTrack, nil
}

// albumMeasureFormat conforms each track to a common sample rate and stereo
// layout before the group measurement, so concat does not fail when tracks differ
// in rate or channel layout. ffmpeg auto-inserts the resampler/rematrix needed to
// satisfy aformat.
const albumMeasureFormat = "aformat=sample_rates=48000:channel_layouts=stereo"

// measureConcat runs one analysis pass over every input concatenated, returning
// the group loudness.
func measureConcat(ctx context.Context, r *transcode.Runner, inputs []string) (Loudness, error) {
	args := []string{"-hide_banner", "-nostdin"}
	var g strings.Builder
	for i := range inputs {
		args = append(args, "-i", inputs[i])
		fmt.Fprintf(&g, "[%d:a:0]%s[a%d];", i, albumMeasureFormat, i)
	}
	for i := range inputs {
		fmt.Fprintf(&g, "[a%d]", i)
	}
	fmt.Fprintf(&g, "concat=n=%d:v=0:a=1[c];[c]%s[out]", len(inputs), measureFilter)
	args = append(args, "-filter_complex", g.String(), "-map", "[out]", "-f", "null", "-")

	res, err := r.Run(ctx, transcode.Command{Args: args})
	if err != nil {
		return Loudness{}, err
	}
	return parseLoudness(res.Stderr)
}

// AlbumGainFilter builds the per-track filter for destructive album
// normalization. Each track receives the same target-albumIntegrated dB offset,
// preserving track-to-track loudness differences across the album. Use
// ApplyFilter for independent track normalization.
//
// It returns anull when either value is not finite (for example a silent album),
// matching ApplyFilter's silent-input behavior.
func AlbumGainFilter(target, albumIntegrated float64) string {
	if math.IsInf(target, 0) || math.IsNaN(target) ||
		math.IsInf(albumIntegrated, 0) || math.IsNaN(albumIntegrated) {
		return passthroughFilter
	}
	return fmt.Sprintf("volume=%sdB", ftoa(target-albumIntegrated))
}

// ApplyFilter builds the loudnorm filter for a normalizing encode, targeting the
// given LUFS value. It uses values from a prior Measure pass and enables
// loudnorm's linear mode. Pass the result through transcode.Spec.Filters to run
// normalization as part of the encode.
//
// When sampleRate contains a positive value, the filter appends aresample to
// preserve that output rate. Without it, loudnorm outputs 192 kHz. The variadic
// parameter preserves compatibility with the original two-argument API; only
// the first value is used.
//
// When the measurement is not finite, ApplyFilter returns anull. That keeps
// silent inputs valid and avoids formatting loudnorm parameters such as
// measured_I=-Inf, which ffmpeg rejects.
func ApplyFilter(target float64, m Loudness, sampleRate ...int) string {
	if !m.Finite() {
		return passthroughFilter
	}
	filter := fmt.Sprintf(
		"loudnorm=I=%s:TP=%s:measured_I=%s:measured_TP=%s:measured_LRA=%s:measured_thresh=%s:offset=%s:linear=true",
		ftoa(target), ftoa(defaultTruePeak),
		ftoa(m.IntegratedLUFS), ftoa(m.TruePeakDBTP), ftoa(m.LRA), ftoa(m.Threshold), ftoa(m.Offset),
	)
	if len(sampleRate) > 0 && sampleRate[0] > 0 {
		filter += ",aresample=" + strconv.Itoa(sampleRate[0])
	}
	return filter
}

// rawLoudness mirrors loudnorm's JSON block, where every value is a string.
type rawLoudness struct {
	InputI       string `json:"input_i"`
	InputTP      string `json:"input_tp"`
	InputLRA     string `json:"input_lra"`
	InputThresh  string `json:"input_thresh"`
	TargetOffset string `json:"target_offset"`
}

func parseLoudness(stderr []byte) (Loudness, error) {
	obj, err := lastJSONObject(stderr)
	if err != nil {
		return Loudness{}, fmt.Errorf("normalize: locate loudnorm json: %w", err)
	}
	var raw rawLoudness
	if err := json.Unmarshal(obj, &raw); err != nil {
		return Loudness{}, fmt.Errorf("normalize: parse loudnorm json: %w", err)
	}
	return Loudness{
		IntegratedLUFS: atof(raw.InputI),
		TruePeakDBTP:   atof(raw.InputTP),
		LRA:            atof(raw.InputLRA),
		Threshold:      atof(raw.InputThresh),
		Offset:         atof(raw.TargetOffset),
	}, nil
}

// lastJSONObject extracts the final brace-delimited object from b. loudnorm's
// JSON is flat (no nested objects) and printed last, after a stream of info
// logging, so scanning from the final '}' back to the nearest '{' isolates it
// even if earlier log lines contained a stray brace.
func lastJSONObject(b []byte) ([]byte, error) {
	end := bytes.LastIndexByte(b, '}')
	if end < 0 {
		return nil, errors.New("no json object found")
	}
	start := bytes.LastIndexByte(b[:end], '{')
	if start < 0 {
		return nil, errors.New("no json object found")
	}
	return b[start : end+1], nil
}

// ftoa formats a loudness value with the precision loudnorm prints.
func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// atof parses a loudnorm string value, tolerating surrounding whitespace and the
// "-inf"/"inf" values loudnorm can report for silence.
func atof(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}
