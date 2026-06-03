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
// writes no output file.
func Measure(ctx context.Context, r *transcode.Runner, input string, pre []string) (Loudness, error) {
	filters := append(append([]string{}, pre...), measureFilter)
	args := []string{
		"-hide_banner", "-nostdin",
		"-i", input,
		"-vn", "-map", "0:a:0",
		"-af", strings.Join(filters, ","),
		"-f", "null", "-",
	}
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

// ApplyFilter builds the loudnorm filter for a normalizing encode, targeting the
// given LUFS value. It uses values from a prior Measure pass and enables
// loudnorm's linear mode. Pass the result through transcode.Spec.Filters to run
// normalization as part of the encode.
//
// When the measurement is not finite, ApplyFilter returns anull. That keeps
// silent inputs valid and avoids formatting loudnorm parameters such as
// measured_I=-Inf, which ffmpeg rejects.
func ApplyFilter(target float64, m Loudness) string {
	if !m.Finite() {
		return passthroughFilter
	}
	return fmt.Sprintf(
		"loudnorm=I=%s:TP=%s:measured_I=%s:measured_TP=%s:measured_LRA=%s:measured_thresh=%s:offset=%s:linear=true",
		ftoa(target), ftoa(defaultTruePeak),
		ftoa(m.IntegratedLUFS), ftoa(m.TruePeakDBTP), ftoa(m.LRA), ftoa(m.Threshold), ftoa(m.Offset),
	)
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
