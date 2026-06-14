// Package transcode wraps ffmpeg and ffprobe for local audio files.
//
// Callers pass file paths; the package resolves the ffmpeg/ffprobe binaries,
// limits concurrent processes when configured, captures bounded stderr for
// diagnostics, and terminates the process, or its process group where supported,
// when a context is canceled.
//
// A [Command] is an argument list, so command construction can be tested
// without starting ffmpeg. A [Spec] selects the target [Codec], optional bitrate,
// and pre-encode audio filters. Loudness normalization and cuts are passed in as
// filters so they can run in the same encode as the format conversion.
//
// Processing uses seekable files, not pipes, because several muxers need seeks
// while writing. Outputs are staged next to the destination path and renamed into
// place only after ffmpeg exits successfully. [CodecCopy] is the no-encode mode;
// FLAC, ALAC, and WAV decode and encode with lossless codecs.
package transcode

import (
	"context"
	"os"

	"github.com/colespringer/waxtap/internal/tempfile"
)

// Result reports a completed transcode.
type Result struct {
	Output string // final output path
	Size   int64  // output size in bytes (0 if it could not be stat'd)
	Codec  Codec  // codec the output was encoded with
}

// Transcode reads input, applies spec.Filters, encodes per spec, and writes the
// result to output. The output is staged in a temp file in output's directory and
// atomically renamed into place on success; on failure or cancellation the temp
// is removed and any existing file at output is left untouched.
//
// Input must be a seekable local file. A stream copy (CodecCopy) combined with
// filters is rejected with waxerr.ErrIncompatibleSpec before any process starts.
func (r *Runner) Transcode(ctx context.Context, input, output string, spec Spec) (Result, error) {
	// WAV has separate PCM encoders for each representation, so choose the
	// encoder from the probed source rather than relying on the preset fallback.
	encoder := ""
	if spec.Codec == CodecWAV {
		probed, err := r.Probe(ctx, input)
		if err != nil {
			return Result{}, err
		}
		audio, _ := probed.AudioStream()
		encoder = wavEncoder(audio)
	}

	staged, err := tempfile.NewExternal(output, spec.StageExt)
	if err != nil {
		return Result{}, err
	}
	defer staged.Discard() // no-op after a successful Commit; cleans up on every error path

	cmd, err := buildCommandWith(input, staged.Path(), spec, encoder)
	if err != nil {
		return Result{}, err // spec error (e.g. copy + filters)
	}
	if _, err := r.Run(ctx, cmd); err != nil {
		// ffmpeg's error names the staged temp; show the caller's output path.
		return Result{}, RedactPath(err, staged.Path(), output)
	}
	if err := staged.Commit(); err != nil {
		return Result{}, err
	}

	res := Result{Output: output, Codec: spec.Codec}
	if fi, err := os.Stat(output); err == nil {
		res.Size = fi.Size()
	}
	return res, nil
}
