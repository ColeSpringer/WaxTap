package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/media/loudness"
	"github.com/colespringer/waxtap/v3/internal/pipeline"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// Process runs the transcode/cut/normalize pipeline on a local file, with no
// YouTube access, through the same source-agnostic pipeline as Download.
// SponsorBlock is not used here: it is keyed by video ID, which a local file does
// not have, so only explicit Cut.Ranges apply.
//
// The input is validated up front (probed); a corrupt or non-audio file fails
// with ErrUnsupportedInput. Writing the output over the input is rejected unless
// the caller targets a different path.
//
// Callers may omit Output only for pure loudness measurement: LoudnessMeasureOnly
// with no transcode, downmix, or cut. Client.Measure wraps that case.
func (c *Client) Process(ctx context.Context, req ProcessRequest) (res *Result, err error) {
	em := newEmitter(req.Events, "")
	defer func() { em.finish(res, err) }()

	if req.Input == "" {
		return nil, fmt.Errorf("waxtap.Process: Input is required")
	}
	if req.Output.kind == outputNone && !isMeasureOnlySpec(req.ProcessSpec) {
		return nil, fmt.Errorf("waxtap.Process: an Output is required")
	}
	if err := validateProcessSpec(req.ProcessSpec); err != nil {
		return nil, err
	}
	if req.Output.kind == outputFile {
		if sameFile(req.Output.path, req.Input) {
			return nil, fmt.Errorf("%w: output path equals input path", waxerr.ErrIncompatibleSpec)
		}
		if req.SkipIfExists && fileExists(req.Output.path) {
			em.stage(StageSkipped)
			return &Result{SourceKind: SourceLocalFile, InputPath: req.Input, OutputPath: req.Output.path}, nil
		}
		if err := ensureParentDir(req.Output.path); err != nil {
			return nil, err
		}
	}

	runner := c.engine()

	srcExt := filepath.Ext(req.Input)
	pipeOut := req.Output.path
	if req.Output.kind == outputWriter {
		// Writer output needs a staging file; direct file output and pure measurement
		// do not.
		jobDir, err := c.makeJobDir()
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(jobDir)
		pipeOut = filepath.Join(jobDir, "output"+outputExt(req.Transcode, srcExt))
	}
	// An exclusive file output stages beside the destination instead: the publish
	// below, not the pipeline's own write, is what claims the path.
	staging, err := stageExclusive(req.Output)
	if err != nil {
		return nil, err
	}
	if staging != nil {
		defer staging.Discard() // no-op after a successful publish
		pipeOut = staging.Path()
	}

	em.stage(StageStaging)
	ranges := cutRanges(processRanges(req.Cut))

	pres, err := pipeline.Run(ctx, runner, req.Input, pipeOut, pipelineSpec(req.ProcessSpec, ranges), em.pipelineStage)
	if err != nil {
		return nil, err
	}
	// Local inputs have no SponsorBlock source, so no SponsorBlock segments were
	// returned.
	warnEmptyCut(em, req.Cut, pres, false)
	warnLoudnessTargetMissed(em, req.Loudness, pres)
	warnImplicitDownmix(em, req.ProcessSpec, pres)

	srcFmt := Format{
		Codec:     pres.SourceCodec,
		Extension: strings.TrimPrefix(srcExt, "."),
	}
	res = newProcessResult(SourceLocalFile, pres, srcFmt, loudnessTarget(req.Loudness))
	res.InputPath = req.Input

	deliver := pres.OutputPath
	measureOnly := deliver == ""
	if measureOnly {
		deliver = req.Input
	} else {
		// A WaxFlow rewrite carries no tags, so restore the input's own embedded
		// metadata onto the output before delivery (both sinks read deliver).
		// Measure-only runs deliver the input itself and must not edit it. No
		// re-encode and no cut means the output is a whole-file packet copy, the
		// one case where own-audio tags still hold.
		remuxed := !pres.Transcoded && !pres.Cut
		c.carryTags(ctx, req.Input, deliver, req.Output.path, appliedCutFrom(pres), remuxed, em)
	}

	em.stage(StageFinalizing)
	switch req.Output.kind {
	case outputFile:
		// Measure-only delivers the caller's own input, which must be copied and
		// never moved, so it is settled before the shared publish.
		if measureOnly {
			if err := copyFile(req.Input, req.Output.path, req.Output.exclusive); err != nil {
				return nil, err
			}
		} else if err := publishProduced(deliver, staging, req.Output); err != nil {
			return nil, err
		}
		res.OutputPath = req.Output.path
		res.OutputBytes = fileSize(req.Output.path)
	case outputWriter:
		n, err := streamFileTo(req.Output.writer, deliver)
		if err != nil {
			return nil, err
		}
		res.OutputBytes = n
	}
	// contentLength describes the delivered file. The pipeline's output probe
	// runs before carryTags rewrites the file, so the probe size can undercount
	// carried metadata. OutputBytes is stat'd after every write, so it is the
	// authority, same as the download path after its embed pass.
	if res.OutputBytes > 0 {
		res.OutputFormat.ContentLength = res.OutputBytes
	}
	res.SourceBytes = fileSize(req.Input)
	return res, nil
}

