package main

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/colespringer/waxtap/v2"
	"github.com/colespringer/waxtap/v2/format"
	"github.com/colespringer/waxtap/v2/internal/tempfile"
)

// audioExts lists the case-insensitive file extensions accepted for directory
// processing.
var audioExts = map[string]bool{
	".flac": true, ".wav": true, ".mp3": true, ".m4a": true, ".aac": true,
	".opus": true, ".ogg": true, ".alac": true, ".mka": true, ".webm": true,
}

// collectAudioInputs returns recognized audio files under root in sorted order.
// Recursive walks do not follow directory symlinks. excludeDir is omitted from a
// recursive walk so an output directory beneath root is not processed as input.
// Unrecognized regular files contribute to ignored; directories and other file
// types do not.
func collectAudioInputs(root string, recursive bool, excludeDir string) (inputs []string, ignored int, err error) {
	absRoot, _ := filepath.Abs(root)
	absExclude := ""
	if excludeDir != "" {
		if a, e := filepath.Abs(excludeDir); e == nil && a != absRoot {
			absExclude = a
		}
	}
	consider := func(path string, d fs.DirEntry) {
		if !d.Type().IsRegular() {
			return // skip symlinks, devices, and directories
		}
		if audioExts[strings.ToLower(filepath.Ext(path))] {
			inputs = append(inputs, path)
		} else {
			ignored++
		}
	}

	if !recursive {
		entries, rerr := os.ReadDir(root)
		if rerr != nil {
			return nil, 0, rerr
		}
		for _, e := range entries {
			consider(filepath.Join(root, e.Name()), e)
		}
	} else {
		werr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if absExclude != "" {
					if a, _ := filepath.Abs(path); a == absExclude {
						return filepath.SkipDir
					}
				}
				return nil
			}
			consider(path, d)
			return nil
		})
		if werr != nil {
			return nil, 0, werr
		}
	}
	sort.Strings(inputs)
	return inputs, ignored, nil
}

// batchAction identifies how runBatchJobs handles an input.
type batchAction int

const (
	actProcess   batchAction = iota // run processFn
	actCopy                         // copy an unchanged file into --dir
	actUnchanged                    // leave an in-place match unchanged
	actSkip                         // skip an existing output
)

// batchJob is one planned input transformation.
type batchJob struct {
	index  int
	input  string
	output string // destination; the input path for actUnchanged
	action batchAction
}

// batchStatus identifies a completed job's outcome.
type batchStatus int

const (
	statusOK        batchStatus = iota // processed
	statusCopied                       // copied through (already target codec)
	statusUnchanged                    // in-place no-op
	statusSkipped                      // collision skip
	statusError                        // failed
	statusNotRun                       // canceled before running
)

func (s batchStatus) String() string {
	switch s {
	case statusOK:
		return "ok"
	case statusCopied:
		return "copied"
	case statusUnchanged:
		return "unchanged"
	case statusSkipped:
		return "skipped"
	case statusError:
		return "error"
	default:
		return "not-run"
	}
}

// batchOutcome is a job's result after runBatchJobs.
type batchOutcome struct {
	index  int
	input  string
	output string
	status batchStatus
	result *waxtap.Result
	err    error
}

// targetCodecFamily returns the codec family produced by a transcode format.
// It returns an empty string when the no-op check cannot reliably identify a
// matching source. Keep these cases aligned with parseTranscodeFormat and
// transcodeExt.
func targetCodecFamily(tf waxtap.TranscodeFormat) string {
	switch tf {
	case waxtap.FormatFLAC:
		return "flac"
	case waxtap.FormatALAC:
		return "alac"
	case waxtap.FormatAAC:
		return "aac"
	case waxtap.FormatMP3:
		return "mp3"
	case waxtap.FormatOpus:
		return "opus"
	case waxtap.FormatVorbis:
		return "vorbis"
	default:
		return "" // WAV, copy, and unknown formats cannot be confirmed as matches.
	}
}

// matchesTargetFamily reports whether a probed codec is one that tf produces.
// Formats without a stable codec family, such as WAV and copy, return false so
// single-file and batch planning use the same conservative rule.
func matchesTargetFamily(codec string, tf waxtap.TranscodeFormat) bool {
	fam := targetCodecFamily(tf)
	return fam != "" && format.CodecFamily(codec) == fam
}

// batchTransforms reports whether the spec requires rewriting matching codecs.
func batchTransforms(spec waxtap.ProcessSpec) bool {
	if spec.Downmix || spec.Loudness != nil {
		return true
	}
	return spec.Transcode != nil && spec.Transcode.Bitrate > 0
}

