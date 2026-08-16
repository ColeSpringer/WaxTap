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

	"github.com/colespringer/waxtap/v3"
	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/internal/tempfile"
)

// audioExts lists the case-insensitive file extensions accepted for directory
// processing. These are the conventional spellings, not everything WaxFlow can
// sniff: .wave, .rf64, .bw64, .m4r, .mov, .adts and .mpga also decode, but a
// directory walk should not claim them unasked. Naming one directly still works.
var audioExts = map[string]bool{
	".flac": true, ".wav": true, ".mp3": true, ".m4a": true, ".aac": true,
	".opus": true, ".ogg": true, ".alac": true, ".mka": true, ".webm": true,
	".aiff": true, ".aif": true, ".aifc": true, ".afc": true,
	".oga": true, ".mp4": true, ".m4b": true, ".mkv": true,
}

// collectAudioInputs returns recognized audio files under root in sorted order.
// Recursive walks do not follow directory symlinks. excludeDir is omitted from a
// recursive walk so an output directory beneath root is not processed as input.
// Unrecognized regular files contribute to ignored; directories and other file
// types do not.
//
// A walk skips hidden entries: macOS writes AppleDouble stubs (._Track.wav) that
// carry an audio extension but no audio, and metadata directories (.Trashes,
// .Spotlight-V100) hold more of them. A hidden file the walk reaches counts as
// ignored; a hidden directory is skipped whole and its contents are not counted,
// since descending a metadata directory to tally it would be work spent to
// inflate a number. Naming a hidden directory as root still walks it, the same
// escape hatch audioExts documents; a hidden file named directly never reaches
// here, since only a directory argument starts a batch.
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
		// A dotfile is counted, not silently dropped: it was present and not
		// processed, the same as a file with an unrecognized extension.
		if strings.HasPrefix(d.Name(), ".") {
			ignored++
			return
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
				// A hidden directory is skipped whole, but only below the root: the
				// user may have named a hidden directory (or "." itself, whose
				// d.Name() is "."). The comparison is exact because WalkDir passes
				// root verbatim for the root entry and filepath.Join, which cleans,
				// for every child.
				if path != root && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
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
		return "" // WAV, AIFF, copy, and unknown formats cannot be confirmed as matches.
	}
}

// matchesTargetFamily reports whether a probed codec is one that tf produces.
// Formats without a stable codec family, such as WAV, AIFF, and copy, return
// false so single-file and batch planning use the same conservative rule.
func matchesTargetFamily(codec string, tf waxtap.TranscodeFormat) bool {
	fam := targetCodecFamily(tf)
	return fam != "" && format.CodecFamily(codec) == fam
}

// specChangesAudio reports whether the spec requires rewriting a file whose codec
// already matches the target. Such a file is otherwise left alone, copied
// through, or remuxed, which would silently drop what was asked for.
// srcChannels is the probed source channel count; 0 means unknown and answers
// conservatively.
func specChangesAudio(spec waxtap.ProcessSpec, srcChannels int) bool {
	return audioChangeIsCertain(spec) || foldsChannels(spec, srcChannels)
}

// audioChangeIsCertain reports the half of specChangesAudio the source cannot
// affect: a loudness pass, or an encoding knob the target's encoder reads.
// Probing is pointless once this is true, so planBatchOutputs gates on it.
//
// A knob the encoder never reads is not a request to rewrite: honoring it would
// spend a re-encode, and a generation of loss on a lossy target, on a value the
// same run reports as ignored. knobConditional does count, since a promoted copy
// honors it.
func audioChangeIsCertain(spec waxtap.ProcessSpec) bool {
	if spec.Loudness != nil {
		return true
	}
	t := spec.Transcode
	if t == nil {
		return false
	}
	return (t.Bitrate > 0 && bitrateEffect(t.Format).effect != knobIgnored) ||
		(t.BitDepth > 0 && bitDepthEffect(t.Format).effect != knobIgnored)
}

