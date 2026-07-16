package media

import (
	"context"
	"fmt"
	"os"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/format"

	"github.com/colespringer/waxtap/v3/internal/tempfile"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// Spec describes a transcode. Codec selects the output codec; Bitrate overrides
// lossy preset defaults in bits per second; Channels downmixes (1 or 2) when
// positive; GainDB applies a normalization gain (0 is a no-op). The zero value
// is a container remux (CodecCopy) with no processing.
type Spec struct {
	Codec    Codec
	Bitrate  int
	Channels int     // output channel count (downmix); 0 keeps the source layout
	GainDB   float64 // scalar normalization gain in dB; 0 is a no-op
}

// Result reports a completed transcode.
type Result struct {
	Output string // final output path
	Size   int64  // output size in bytes (0 if it could not be stat'd)
	Codec  Codec  // codec the output was encoded with
}

// Transcode reads input, applies spec, and writes the result to output. The
// output is staged in a temp file in output's directory and atomically renamed
// into place on success; on failure or cancellation the temp is removed and any
// existing file at output is left untouched.
//
// CodecCopy is a whole-file container remux (no re-encode); it rejects a channel
// or gain change, which require decoding.
func (r *Runner) Transcode(ctx context.Context, input, output string, spec Spec) (Result, error) {
	if spec.Codec == CodecCopy && (spec.Channels > 0 || spec.GainDB != 0) {
		return Result{}, fmt.Errorf("%w: a container copy cannot change channels or loudness", waxerr.ErrIncompatibleSpec)
	}

	src, closeSrc, err := openSource(input)
	if err != nil {
		return Result{}, err
	}
	defer closeSrc()

	staged, err := tempfile.New(output)
	if err != nil {
		return Result{}, err
	}
	defer staged.Discard() // no-op after Commit; cleans up on every error path

	if err := r.acquire(ctx); err != nil {
		return Result{}, err
	}
	defer r.release()

	hint := hintFor(input)
	if spec.Codec == CodecCopy {
		err = r.remux(ctx, src, hint, hintFor(output), staged)
	} else {
		opts := encodeOptions(spec)
		format, _ := codecFormat(spec.Codec)
		opts.Container = containerFor(format, hintFor(output))
		_, err = r.engine.Transcode(ctx, src, hint, staged, opts)
	}
	if err != nil {
		return Result{}, err
	}

	if err := staged.Commit(); err != nil {
		return Result{}, err
	}
	res := Result{Output: output, Codec: spec.Codec}
	if fi, serr := os.Stat(output); serr == nil {
		res.Size = fi.Size()
	}
	return res, nil
}

// RemuxContainer remuxes input to output using an explicit WaxFlow container
// override, e.g. "progressive" to flatten a fragmented MP4 into a tag-writable
// progressive one. It is a packet copy (no re-encode). input and output may be
// the same path; the source is closed before the atomic rename.
func (r *Runner) RemuxContainer(ctx context.Context, input, output, container string) error {
	src, closeSrc, err := openSource(input)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = closeSrc()
		}
	}()

	if err := r.acquire(ctx); err != nil {
		return err
	}
	defer r.release()

	staged, err := tempfile.New(output)
	if err != nil {
		return err
	}
	defer staged.Discard()

	demux, info, err := format.OpenDemuxer(src, hintFor(input), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	outFormat, ok := codecToFormat(track.Codec)
	if !ok {
		return fmt.Errorf("%w: cannot remux %s audio", waxerr.ErrIncompatibleSpec, codecName(track.Codec))
	}
	opts := waxflow.TranscodeOptions{Format: outFormat, Container: container}
	if _, err := r.engine.RemuxDemuxer(ctx, demux, track, staged, opts); err != nil {
		return err
	}
	// Close the source before the rename: on Windows a rename over an open file
	// fails, and input may equal output.
	closed = true
	if err := closeSrc(); err != nil {
		return err
	}
	return staged.Commit()
}

// remux rewrites the source packets into the container the output extension
// names, choosing WaxFlow's output format from the source codec (the codec must
// survive the trip) so no re-encode happens.
func (r *Runner) remux(ctx context.Context, src container.Source, srcHint, outExt string, dst *tempfile.File) error {
	demux, info, err := format.OpenDemuxer(src, srcHint, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, err)
	}
	track := info.Default()
	outFormat, ok := codecToFormat(track.Codec)
	if !ok {
		return fmt.Errorf("%w: cannot remux %s audio", waxerr.ErrIncompatibleSpec, codecName(track.Codec))
	}
	opts := waxflow.TranscodeOptions{Format: outFormat, Container: containerFor(outFormat, outExt)}
	_, err = r.engine.RemuxDemuxer(ctx, demux, track, dst, opts)
	return err
}

// containerFor returns the WaxFlow Container override for delivering format into
// the container named by ext, or "" for the format's own default container.
//
// It is format-aware because the right override depends on both: AAC/ALAC into
// .m4a need "progressive" (else WaxFlow ships fragmented CMAF, which Apple players
// and tag editors reject), and FLAC into .ogg needs "ogg" (else WaxFlow writes a
// bare FLAC stream in a .ogg file). Opus/Vorbis are Ogg natively, so "" suffices.
func containerFor(format, ext string) string {
	switch ext {
	case "mka", "mkv":
		return "mka"
	case "webm":
		return "webm"
	case "aac":
		return "adts"
	case "m4a", "mp4", "m4b":
		if format == "aac" || format == "alac" {
			return "progressive"
		}
	case "ogg", "oga":
		if format == "flac" {
			return "ogg"
		}
	}
	return ""
}

// codecToFormat maps a source codec ID to the WaxFlow output format that carries
// it unchanged, for a remux. It reports false for a codec WaxTap cannot remux.
func codecToFormat(id codec.ID) (string, bool) {
	switch id {
	case codec.Opus:
		return "opus", true
	case codec.AACLC:
		return "aac", true
	case codec.FLAC:
		return "flac", true
	case codec.ALAC:
		return "alac", true
	case codec.MP3:
		return "mp3", true
	case codec.Vorbis:
		return "vorbis", true
	case codec.PCM:
		return "wav", true
	}
	return "", false
}
