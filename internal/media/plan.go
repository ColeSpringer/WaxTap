package media

import (
	"context"

	"github.com/colespringer/waxflow/format"
)

// PlanEncode reports what an encode of input under spec delivers, from
// WaxFlow's own plan of it (Engine.PlanTranscode, headers only): the channel
// count and the bit rate the encoder really runs at.
//
// The lossy rows fold a source wider than stereo on their own, and a loudness
// measurement that feeds such an encode has to fold the same way. The rate is
// the plan's projected output rate (TranscodePlan.BitRate), which is the
// encoder's own answer to spec.Bitrate: each encoder has its own range and
// grid, so a request it cannot use exactly comes back as the nearest it can.
// It is 0 for a codec that carries no rate (the lossless rows, and Vorbis,
// which is quality-driven).
//
// A CodecCopy spec keeps the source layout and reports no rate. output names
// the container the way Transcode's does, since the plan is keyed on the
// options the encode runs under. A plan that fails is an encode that would
// fail, classified the same way: a rate under the encoder's floor is refused
// here rather than after a file is written.
//
// The plan itself decodes nothing, but opening the source to read its headers
// is real I/O, so it takes a concurrency slot and stops at a cancellation like
// every other read here: an album plans once per track.
func (r *Runner) PlanEncode(ctx context.Context, input, output string, spec Spec) (channels, bitRate int, err error) {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return 0, 0, err
	}
	defer closeSrc()
	if err := r.acquire(ctx); err != nil {
		return 0, 0, err
	}
	defer r.release()
	_, info, err := format.OpenDemuxer(src, hintFor(input), nil)
	if err != nil {
		return 0, 0, classifyInputError(err, input)
	}
	track := info.Default()
	if spec.Codec == CodecCopy {
		return track.Fmt.Channels, 0, nil
	}
	opts := encodeOptions(spec)
	name, _ := codecFormat(spec.Codec)
	opts.Container = containerFor(name, hintFor(output))
	plan, err := r.engine.PlanTranscode(track, opts)
	if err != nil {
		return 0, 0, classifyEngineError(err, input, output)
	}
	return plan.Format.Channels, plan.BitRate, nil
}