// foldsChannels reports whether --downmix has anything to fold: a source with
// more channels than the requested layout. A fold that matches the source is a
// no-op the engine skips anyway (the pipeline computes fold = 0 from the same
// comparison), so re-encoding for it costs a generation and delivers the same
// audio.
//
// Unlike the knobs, this cannot be answered from the spec alone, so it is decided
// per file. An unknown count (0, from a probe that failed or reported nothing)
// keeps the old answer: assume the fold is real.
func foldsChannels(spec waxtap.ProcessSpec, srcChannels int) bool {
	if !spec.Downmix {
		return false
	}
	target := spec.Channels.ChannelCount()
	return target == 0 || srcChannels <= 0 || srcChannels > target
}

// extPossiblyCodec reports whether ext can contain the given codec family. It
// only filters probe candidates; every possible match is still confirmed with
// a probe. General-purpose and unknown containers return true.
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
	case ".m4a", ".m4b":
		return family == "aac" || family == "alac"
	case ".ogg", ".oga":
		return family == "vorbis" || family == "opus" || family == "flac"
	case ".webm":
		return family == "opus" || family == "vorbis"
	case ".wav", ".aiff", ".aif", ".aifc", ".afc":
		return false // PCM is not one of the comparable target families.
	case ".mp4", ".mkv":
		// The video spellings of MP4 and Matroska. A match makes a file a
		// copy-through candidate, and a copy-through delivers the whole container.
		// WaxFlow reports only audio tracks, so a probe cannot separate an audio-only
		// .mp4 from a movie whose audio is AAC, and the movie would reach the output
		// directory untouched. Declining sends them to the encoder, which extracts
		// the audio. The audio-only spellings stay copy-eligible.
		return false
	default:
		return true // Probe general-purpose and unknown containers.
	}
}

// batchProbeAudio probes candidate files in parallel. Files that cannot be left
// unchanged are not probed, and failed probes are omitted from the result.
func batchProbeAudio(ctx context.Context, inputs []string, family string, skip bool, probeAudio func(context.Context, string) (waxtap.AudioProbe, error)) map[string]waxtap.AudioProbe {
	probes := make(map[string]waxtap.AudioProbe)
	if skip || family == "" {
		return probes
	}
	var todo []string
	for _, in := range inputs {
		if extPossiblyCodec(strings.ToLower(filepath.Ext(in)), family) {
			todo = append(todo, in)
		}
	}
	if len(todo) == 0 {
		return probes
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
			if p, err := probeAudio(ctx, in); err == nil {
				mu.Lock()
				probes[in] = p
				mu.Unlock()
			}
		}(in)
	}
	wg.Wait()
	return probes
}

// planBatchOutputs creates jobs for a transcode or normalize-apply batch. It
// applies collision policy, rejects outputs that would overwrite another input,
// and rejects multiple inputs that map to the same output. A file the spec would
// not change is left in place or copied unchanged into dir, unless force is set.
func planBatchOutputs(ctx context.Context, inputs []string, root, dir string, recursive bool, tf waxtap.TranscodeFormat, spec waxtap.ProcessSpec, mode collisionMode, force bool, tag string, probeAudio func(context.Context, string) (waxtap.AudioProbe, error)) ([]batchJob, error) {
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

	// Probe candidates before planning so the probes can run in parallel. A failed
	// probe leaves the file scheduled for normal processing. The gate is the
	// source-independent half of the question: --downmix is settled per file
	// below, from the count the probe reports.
	probes := batchProbeAudio(ctx, inputs, fam, force || audioChangeIsCertain(spec), probeAudio)

	jobs := make([]batchJob, 0, len(inputs))
	for i, in := range inputs {
		noop := false
		if p, ok := probes[in]; ok && matchesTargetFamily(p.Codec, tf) && !specChangesAudio(spec, p.Channels) {
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
