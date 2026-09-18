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
func (r *Runner) RenderSpan(ctx context.Context, input, output string, from, to int64, enc Spec) (Result, error) {
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
	sl, err := waxflow.Slice(med, from, to)
	if err != nil {
		med.Close()
		return Result{}, classifyEngineError(err, input, output)
	}
	defer sl.Close() // owns med
	opts := encodeOptions(enc)
	name, _ := codecFormat(enc.Codec)
	opts.Container = containerFor(name, hintFor(output))
	tres, err := r.engine.TranscodeMedia(ctx, sl, staged, opts)
	if err != nil {
		return Result{}, classifyEngineError(err, input, output)
	}
	if err := staged.Commit(); err != nil {
		return Result{}, err
	}
	res := Result{Output: output, Codec: enc.Codec, Levels: levelsOf(tres), InputWarnings: sourceWarnings(tres.InputWarnings)}
	if fi, serr := os.Stat(output); serr == nil {
		res.Size = fi.Size()
	}
	return res, nil
}
