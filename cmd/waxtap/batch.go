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
	// .wma is WMA's audio spelling; .asf, the same container, is left out like
	// the other loose spellings above since it usually names video.
	".wv": true, ".ape": true, ".wma": true,
}

// collectAudioInputs returns recognized audio files under root in sorted order.
// excludeDir is omitted from a recursive walk so an output directory beneath
// root is not processed as input. Unrecognized regular files contribute to
// ignored; directories and other file types do not.
//
// A symlink is resolved to what it points at, as a single named input already is
// (isLocalFile stats its argument). A link to a regular file is collected, or
// counted, by its own extension, exactly like the file it names: dropping it
// before the extension check left a linked-in album absent from the run and from
// the summary both. A link to a directory is counted rather than entered, in a
// recursive walk as much as a shallow one, so a walk still descends only real
// subdirectories and a link pointing at an ancestor is not a cycle anyone has to
// detect. A broken link is counted for the reason an unrecognized file is: it
// was there and it was not processed.
//
// The root is the one exception to counting a directory link: naming a link AS
// the directory to process is asking for what it points at, so the walk follows
// it, and every returned path is spelled under the name the caller used. That
// matches the shallow branch, which has always followed a linked root because
// os.ReadDir stats its argument. Links below the root stay counted; only the
// argument itself is an instruction.
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
		if a, e := filepath.Abs(excludeDir); e == nil && !sameDirThroughLinks(a, absRoot) {
			absExclude = a
		}
	}
	consider := func(path string, d fs.DirEntry) {
		if d.Type()&fs.ModeSymlink != 0 {
			// Stat the target instead of dropping the entry: a link to an audio file
			// is an audio file. A link to a directory, and a broken one, are counted
			// here rather than dropped, so neither leaves the run without a number.
			if fi, serr := os.Stat(path); serr != nil || !fi.Mode().IsRegular() {
				ignored++
				return
			}
		} else if !d.Type().IsRegular() {
			return // skip devices, sockets, and directories
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
		// WalkDir Lstats the final component of its root and does not follow a
		// symlink there, so a root that is itself a link to a directory used to
		// yield exactly one entry: the link, counted ignored. The shallow branch
		// above reads the same root through os.ReadDir, which follows the link,
		// so `transcode <link>` worked while `transcode <link> -r` found nothing.
		// The walk therefore runs over the resolved target, and every produced
		// path is re-spelled under the root the caller named, so the contract is
		// unchanged: inputs come back spelled the way the user typed them, which
		// is also what planBatchOutputs' literal relUnder mirroring needs. Only
		// the final component matters (Lstat follows intermediate links), and a
		// root that fails to resolve falls back to the literal walk it always got.
		wroot := root
		if fi, lerr := os.Lstat(root); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			if r := resolveDirPath(root); r != "" {
				wroot = r
			}
		}
		werr := filepath.WalkDir(wroot, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				// A hidden directory is skipped whole, but only below the root: the
				// user may have named a hidden directory (or "." itself, whose
				// d.Name() is "."). The comparison is exact because WalkDir passes
				// its root verbatim for the root entry and filepath.Join, which
				// cleans, for every child.
				if path != wroot && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				if absExclude != "" {
					if a, e := filepath.Abs(path); e == nil && sameDirThroughLinks(a, absExclude) {
						return filepath.SkipDir
					}
				}
				return nil
			}
			spelled := path
			if wroot != root {
				if rel, rerr := filepath.Rel(wroot, path); rerr == nil {
					spelled = filepath.Join(root, rel)
				}
			}
			consider(spelled, d)
			return nil
		})
		if werr != nil {
			return nil, 0, werr
		}
	}
	sort.Strings(inputs)
	return inputs, ignored, nil
}

// sameDirThroughLinks reports whether two absolute paths name one directory. It
// compares the literal spellings first and the symlink-resolved spellings
// second, so --dir and the walk naming one directory two ways still match.
//
// The order is the whole design. EvalSymlinks fails on a path that does not
// exist, and --dir most often names an output directory this run has yet to
// create, so resolution can only add matches: whenever either side fails to
// resolve, the answer is the plain absolute comparison this replaced. A rule
// that trusted resolution alone would treat two unresolvable paths as equal and
// skip the root of every run whose --dir does not exist yet.
func sameDirThroughLinks(a, b string) bool {
	if a == b {
		return true
	}
	ra, rb := resolveDirPath(a), resolveDirPath(b)
	return ra != "" && ra == rb
}

// resolveDirPath returns path as an absolute path with symlinks resolved, or ""
// when it cannot be resolved, which for a --dir value usually means it does not
// exist yet. Nothing in it is directory-specific; pathKeyer uses it on files
// too.
func resolveDirPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(resolved) // EvalSymlinks keeps a relative path relative.
	if err != nil {
		return ""
	}
	return abs
}

// pathKeyer computes the identity the planner's guards compare a path under:
// the symlink-resolved absolute path when the path is itself a link, else the
// resolved parent joined with the base name, else the plain absolute spelling.
//
// The tiering exists because the guards compare files that do not all exist
// yet. A planned output usually does not, but its directory often does (--dir
// naming an input directory through a link is exactly the case that used to
// slip past), so the parent resolves and the base rides along. A directory the
// run has yet to create resolves to nothing, and the plain absolute spelling is
// then the same comparison these guards have always made, so nothing that used
// to plan stops planning.
//
// Keying on identity rather than spelling is what makes the three guards mean
// what they say: "output would overwrite an input" is about the file, not about
// how the path was typed, and before this a --dir reaching the input's own
// directory through a link produced a misleading collision error under the
// default policy and a self-copy that rewrote the input in place under
// --collision overwrite.
//
// The parent resolution is memoized because a plan visits every input and every
// output and a library's files share a handful of directories: without the
// cache each key was a full symlink walk per path, tens of thousands of lstats
// before the first encode of a large batch. Each path itself costs one Lstat,
// which decides whether the full walk is needed at all (only for a path that is
// its own link).
type pathKeyer struct{ dirs map[string]string }

