package media

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// ToEnd is WaxFlow's open-ended span bound, re-exported so a caller can name a
// span that runs to whatever the source holds without importing the engine.
const ToEnd = waxflow.ToEnd

// SampleTime is the duration n samples occupy at rate, WaxFlow's own
// conversion, re-exported for the same reason.
func SampleTime(n int64, rate int) time.Duration { return waxflow.SampleTime(n, rate) }

// RenderSpan decodes the sample span [from, to) of input (to may be ToEnd) and
// encodes it to output under enc, the way a cut renders one kept span:
// sample-exact, since the slice sits downstream of the decoder. It is the
// primitive a CUE split loops over, one call per piece, each opening the source
// afresh and decoding only its own span, so the whole split costs about one
// decode. Output is staged and renamed into place on success.
//
// source is the length the span's bounds were checked against, when the caller
// measured the file because its headers only claim one (Runner.MeasureLength);
// the zero value leaves the file's own declaration to bound the span. The span
// is held to it: a file whose headers declare less than a read delivers (an
// ASF's rounded duration) still serves a span past the declaration, and a file
// that ends short of a length it stood behind fails the span rather than
// writing it short. No piece of a split is empty: a span that names no samples
// (its end at its start, or an open one starting where the source ends) is
// refused as the request it is, and one that names some and delivers none is
// refused as the file's fault.
func (r *Runner) RenderSpan(ctx context.Context, input, output string, from, to int64, source Measured, enc Spec) (Result, error) {
	if enc.Codec == CodecCopy {
		return Result{}, fmt.Errorf("%w: a span is rendered by decoding, so it needs an encode target, not copy", waxerr.ErrIncompatibleSpec)
	}
	src, closeSrc, err := openSource(input)
	if err != nil {
		return Result{}, err
	}
	defer closeSrc()
	if err := r.acquire(ctx); err != nil {
		return Result{}, err
	}
	defer r.release()
	staged, err := tempfile.New(output)
	if err != nil {
		return Result{}, err
	}
	defer staged.Discard()
	med, err := format.Open(src, hintFor(input), nil)
	if err != nil {
		return Result{}, classifyInputError(err, input)
	}
	// ctx also bounds the seek the span's first read runs to its start, which
	// is a walk for a payload indexed lazily (an MP3, a WMA v1/v2 track).
	sl, err := waxflow.SliceMeasured(ctx, med, source.track(), from, to) // closes med on error
	if err != nil {
		return Result{}, classifyEngineError(err, input, output)
	}
	defer sl.Close() // owns med
	// The span's own length, as the slice resolved it against the source's:
	// zero is an empty request, whichever bound made it so, and it is refused
	// before an encoder writes headers for it.
	if sl.Info().Default().Samples == 0 {
		return Result{}, fmt.Errorf("%w: the span from sample %d holds no samples", waxerr.ErrIncompatibleSpec, from)
	}
	opts := encodeOptions(enc)
	name, _ := codecFormat(enc.Codec)
	opts.Container = containerFor(name, hintFor(output))
	tres, err := r.engine.TranscodeMedia(ctx, sl, staged, opts)
	if err != nil {
		return Result{}, classifyEngineError(err, input, output)
	}
	if tres.Samples == 0 {
		// The span named samples and none came. A read that ends short of an
		// exact length already fails, so this is the belt for a length nothing
		// stood behind: a seek that landed past the audio, refused rather than
		// written.
		return Result{}, fmt.Errorf("%w: the source delivered no audio from sample %d on", waxerr.ErrUnsupportedInput, from)
	}
	if err := staged.Commit(); err != nil {
		return Result{}, err
	}
	// The loss is the span's own: a hole is counted by the piece it falls in,
	// and a truncation only by a span that runs to the end of the source.
	res := Result{Output: output, Codec: enc.Codec, Levels: levelsOf(tres), InputWarnings: sourceWarnings(tres.InputWarnings), Loss: lossOf(tres)}
	if fi, serr := os.Stat(output); serr == nil {
		res.Size = fi.Size()
	}
	return res, nil
}