// processRanges returns the explicit removal ranges for a local-file process.
// SponsorBlock is ignored because there is no video ID.
func processRanges(cs *CutSpec) []TimeRange {
	if cs == nil {
		return nil
	}
	return cs.Ranges
}

// isMeasureOnlySpec reports whether Process can run without an Output. Any
// transcode, downmix, or cut writes audio, including FormatCopy remuxes.
func isMeasureOnlySpec(s ProcessSpec) bool {
	return s.Loudness != nil && s.Loudness.Mode == LoudnessMeasureOnly &&
		s.Transcode == nil && !s.Downmix && !cutRequested(s.Cut)
}

// ProbeCodec reports the codec name of the first audio stream in a local file,
// such as "opus" or "aac". It returns ErrUnsupportedInput when the file has no
// audio stream.
func (c *Client) ProbeCodec(ctx context.Context, path string) (string, error) {
	runner := c.engine()
	probe, err := runner.Probe(ctx, path)
	if err != nil {
		return "", err
	}
	audio, ok := probe.AudioStream()
	if !ok {
		return "", fmt.Errorf("%w: no audio stream in %s", ErrUnsupportedInput, path)
	}
	return audio.CodecName, nil
}

// AlbumLoudnessResult reports a group loudness measurement plus per-track
// measurements, in input order. The album value is a true group EBU R128
// measurement, not a mean of the per-track LUFS.
type AlbumLoudnessResult struct {
	Album    LoudnessInfo   // loudness measured across the complete album
	PerTrack []LoudnessInfo // measurements in input order
}

// Measure reports EBU R128 integrated loudness for a single local audio file. It
// uses Process with a measure-only spec and no Output, so no output or scratch
// file is created.
//
// Use MeasureAlbum to measure several files as one album, or Process with a
// LoudnessApply spec to normalize and write audio.
func (c *Client) Measure(ctx context.Context, path string) (LoudnessInfo, error) {
	res, err := c.Process(ctx, ProcessRequest{
		Input:       path,
		ProcessSpec: ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly}},
	})
	if err != nil {
		return LoudnessInfo{}, err
	}
	if res.Loudness == nil || res.Loudness.Input == nil {
		return LoudnessInfo{}, fmt.Errorf("waxtap.Measure: no loudness measured for %s", path)
	}
	return *res.Loudness.Input, nil
}

