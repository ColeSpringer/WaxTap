package media

import (
	"context"

	"github.com/colespringer/waxflow/format"
)

// PlanOutputChannels reports the channel count an encode of input under spec
// delivers, from WaxFlow's own plan of it (Engine.PlanTranscode, headers
// only): the lossy rows fold a source wider than stereo on their own, and a
// loudness measurement that feeds such an encode has to fold the same way.
// A CodecCopy spec keeps the source layout. output names the container the
// way Transcode's does, since the plan is keyed on the options the encode
// runs under. A plan that fails is an encode that would fail, classified the
// same way.
//
// The plan itself decodes nothing, but opening the source to read its headers
// is real I/O, so it takes a concurrency slot and stops at a cancellation like
// every other read here: an album plans once per track.
func (r *Runner) PlanOutputChannels(ctx context.Context, input, output string, spec Spec) (int, error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return 0, err
	}
	defer closeSrc()
	if err := r.acquire(ctx); err != nil {
		return 0, err
	}
	defer r.release()
	_, info, err := format.OpenDemuxer(src, hintFor(input), nil)
	if err != nil {
		return 0, classifyInputError(err, input)
	}
	track := info.Default()
	if spec.Codec == CodecCopy {
		return track.Fmt.Channels, nil
	}
	opts := encodeOptions(spec)
	name, _ := codecFormat(spec.Codec)
	opts.Container = containerFor(name, hintFor(output))
	plan, err := r.engine.PlanTranscode(track, opts)
	if err != nil {
		return 0, classifyEngineError(err, input, output)
	}
	return plan.Format.Channels, nil
}