// extPossiblyCodec reports whether ext can contain the given codec family. It
// only filters probe candidates; every possible match is still confirmed with
// ffprobe. General-purpose and unknown containers return true.
func extPossiblyCodec(ext, family string) bool {
	switch ext {
	case ".flac":
		return family == "flac"
	case ".mp3":
		return family == "mp3"
	case ".opus":
		return family == "opus"
	case ".alac":
		return family == "alac"
	case ".aac":
		return family == "aac"
	case ".m4a":
		return family == "aac" || family == "alac"
	case ".ogg", ".oga":
		return family == "vorbis" || family == "opus" || family == "flac"
	case ".webm":
		return family == "opus" || family == "vorbis"
	case ".wav":
		return false // PCM is not one of the comparable target families.
	default:
		return true // Probe general-purpose and unknown containers.
	}
}

// batchProbeCodecs probes candidate files in parallel. Files that cannot be left
// unchanged are not probed, and failed probes are omitted from the result.
func batchProbeCodecs(ctx context.Context, inputs []string, family string, skip bool, probeCodec func(context.Context, string) (string, error)) map[string]string {
	codecs := make(map[string]string)
	if skip || family == "" {
		return codecs
	}
	var todo []string
	for _, in := range inputs {
		if extPossiblyCodec(strings.ToLower(filepath.Ext(in)), family) {
			todo = append(todo, in)
		}
	}
	if len(todo) == 0 {
		return codecs
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, min(len(todo), runtime.NumCPU())))
	for _, in := range todo {
		if ctx.Err() != nil {
			break
		}
		// Include cancellation while waiting for a worker slot so Ctrl-C during
		// planning is not ignored until an in-flight probe returns.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(in string) {
			defer wg.Done()
			defer func() { <-sem }()
			if codec, err := probeCodec(ctx, in); err == nil {
				mu.Lock()
				codecs[in] = codec
				mu.Unlock()
			}
		}(in)
	}
	wg.Wait()
	return codecs
}

// planBatchOutputs creates jobs for a transcode or normalize-apply batch. It
// applies collision policy, rejects outputs that would overwrite another input,
// and rejects multiple inputs that map to the same output. A file whose probed
// codec already matches the target is left in place or copied unchanged into
// dir, unless force is set.
func planBatchOutputs(ctx context.Context, inputs []string, root, dir string, recursive bool, tf waxtap.TranscodeFormat, spec waxtap.ProcessSpec, mode collisionMode, force bool, tag string, probeCodec func(context.Context, string) (string, error)) ([]batchJob, error) {
	if tf == waxtap.FormatCopy {
		return nil, usagef("directory processing does not support --format copy; choose an encoded output format")
	}
	reserver := newPathReserver()
	inputAbs := make(map[string]bool, len(inputs))
	absByInput := make(map[string]string, len(inputs))
	for _, in := range inputs {
		if a, e := filepath.Abs(in); e == nil {
			inputAbs[a] = true
			absByInput[in] = a
		}
	}
	seenOut := map[string]string{}
	fam := targetCodecFamily(tf)

	// Probe candidates before planning so ffprobe calls can run in parallel.
	// A failed probe leaves the file scheduled for normal processing.
	codecs := batchProbeCodecs(ctx, inputs, fam, force || batchTransforms(spec), probeCodec)

	jobs := make([]batchJob, 0, len(inputs))
	for i, in := range inputs {
		noop := false
		if codec, ok := codecs[in]; ok && matchesTargetFamily(codec, tf) {
			noop = true
		}

		if noop && dir == "" {
			jobs = append(jobs, batchJob{index: i, input: in, output: in, action: actUnchanged})
			continue
		}

		// A copy-through preserves the source container, so it keeps the original
		// name; a re-encode uses the target extension.
		var out string
		switch {
		case noop:
			out = mirrorInto(dir, root, in, recursive, filepath.Base(in))
		case dir == "":
			out = deriveLocalOutput(in, transcodeExt(tf), tag)
		default:
			stem := strings.TrimSuffix(filepath.Base(in), filepath.Ext(in))
			out = mirrorInto(dir, root, in, recursive, stem+"."+transcodeExt(tf))
		}

		absOut, _ := filepath.Abs(out)
		// A matching file mapped to itself remains unchanged.
		if noop && absOut == absByInput[in] {
			jobs = append(jobs, batchJob{index: i, input: in, output: in, action: actUnchanged})
			continue
		}
		if inputAbs[absOut] {
			return nil, usagef("output %q would overwrite an input file; choose a different --dir or format", out)
		}
		if prev, dup := seenOut[absOut]; dup {
			return nil, usagef("inputs %q and %q both map to output %q; rename one or choose a different --dir", prev, in, out)
		}
		seenOut[absOut] = in

		resolved, skip, rerr := reserver.reserveOr(out, mode)
		if rerr != nil {
			return nil, rerr
		}
		switch {
		case skip:
			jobs = append(jobs, batchJob{index: i, input: in, output: resolved, action: actSkip})
		case noop:
			jobs = append(jobs, batchJob{index: i, input: in, output: resolved, action: actCopy})
		default:
			jobs = append(jobs, batchJob{index: i, input: in, output: resolved, action: actProcess})
		}
	}
	return jobs, nil
}