// MeasureAlbum measures local audio files as one album and also returns each
// track's loudness. It does not write output files; callers can use the album
// value for ReplayGain tags or playback gain.
//
// Use ProcessAlbum to measure the album and write normalized tracks.
func (c *Client) MeasureAlbum(ctx context.Context, paths []string) (*AlbumLoudnessResult, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("waxtap.MeasureAlbum: no inputs")
	}
	runner := c.engine()
	album, perTrack, err := loudness.MeasureAlbum(ctx, runner, paths)
	if err != nil {
		return nil, err
	}
	res := &AlbumLoudnessResult{
		Album:    loudnessInfo(album),
		PerTrack: make([]LoudnessInfo, len(perTrack)),
	}
	for i, l := range perTrack {
		res.PerTrack[i] = loudnessInfo(l)
	}
	return res, nil
}

// AlbumTrack names one album input and where its processed output should be
// written.
type AlbumTrack struct {
	Input  string // source file path
	Output string // destination file path
}

// AlbumProcessResult reports the album loudness, the gain applied to every track,
// the input measurements, and the output paths.
type AlbumProcessResult struct {
	Album LoudnessInfo // loudness measured across the complete album
	// GainDB is the gain applied to every track: Target - album integrated LUFS,
	// held under the album-wide true-peak clamp in PeakCap mode, and 0 for a silent
	// album.
	GainDB   float64
	PerTrack []LoudnessInfo // input measurements in track order
	Outputs  []string       // completed output paths in track order
	// Delivered is the loudness of the normalized album. It is measured, over the
	// written outputs, only where a measurement is the sole way to know it: a
	// boosting gain in PeakLimit mode, where the true-peak limiter gives back an
	// unpredictable part of the gain. Everywhere else it is derived, because the
	// delivered result is analytic: a capping gain leaves the limiter idle, and the
	// limiter never engages on an attenuating gain, so the album lands at
	// Album + GainDB with the peaks shifted by the same scalar and the range
	// unchanged. A derived value therefore describes a pure gain and does not
	// account for a lossy encoder overshooting the source true peak. It is nil for a
	// silent album, which no gain moves, and when a measurement was attempted and
	// failed.
	Delivered *LoudnessInfo
	Warnings  []Warning // non-fatal signals (loudness miss, metadata carry), across all tracks
}

// AlbumOption configures [Client.ProcessAlbum].
type AlbumOption func(*albumOptions)

type albumOptions struct{ peakMode PeakMode }

// WithAlbumPeakMode selects album peak protection. The default is [PeakLimit]:
// one gain aimed at the target, with WaxFlow's true-peak limiter guarding each
// track's peaks. [PeakCap] instead clamps the single gain by the least true-peak
// headroom across the album, which leaves the limiter idle and so reproduces the
// input's track-to-track spacing exactly, at the cost of landing short of the
// target whenever one track is already loud.
func WithAlbumPeakMode(m PeakMode) AlbumOption {
	return func(o *albumOptions) { o.peakMode = m }
}

