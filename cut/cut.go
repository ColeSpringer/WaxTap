// Package cut removes time ranges from an audio file.
//
// Range math is kept pure in ranges.go. [RangesFromSegments] converts
// SponsorBlock segments into removal ranges, and [Render] applies the resulting
// cut in one of three modes:
//
//   - Copy: stream-copy the retained spans and concatenate them. No re-encode,
//     but boundaries snap to packet/frame edges. Some containers, notably raw
//     FLAC, keep a stale duration header after a copy even though the audio
//     is trimmed correctly; accurate mode avoids that.
//   - Accurate: trim sample-exactly and re-encode through a -filter_complex
//     graph, so boundaries are exact. Required for a crossfade.
//   - Smart (default): copy when no encode is involved, accurate when a
//     transcode, filter, or crossfade is requested.
//
// Cut builds the copy commands and filter graphs; transcode owns ffmpeg
// execution.
package cut

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/colespringer/waxtap/internal/tempfile"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/waxerr"
)

// Mode selects how a cut is rendered. The facade maps waxtap.CutMode onto these.
type Mode uint8

const (
	// ModeSmart copies unless the spec needs decoding: an output codec, encode
	// filters, or a crossfade.
	ModeSmart Mode = iota
	// ModeCopy uses stream copy. It cannot transcode, apply filters, or crossfade.
	ModeCopy
	// ModeAccurate decodes, trims sample-exactly, and re-encodes. It requires an
	// output codec, not CodecCopy.
	ModeAccurate
)

func (m Mode) String() string {
	switch m {
	case ModeCopy:
		return "copy"
	case ModeAccurate:
		return "accurate"
	default:
		return "smart"
	}
}

// Spec describes a cut.
type Spec struct {
	// Remove lists the [Start, End) spans to delete. They are clamped to the
	// probed media duration and merged, so overlaps and out-of-range values are
	// harmless.
	Remove []Range
	// Mode selects copy/accurate/smart rendering.
	Mode Mode
	// Crossfade blends adjacent retained spans over this duration. It is ignored
	// when only one span remains and rejected in copy mode.
	Crossfade time.Duration
	// Encode configures the accurate path. Codec and Bitrate select the output;
	// Filters are appended to the cut graph because ffmpeg cannot combine -af
	// with the -filter_complex graph used for cutting. Render owns
	// FilterComplex. Leave Encode zero for a copy cut.
	Encode transcode.Spec
}

// Result reports a completed cut.
type Result struct {
	Output  string        // final output path (empty when Applied is false)
	Removed time.Duration // total audio removed
	Mode    Mode          // effective mode used; Smart is resolved before rendering
	// Applied is false when no ranges remained after clamping/merging, in which
	// case Render writes no output and the caller proceeds with the source
	// unchanged.
	Applied bool
}

// Render applies spec's cut to input and writes the result to output. It probes
// input to clamp ranges against the real duration, then renders per the resolved
// mode. Output is staged and atomically renamed on success; failures and
// cancellation leave output untouched.
//
// When the ranges remove nothing (empty after clamping/merging), Render writes no
// output and returns Applied=false. When they would remove the entire track it
// returns ErrIncompatibleSpec.
func Render(ctx context.Context, r *transcode.Runner, input, output string, spec Spec) (Result, error) {
	probe, err := r.Probe(ctx, input)
	if err != nil {
		return Result{}, err
	}
	total := probe.Format.Duration
	if total <= 0 {
		return Result{}, fmt.Errorf("%w: could not determine input duration", waxerr.ErrUnsupportedInput)
	}

	keeps := Keeps(spec.Remove, total)
	if len(keeps) == 0 {
		return Result{}, fmt.Errorf("%w: cut would remove the entire track", waxerr.ErrIncompatibleSpec)
	}
	kept := OutputDuration(keeps, 0)
	if kept >= total {
		return Result{Applied: false}, nil // nothing removed after clamping
	}

	mode, err := resolveMode(spec.Mode, spec.Crossfade, spec.Encode)
	if err != nil {
		return Result{}, err
	}
	if err := ValidateCrossfade(keeps, spec.Crossfade); err != nil {
		return Result{}, err
	}

	if mode == ModeCopy {
		if err := renderCopy(ctx, r, input, output, keeps); err != nil {
			return Result{}, err
		}
	} else {
		enc := spec.Encode
		// Keep the cut and encode filters in one graph; ffmpeg cannot apply -af to
		// an output produced by -filter_complex.
		enc.FilterComplex = encodeGraph(keeps, spec.Crossfade, total, enc.Filters)
		enc.Filters = nil
		if _, err := r.Transcode(ctx, input, output, enc); err != nil {
			return Result{}, err
		}
	}

	return Result{
		Output:  output,
		Removed: total - kept,
		Mode:    mode,
		Applied: true,
	}, nil
}