func newPathKeyer() *pathKeyer { return &pathKeyer{dirs: map[string]string{}} }

func (k *pathKeyer) key(path string) string {
	// Only a path that is itself a symlink needs the full resolution; for
	// everything else the identity is its (resolved) directory plus its name.
	// A broken link falls through to the parent tier like a missing file.
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if r := resolveDirPath(path); r != "" {
			return r
		}
	}
	return filepath.Join(k.dirKey(filepath.Dir(path)), filepath.Base(path))
}

func (k *pathKeyer) dirKey(dir string) string {
	if v, ok := k.dirs[dir]; ok {
		return v
	}
	v := resolveDirPath(dir)
	if v == "" {
		if abs, err := filepath.Abs(dir); err == nil {
			v = abs
		} else {
			v = dir
		}
	}
	k.dirs[dir] = v
	return v
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
	// mode is the run's collision policy, consumed by actCopy: processed items
	// carry theirs inside the Output the process function builds.
	mode collisionMode
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
	case waxtap.FormatHEAAC:
		return "he-aac"
	case waxtap.FormatMP3:
		return "mp3"
	case waxtap.FormatOpus:
		return "opus"
	case waxtap.FormatVorbis:
		return "vorbis"
	case waxtap.FormatWavPack:
		return "wavpack"
	case waxtap.FormatAPE:
		return "ape"
	default:
		return "" // WAV, AIFF, copy, and unknown formats cannot be confirmed as matches.
	}
}

// matchesTargetFamily reports whether a probed codec is one that tf produces.
// Formats without a stable codec family, such as WAV, AIFF, and copy, return
// false so single-file and batch planning use the same conservative rule.
//
// An aac target also matches an HE-AAC source: WaxFlow keeps such a copy under
// its own identity (its format=aac remux redirects to the he-aac row), so
// "already matches" here means the file is copied through rather than lossily
// re-encoded to AAC-LC, the same choice the engine makes. The reverse does not
// hold: an he-aac target on an AAC-LC source is a real (down)encode request.
func matchesTargetFamily(codec string, tf waxtap.TranscodeFormat) bool {
	fam := targetCodecFamily(tf)
	if fam == "" {
		return false
	}
	got := format.CodecFamily(codec)
	if tf == waxtap.FormatAAC && got == "he-aac" {
		return true
	}
	return got == fam
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
	if family == "he-aac" {
		family = "aac" // one container family; see media.ContainerAccepts
	}
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
	case ".wv":
		return family == "wavpack"
	case ".ape":
		return family == "ape"
	case ".wma":
		return false // decode-only: no target family ever produces WMA.
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
	// Guard keys are identities (pathKeyer), not spellings: the walk and --dir
	// can name one file two ways, and every rejection below is about the file.
	keyer := newPathKeyer()
	inputAbs := make(map[string]bool, len(inputs))
	absByInput := make(map[string]string, len(inputs))
	for _, in := range inputs {
		a := keyer.key(in)
		inputAbs[a] = true
		absByInput[in] = a
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

		absOut := keyer.key(out)
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
			jobs = append(jobs, batchJob{index: i, input: in, output: resolved, action: actCopy, mode: mode})
		default:
			jobs = append(jobs, batchJob{index: i, input: in, output: resolved, action: actProcess, mode: mode})
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

// copyThrough copies src to dst using the same staged-output path as other
// writes, honoring the run's collision policy at publish exactly as a processed
// item's Output does: fail claims exclusively, auto-number renumbers, the rest
// replace. It returns the path it published.
func copyThrough(src, dst string, mode collisionMode) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return "", tempfile.WrapOutput("mkdir", err)
	}
	tf, err := tempfile.New(dst)
	if err != nil {
		return "", err
	}
	defer tf.Discard()
	// Treat copy failures as output failures so the CLI can provide destination
	// directory guidance.
	if _, err := io.Copy(tf, in); err != nil {
		return "", tempfile.WrapOutput("copy", err)
	}
	switch mode {
	case collisionAutoNumber:
		return tf.CommitNewNumbered()
	case collisionFail:
		return dst, tf.CommitNew()
	default:
		return dst, tf.Commit()
	}
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
				if published, err := copyThrough(job.input, job.output, job.mode); err != nil {
					outcomes[idx].status, outcomes[idx].err = statusError, err
				} else {
					outcomes[idx].status, outcomes[idx].output = statusCopied, published
				}
			} else {
				res, err := processFn(ctx, job.input, job.output)
				if err != nil {
					outcomes[idx].status, outcomes[idx].err = statusError, err
				} else {
					outcomes[idx].status, outcomes[idx].result = statusOK, res
					// The pre-flight pick can be renumbered at publish; the
					// result carries the path actually written.
					if res != nil && res.OutputPath != "" {
						outcomes[idx].output = res.OutputPath
					}
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
	errs := make([]error, len(outcomes))
	for i, o := range outcomes {
		errs[i] = o.err
	}
	return worstClassifiedError(errs)
}
