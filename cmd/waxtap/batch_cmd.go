package main

import (
	"context"
	"io"

	"github.com/colespringer/waxtap/v2"
	"github.com/spf13/cobra"
)

// directoryTranscodeParams carries the transcode command's directory-mode inputs.
type directoryTranscodeParams struct {
	root         string
	explicit     string
	dir          string
	recursive    bool
	format       string
	bitrate      int
	channels     string
	downmix      bool
	collisionStr string
	force        bool
	concurrency  int
}

// runDirectoryTranscode processes recognized audio files from a directory input.
func runDirectoryTranscode(cmd *cobra.Command, env *appEnv, p directoryTranscodeParams) error {
	if err := rejectChangedFlags(cmd, "is only used with a URL input", "itag", "codec", "source-policy", "no-fallback"); err != nil {
		return err
	}
	if p.explicit != "" {
		return usagef("a directory input writes multiple files; use --dir, not a single output path")
	}
	if err := rejectDirIsFile(p.dir); err != nil {
		return err
	}
	if p.format == "" {
		return usagef("a directory input needs --format (the output extension cannot be inferred per file)")
	}
	tf, err := parseTranscodeFormat(p.format)
	if err != nil {
		return err
	}
	if tf == waxtap.FormatCopy {
		return usagef("directory processing does not support --format copy; choose an encoded output format")
	}
	layout, doDownmix, err := resolveChannels(cmd, env.cfg, p.channels, p.downmix)
	if err != nil {
		return err
	}
	mode, err := collisionFor(cmd, p.collisionStr)
	if err != nil {
		return err
	}
	concurrency, err := batchConcurrency(env, p.concurrency)
	if err != nil {
		return err
	}

	spec := waxtap.ProcessSpec{Transcode: &waxtap.TranscodeSpec{Format: tf, Bitrate: p.bitrate}, Channels: layout, Downmix: doDownmix}
	ctx := cmd.Context()
	inputs, ignored, err := collectAudioInputs(p.root, p.recursive, p.dir)
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		env.info("no recognized audio files found in %s\n", p.root)
	}
	jobs, err := planBatchOutputs(ctx, inputs, p.root, p.dir, p.recursive, tf, spec, mode, p.force, "transcoded", env.client.ProbeCodec)
	if err != nil {
		return err
	}
	processFn := fileProcessFn(env, spec, batchThreadCap(concurrency))
	outcomes := runBatchJobs(ctx, jobs, concurrency, processFn, batchProgress(env, len(jobs)))
	emitBatchProcess(env, outcomes, ignored, transcodeExt(tf))
	return batchExit(ctx, outcomes)
}

// directoryNormalizeParams carries the normalize command's directory-mode inputs.
type directoryNormalizeParams struct {
	root         string
	explicit     string
	dir          string
	recursive    bool
	measure      bool
	target       float64
	format       string
	bitrate      int
	channels     string
	downmix      bool
	collisionStr string
	concurrency  int
}

// runDirectoryNormalize processes recognized audio files in a directory. It
// normalizes by default and measures loudness when requested.
func runDirectoryNormalize(cmd *cobra.Command, env *appEnv, p directoryNormalizeParams) error {
	if err := validateNormalizeInputFlags(cmd, p.measure, true, false); err != nil {
		return err
	}
	if p.explicit != "" {
		if !p.measure {
			return usagef("a directory input writes multiple files; use --dir, not a single output path")
		}
		return usagef("--measure-loudness does not write output; remove the output path")
	}
	concurrency, err := batchConcurrency(env, p.concurrency)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	// Exclude a nested output directory only when writing files.
	excludeDir := ""
	if !p.measure {
		excludeDir = p.dir
	}
	inputs, ignored, err := collectAudioInputs(p.root, p.recursive, excludeDir)
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		env.info("no recognized audio files found in %s\n", p.root)
	}

	if p.measure {
		threadCap := batchThreadCap(concurrency)
		measureFn := func(ctx context.Context, input, _ string) (*waxtap.Result, error) {
			return env.client.Process(ctx, waxtap.ProcessRequest{
				Input: input,
				ProcessSpec: waxtap.ProcessSpec{
					Loudness: &waxtap.LoudnessSpec{Mode: waxtap.LoudnessMeasureOnly},
					Output:   waxtap.ToWriter(io.Discard),
					Threads:  threadCap,
				},
			})
		}
		outcomes := runBatchJobs(ctx, measureJobs(inputs), concurrency, measureFn, batchProgress(env, len(inputs)))
		emitBatchMeasure(env, outcomes, ignored)
		return batchExit(ctx, outcomes)
	}

	if err := rejectDirIsFile(p.dir); err != nil {
		return err
	}
	if p.format == "" {
		return usagef("normalizing a directory requires --format (e.g. flac); use --measure-loudness to analyze without writing files")
	}
	tf, err := parseTranscodeFormat(p.format)
	if err != nil {
		return err
	}
	if tf == waxtap.FormatCopy {
		return usagef("directory normalization re-encodes; --format copy is not supported")
	}
	layout, doDownmix, err := resolveChannels(cmd, env.cfg, p.channels, p.downmix)
	if err != nil {
		return err
	}
	mode, err := collisionFor(cmd, p.collisionStr)
	if err != nil {
		return err
	}
	spec := waxtap.ProcessSpec{
		Transcode: &waxtap.TranscodeSpec{Format: tf, Bitrate: p.bitrate},
		Loudness:  &waxtap.LoudnessSpec{Mode: waxtap.LoudnessApply, Target: p.target},
		Channels:  layout,
		Downmix:   doDownmix,
	}
	jobs, err := planBatchOutputs(ctx, inputs, p.root, p.dir, p.recursive, tf, spec, mode, false, "normalized", env.client.ProbeCodec)
	if err != nil {
		return err
	}
	processFn := fileProcessFn(env, spec, batchThreadCap(concurrency))
	outcomes := runBatchJobs(ctx, jobs, concurrency, processFn, batchProgress(env, len(jobs)))
	emitBatchProcess(env, outcomes, ignored, transcodeExt(tf))
	return batchExit(ctx, outcomes)
}

// fileProcessFn returns a processor that gives each job its own output path.
func fileProcessFn(env *appEnv, spec waxtap.ProcessSpec, threadCap int) func(context.Context, string, string) (*waxtap.Result, error) {
	return func(ctx context.Context, input, output string) (*waxtap.Result, error) {
		s := spec
		s.Threads = threadCap
		s.Output = waxtap.ToFile(output)
		return env.client.Process(ctx, waxtap.ProcessRequest{Input: input, ProcessSpec: s})
	}
}

// batchConcurrency validates and clamps the requested parallelism. Zero (the
// default, or an explicit --concurrency 0) means serial; only negatives are
// rejected. It does not auto-detect cores.
func batchConcurrency(env *appEnv, n int) (int, error) {
	n, err := clampConcurrency(env, n)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		n = 1
	}
	return n, nil
}

// batchExit returns cancellation or the item error with the highest exit code.
// The returned error is marked as rendered because item errors have already been
// printed.
func batchExit(ctx context.Context, outcomes []batchOutcome) error {
	if ctx.Err() != nil {
		return alreadyRendered(ctx.Err())
	}
	if rep := representativeError(outcomes); rep != nil {
		return alreadyRendered(rep)
	}
	return nil
}