// resolveMode turns Smart into Copy or Accurate and rejects contradictory specs.
// Copy cannot honor a requested codec, filter, or crossfade; Accurate needs a
// real output codec.
func resolveMode(mode Mode, crossfade time.Duration, enc transcode.Spec) (Mode, error) {
	needsEncode := enc.Codec != transcode.CodecCopy || crossfade > 0 || len(enc.Filters) > 0
	effective := mode
	if effective == ModeSmart {
		if needsEncode {
			effective = ModeAccurate
		} else {
			effective = ModeCopy
		}
	}
	switch effective {
	case ModeCopy:
		if enc.Codec != transcode.CodecCopy {
			return 0, fmt.Errorf("%w: copy mode cannot transcode to %v (use accurate or smart mode)", waxerr.ErrIncompatibleSpec, enc.Codec)
		}
		if crossfade > 0 {
			return 0, fmt.Errorf("%w: crossfade requires a re-encode (use accurate mode)", waxerr.ErrIncompatibleSpec)
		}
		if len(enc.Filters) > 0 {
			return 0, fmt.Errorf("%w: encode filters require a re-encode, not copy", waxerr.ErrIncompatibleSpec)
		}
	case ModeAccurate:
		if enc.Codec == transcode.CodecCopy {
			return 0, fmt.Errorf("%w: accurate cut requires an output codec, not copy", waxerr.ErrIncompatibleSpec)
		}
	}
	return effective, nil
}

// renderCopy renders the cut without re-encoding. Each keep span is extracted to
// its own clean temp segment with a seek-and-copy (timestamps rebased to zero);
// a single span is written straight to the output, and multiple spans are joined
// with the concat demuxer over the separate, clean files. Concatenating distinct
// rebased files is reliable, unlike trimming one file in place with copy.
func renderCopy(ctx context.Context, r *transcode.Runner, input, output string, keeps []Range) error {
	absIn, err := filepath.Abs(input)
	if err != nil {
		return err
	}

	staged, err := tempfile.NewExternal(output)
	if err != nil {
		return err
	}
	defer staged.Discard()

	if len(keeps) == 1 {
		if err := extractCopy(ctx, r, absIn, staged.Path(), keeps[0]); err != nil {
			return err
		}
		return staged.Commit()
	}

	// Extract each span to a clean temp, then concat the separate files.
	outDir, err := filepath.Abs(filepath.Dir(output))
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(outDir, "waxtap-cut-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	ext := filepath.Ext(output)
	segPaths := make([]string, len(keeps))
	for i, k := range keeps {
		seg := filepath.Join(tmpDir, fmt.Sprintf("seg%03d%s", i, ext))
		if err := extractCopy(ctx, r, absIn, seg, k); err != nil {
			return err
		}
		segPaths[i] = seg
	}

	listFile, cleanup, err := tempfile.Scratch(tmpDir, "concat-*.txt")
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := listFile.WriteString(concatList(segPaths)); err != nil {
		return err
	}
	if err := listFile.Sync(); err != nil {
		return err
	}

	cmd := transcode.Command{Args: []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "concat", "-safe", "0", "-i", listFile.Name(),
		"-map", "0:a:0", "-c:a", "copy", staged.Path(),
	}}
	if _, err := r.Run(ctx, cmd); err != nil {
		return err
	}
	return staged.Commit()
}

// extractCopy losslessly extracts the span k from input to output. The seek and
// duration are output-side options (after -i): unlike an input-side seek, they
// trim reliably with -c copy across demuxers. The cut still snaps to the nearest
// packet boundary (a few ms for audio), copy mode's documented behavior.
func extractCopy(ctx context.Context, r *transcode.Runner, input, output string, k Range) error {
	cmd := transcode.Command{Args: []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", input, "-ss", secs(k.Start), "-t", secs(k.Duration()),
		"-vn", "-map", "0:a:0", "-c:a", "copy", output,
	}}
	_, err := r.Run(ctx, cmd)
	return err
}

// concatList builds a concat-demuxer script listing each pre-extracted segment
// file in order.
func concatList(paths []string) string {
	var b strings.Builder
	b.WriteString("ffconcat version 1.0\n")
	for _, p := range paths {
		b.WriteString("file ")
		b.WriteString(concatEscape(p))
		b.WriteByte('\n')
	}
	return b.String()
}

// concatEscape quotes a path for a concat-demuxer "file" directive.
func concatEscape(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