// ProcessAlbum measures local files as one album, then applies the same gain to
// every track. The shared offset preserves track-to-track loudness differences;
// per-track normalization would flatten them.
//
// How exactly it preserves them depends on the peak mode. The default
// [PeakLimit] aims one gain at the target and lets WaxFlow's true-peak limiter
// guard each track, so a boosting gain is given back further on the loudest
// tracks and the spacing compresses; [WithAlbumPeakMode] with [PeakCap] clamps
// the gain album-wide instead, leaving the limiter idle and the spacing exact.
// An attenuating gain never engages the limiter at all, so the two modes agree
// there.
//
// Album processing requires a non-copy transcode format. A silent
// album applies a no-op gain, leaving each track unchanged apart from re-encoding.
// Each track's embedded metadata is carried onto its output, the same pass
// Process runs; carry losses accumulate in the result's Warnings, as does a
// delivered loudness that misses the target.
//
// Tracks are written by path rather than through an [Output], so each one
// publishes with a replacing rename: [ToNewFile]'s exclusive delivery is not
// available here, and two concurrent runs writing one album directory can still
// lose a track.
func (c *Client) ProcessAlbum(ctx context.Context, tracks []AlbumTrack, target float64, spec TranscodeSpec, opts ...AlbumOption) (*AlbumProcessResult, error) {
	if len(tracks) == 0 {
		return nil, fmt.Errorf("waxtap.ProcessAlbum: no inputs")
	}
	ao := albumOptions{peakMode: PeakLimit}
	for _, opt := range opts {
		opt(&ao)
	}
	if spec.Format == FormatCopy {
		return nil, fmt.Errorf("%w: album normalization requires an encode, not copy", waxerr.ErrIncompatibleSpec)
	}
	// Album processing always applies gain and does not build a ProcessSpec.
	if err := validateLoudness(&LoudnessSpec{Mode: LoudnessApply, Target: target}); err != nil {
		return nil, err
	}
	if err := validateBitrate(&spec); err != nil {
		return nil, err
	}
	if err := validateBitDepth(&spec); err != nil {
		return nil, err
	}
	codec := transcodeCodec(spec.Format)
	for _, t := range tracks {
		if t.Input == "" || t.Output == "" {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: each track needs an input and an output path")
		}
		// Album processing builds its own media.Spec, so it never reaches the
		// pipeline's container check. The CLI always names outputs stem +
		// transcodeExt, but a library caller can pass any path.
		if err := media.CheckOutputContainer(codec, t.Output); err != nil {
			return nil, err
		}
	}
	// Validate the whole album before the first write. Otherwise one track could
	// replace another track's source, or two tracks could share an output path.
	for i, ti := range tracks {
		for j, tj := range tracks {
			if sameFile(ti.Output, tj.Input) {
				return nil, fmt.Errorf("%w: album output %q would overwrite track input %q", waxerr.ErrIncompatibleSpec, ti.Output, tj.Input)
			}
			if i < j && sameFile(ti.Output, tj.Output) {
				return nil, fmt.Errorf("%w: album tracks %d and %d share output %q", waxerr.ErrIncompatibleSpec, i, j, ti.Output)
			}
		}
	}

	runner := c.engine()

	inputs := make([]string, len(tracks))
	for i, t := range tracks {
		inputs[i] = t.Input
	}
	album, perTrack, err := loudness.MeasureAlbum(ctx, runner, inputs)
	if err != nil {
		return nil, err
	}

	// One uniform gain for the whole album, capped or limited per the peak mode.
	//
	// This deliberately stays a single pass, unlike the per-track PeakLimit path
	// in internal/pipeline, which measures its output and corrects the gain onto
	// the target. Correcting per track here would reintroduce exactly the
	// per-track variation album mode removes; correcting the album gain as a
	// whole would mean re-encoding every track on each iteration. A limiting album
	// therefore lands wherever the limiter leaves it, which is what the delivered
	// measurement below reports and warns about.
	tspec := media.Spec{
		Codec:    codec,
		Bitrate:  spec.Bitrate,
		BitDepth: spec.BitDepth,
		GainDB:   loudness.AlbumGain(target, album, perTrack, ao.peakMode == PeakCap),
	}

	res := &AlbumProcessResult{
		Album:    loudnessInfo(album),
		GainDB:   tspec.GainDB,
		PerTrack: make([]LoudnessInfo, len(perTrack)),
		Outputs:  make([]string, len(tracks)),
	}
	for i, l := range perTrack {
		res.PerTrack[i] = loudnessInfo(l)
	}
	em := newEmitter(nil, "")
	var fold albumFold
	for i, t := range tracks {
		if err := ensureParentDir(t.Output); err != nil {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: track %d (%s): %w", i, t.Input, err)
		}
		// Album mode writes through runner.Transcode rather than the pipeline, so
		// nothing here computes the probes warnImplicitDownmix reads. Without this
		// the fold is doubly silent, since the engine's own log line is demoted.
		srcCh := probeChannels(ctx, runner, t.Input)
		if _, err := runner.Transcode(ctx, t.Input, t.Output, tspec); err != nil {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: track %d (%s): %w", i, t.Input, err)
		}
		fold.observe(srcCh, probeChannels(ctx, runner, t.Output))
		// Album tracks are always re-encoded, so no cut remap and no own-audio
		// restore apply. Carried ReplayGain would be wrong twice over here: the
		// gain just changed the loudness it describes.
		// Album tracks are written straight to their destinations, so the file
		// warnings name is the file that was written.
		c.carryTags(ctx, t.Input, t.Output, t.Output, nil, false, em)
		res.Outputs[i] = t.Output
	}
	fold.warn(em, codec)
	res.Delivered = albumDelivered(ctx, runner, res.Outputs, album, tspec.GainDB, ao.peakMode)
	warnAlbumTargetMissed(em, target, ao.peakMode, album, perTrack, res.Delivered)
	res.Warnings = em.collected()
	return res, nil
}