// mirrorInto resolves an output path under dir. Recursive runs preserve the
// input's directory relative to root.
func mirrorInto(dir, root, input string, recursive bool, name string) string {
	if recursive {
		if rel, ok := relUnder(root, filepath.Dir(input)); ok && rel != "." {
			return filepath.Join(dir, rel, name)
		}
	}
	return filepath.Join(dir, name)
}

// measureJobs builds jobs that measure every input without writing output.
func measureJobs(inputs []string) []batchJob {
	jobs := make([]batchJob, len(inputs))
	for i, in := range inputs {
		jobs[i] = batchJob{index: i, input: in, action: actProcess}
	}
	return jobs
}

// batchThreadCap returns the per-job ffmpeg thread cap. Serial runs return zero
// and let ffmpeg choose.
func batchThreadCap(concurrency int) int {
	if concurrency <= 1 {
		return 0
	}
	return max(1, runtime.NumCPU()/concurrency)
}

// copyThrough copies src to dst using the same staged-output path as other writes.
func copyThrough(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return tempfile.WrapOutput("mkdir", err)
	}
	tf, err := tempfile.New(dst)
	if err != nil {
		return err
	}
	defer tf.Discard()
	// Treat copy failures as output failures so the CLI can provide destination
	// directory guidance.
	if _, err := io.Copy(tf, in); err != nil {
		return tempfile.WrapOutput("copy", err)
	}
	return tf.Commit()
}

// runBatchJobs executes jobs with bounded concurrency and continues after item
// failures. The returned outcomes preserve input order. Cancellation stops new
// work and marks remaining jobs not-run. onProgress, when set, is called once per
// completed item and is serialized across workers.
func runBatchJobs(ctx context.Context, jobs []batchJob, concurrency int, processFn func(context.Context, string, string) (*waxtap.Result, error), onProgress func(batchOutcome)) []batchOutcome {
	outcomes := make([]batchOutcome, len(jobs))
	sem := make(chan struct{}, max(1, concurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	report := func(o batchOutcome) {
		if onProgress == nil {
			return
		}
		mu.Lock()
		onProgress(o)
		mu.Unlock()
	}

	for idx, job := range jobs {
		outcomes[idx] = batchOutcome{index: job.index, input: job.input, output: job.output}
		if ctx.Err() != nil {
			outcomes[idx].status = statusNotRun
			continue
		}
		switch job.action {
		case actUnchanged:
			outcomes[idx].status, outcomes[idx].output = statusUnchanged, job.input
			report(outcomes[idx])
			continue
		case actSkip:
			outcomes[idx].status = statusSkipped
			report(outcomes[idx])
			continue
		}
		// Include cancellation while waiting for a worker slot.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			outcomes[idx].status = statusNotRun
			continue
		}
		wg.Add(1)
		go func(idx int, job batchJob) {
			defer wg.Done()
			defer func() { <-sem }()
			if job.action == actCopy {
				if err := copyThrough(job.input, job.output); err != nil {
					outcomes[idx].status, outcomes[idx].err = statusError, err
				} else {
					outcomes[idx].status = statusCopied
				}
			} else {
				res, err := processFn(ctx, job.input, job.output)
				if err != nil {
					outcomes[idx].status, outcomes[idx].err = statusError, err
				} else {
					outcomes[idx].status, outcomes[idx].result = statusOK, res
				}
			}
			report(outcomes[idx])
		}(idx, job)
	}
	wg.Wait()
	return outcomes
}

// representativeError returns the item error with the highest CLI exit code.
func representativeError(outcomes []batchOutcome) error {
	var rep error
	best := -1
	for _, o := range outcomes {
		if o.err == nil {
			continue
		}
		if code := exitCodeFor(o.err); code > best {
			best, rep = code, o.err
		}
	}
	return rep
}
