package media

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"slices"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// The OpusHead output gain: bytes 16-17 of the head, little-endian signed Q7.8
// dB (RFC 7845 section 5.1), which every compliant decoder applies to its
// output, WaxFlow's included. WaxFlow's muxers write a track's CodecConfig
// verbatim as the Ogg BOS page and the Matroska CodecPrivate, and its own cut
// rung patches the same bytes for pre-skip, so a patched head is the engine's
// own carrier for the gain.
const opusHeadGainOffset = 16

// OpusGainQ78 converts a gain in dB to the OpusHead output_gain unit, signed
// Q7.8 dB (256 per dB, a step of about 0.004 dB), rounded to the nearest step
// and clamped to the field's range.
func OpusGainQ78(gainDB float64) int {
	return int(min(max(math.Round(gainDB*256), math.MinInt16), math.MaxInt16))
}

// OpusGainDB is the decibels a Q7.8 field value applies, the scale
// waxlabel.OutputGainDecibels defines.
func OpusGainDB(q int) float64 { return float64(q) / 256 }

// fmtGain renders a Q7.8 value as decibels, for an error the user reads.
func fmtGain(q int) string { return fmt.Sprintf("%.2f", OpusGainDB(q)) }

// opusHeadGain reads the output gain out of an OpusHead.
func opusHeadGain(head []byte) (int, error) {
	if len(head) < 19 || string(head[:8]) != "OpusHead" {
		return 0, fmt.Errorf("%w: the track's codec configuration is not an OpusHead", waxerr.ErrUnsupportedInput)
	}
	return int(int16(binary.LittleEndian.Uint16(head[opusHeadGainOffset:]))), nil
}

// opusHeadWithGain returns a copy of head with its output gain moved by delta,
// or an error when the result leaves the field's range.
func opusHeadWithGain(head []byte, delta int) ([]byte, int, error) {
	base, err := opusHeadGain(head)
	if err != nil {
		return nil, 0, err
	}
	gain := base + delta
	if gain < math.MinInt16 || gain > math.MaxInt16 {
		return nil, 0, fmt.Errorf("%w: an output gain of %s dB is outside what an Opus header can state", waxerr.ErrIncompatibleSpec, fmtGain(gain))
	}
	out := slices.Clone(head)
	binary.LittleEndian.PutUint16(out[opusHeadGainOffset:], uint16(int16(gain)))
	return out, gain, nil
}

// OpusHeaderGain reads the OpusHead output gain of path's default track, in
// Q7.8 dB, from whichever container carries it.
func OpusHeaderGain(ctx context.Context, path string) (int, error) {
	src, closeSrc, err := openSource(path)
	if err != nil {
		return 0, err
	}
	defer closeSrc()
	_, info, err := format.OpenDemuxer(src, hintFor(path), nil)
	if err != nil {
		return 0, classifyInputError(err, path)
	}
	track := info.Default()
	if track.Codec != codec.Opus {
		return 0, fmt.Errorf("%w: %s is %s, not Opus", waxerr.ErrIncompatibleSpec, path, codecName(track.Codec))
	}
	return opusHeadGain(track.CodecConfig)
}

// RemuxWithOpusGain copies input's Opus packets to output, in the container the
// output extension names, with the head's output gain moved by delta (Q7.8 dB).
// It returns the gain the written head states. The remux carries the source's
// own gain, and a measurement of the source already heard that gain, so a
// caller adds the change rather than setting a total.
func (r *Runner) RemuxWithOpusGain(ctx context.Context, input, output string, delta int) (Result, int, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return Result{}, 0, err
	}
	defer closeSrc()
	if err := r.acquire(ctx); err != nil {
		return Result{}, 0, err
	}
	defer r.release()
	staged, err := tempfile.New(output)
	if err != nil {
		return Result{}, 0, err
	}
	defer staged.Discard()
	demux, info, err := format.OpenDemuxer(src, hintFor(input), nil)
	if err != nil {
		return Result{}, 0, classifyInputError(err, input)
	}
	track := info.Default()
	if track.Codec != codec.Opus {
		return Result{}, 0, fmt.Errorf("%w: a header gain needs an Opus source, not %s", waxerr.ErrIncompatibleSpec, codecName(track.Codec))
	}
	head, gain, err := opusHeadWithGain(track.CodecConfig, delta)
	if err != nil {
		return Result{}, 0, err
	}
	track.CodecConfig = head
	opts := waxflow.TranscodeOptions{Format: "opus", Container: containerFor("opus", hintFor(output))}
	tres, err := r.engine.RemuxDemuxer(ctx, demux, track, staged, opts)
	if err != nil {
		return Result{}, 0, classifyEngineError(err, input, output)
	}
	if err := staged.Commit(); err != nil {
		return Result{}, 0, err
	}
	res := Result{Output: output, Codec: CodecCopy, InputWarnings: sourceWarnings(tres.InputWarnings)}
	if fi, serr := os.Stat(output); serr == nil {
		res.Size = fi.Size()
	}
	return res, gain, nil
}