// albumFold records the widest channel fold seen across an album's tracks, so one
// warning covers the album instead of one per track.
type albumFold struct{ src, out int }

func (f *albumFold) observe(src, out int) {
	if src <= 0 || out <= 0 || out >= src {
		return
	}
	if src-out > f.src-f.out {
		f.src, f.out = src, out
	}
}

func (f *albumFold) warn(em *emitter, c media.Codec) {
	if f.src == 0 {
		return
	}
	em.warn(WarnImplicitDownmix, fmt.Sprintf(
		"%s cannot hold %d channels, so the encode folded them to %d; album mode has no --downmix, so pick a format that keeps the layout to avoid the fold",
		c, f.src, f.out))
}

// probeChannels reports a file's channel count, or 0 when it cannot be probed.
// It is best-effort on purpose: it exists to describe a fold, and failing to
// describe one must not fail the album.
func probeChannels(ctx context.Context, r *media.Runner, path string) int {
	pr, err := r.Probe(ctx, path)
	if err != nil {
		return 0
	}
	if a, ok := pr.AudioStream(); ok {
		return a.Channels
	}
	return 0
}

// albumDelivered reports the loudness of the normalized album, measuring it only
// where measurement is the only way to know it: a boosting gain that the
// true-peak limiter is free to give part of back. A capping gain holds every
// track under the ceiling, and no gain at or below zero engages the limiter at
// all, so both land analytically at album + gain. See
// AlbumProcessResult.Delivered.
//
// Deriving the analytic cases is not a shortcut: the measurement is a second
// decode of every track in the album, and running it to confirm an answer that is
// already known would double the cost of the common case.
func albumDelivered(ctx context.Context, runner *media.Runner, outputs []string, album loudness.Loudness, gain float64, mode PeakMode) *LoudnessInfo {
	if !album.Finite() {
		return nil // silence: no gain was applied and none would change it
	}
	if mode == PeakCap || gain <= 0 {
		// The limiter is idle, so the album is the measured album shifted by a
		// scalar: the loudness and both peaks move with the gain, the range does not.
		return &LoudnessInfo{
			IntegratedLUFS: album.IntegratedLUFS + gain,
			TruePeakDBTP:   album.TruePeakDBTP + gain,
			LRA:            album.LRA,
			SamplePeakDB:   album.SamplePeakDB + gain,
		}
	}
	// Best-effort, like the pipeline's post-measure: every track is already
	// written, so a failed measurement must not fail the album.
	med, closer, err := runner.OpenAlbumConcat(outputs)
	if err != nil {
		return nil
	}
	defer closer()
	out, err := runner.AnalyzeMedia(ctx, med, 0)
	if err != nil {
		return nil
	}
	return &LoudnessInfo{
		IntegratedLUFS: out.IntegratedLUFS,
		TruePeakDBTP:   out.TruePeakDB,
		LRA:            out.LoudnessRange,
		SamplePeakDB:   out.SamplePeakDB,
	}
}
