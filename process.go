package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
		// Ahead of the skip check: a directory at "d/a.wav/" stats as existing,
		// and skip would answer "already done" to a path that never named a file.
		if err := rejectSeparatorPath(req.Output.path); err != nil {
			return nil, err
		}
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

	pspec := pipelineSpec(req.ProcessSpec, ranges)
	pres, err := pipeline.Run(ctx, runner, req.Input, pipeOut, pspec, em.pipelineStage)
	if err != nil {
		return nil, err
	}
	// Local inputs have no SponsorBlock source, so no SponsorBlock segments were
	// returned.
	warnEmptyCut(em, req.Cut, pres, false)
	warnLoudnessTargetMissed(em, req.Loudness, pres)
	warnImplicitDownmix(em, req.ProcessSpec, pres)
	warnImplicitLossy(em, req.ProcessSpec, pres)
	warnOutputClipping(em, req.Loudness, pres)
	warnInputDamage(em, pres)
	warnEmptyInput(em, pres)
	warnLoudnessUnmeasurable(em, pres)

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
		res.TagCarry = c.carryTags(ctx, req.Input, deliver, req.Output.path, appliedCutFrom(pres), remuxed, em)
	}

	em.stage(StageFinalizing)
	switch req.Output.kind {
	case outputFile:
		// Measure-only delivers the caller's own input, which must be copied and
		// never moved, so it is settled before the shared publish.
		var published string
		var perr error
		if measureOnly {
			published, perr = copyFile(req.Input, req.Output.path, req.Output)
		} else {
			published, perr = publishProduced(deliver, staging, req.Output)
		}
		if perr != nil {
			return nil, perr
		}
		// A renumbering output lands beside the requested path, so the truthful
		// path is the publish's, not the request's.
		res.OutputPath = published
		res.OutputBytes = fileSize(published)
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

// AudioProbe describes the first audio stream in a local file. It carries what a
// caller needs in order to tell whether processing would change the audio at all,
// rather than the whole probe: the codec the file is already in, and the layout
// it already has.
type AudioProbe struct {
	// Codec is the codec name, such as "opus" or "aac".
	Codec string
	// Channels is the channel count, or 0 when the probe did not report one.
	Channels int
}

// ProbeAudio reports the first audio stream in a local file. It returns
// ErrUnsupportedInput when the file has no audio stream.
func (c *Client) ProbeAudio(ctx context.Context, path string) (AudioProbe, error) {
	runner := c.engine()
	probe, err := runner.Probe(ctx, path)
	if err != nil {
		return AudioProbe{}, err
	}
	audio, ok := probe.AudioStream()
	if !ok {
		return AudioProbe{}, fmt.Errorf("%w: no audio stream in %s", ErrUnsupportedInput, path)
	}
	return AudioProbe{Codec: audio.CodecName, Channels: audio.Channels}, nil
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
		return nil, albumTrackError(err, paths)
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
	GainDB float64
	// LoudnessApplied says the album gain actually reached the encodes. It is
	// false when the album's integrated loudness could not be measured: no gain
	// can be derived from one, so every track is a plain re-encode and GainDB's
	// 0 is the absence of a gain rather than a gain of nothing.
	LoudnessApplied bool
	PerTrack        []LoudnessInfo // input measurements in track order
	Outputs         []string       // completed output paths in track order
	// TagCarry itemizes each track's metadata carry, in track order; a nil
	// entry is a track whose carry did not run (nothing to carry). See TagCarry.
	TagCarry []*TagCarry
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
		return nil, albumTrackError(err, inputs)
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
		Album:  loudnessInfo(album),
		GainDB: tspec.GainDB,
		// Same predicate AlbumGain guarded on: below it the gain is 0 because
		// none could be derived, not because the album was already on target.
		LoudnessApplied: loudness.Gainable(album.IntegratedLUFS),
		PerTrack:        make([]LoudnessInfo, len(perTrack)),
		Outputs:         make([]string, len(tracks)),
		TagCarry:        make([]*TagCarry, len(tracks)),
	}
	for i, l := range perTrack {
		res.PerTrack[i] = loudnessInfo(l)
	}
	em := newEmitter(nil, "")
	var fold albumFold
	var levels albumLevels
	var carry albumCarryWarns
	damaged := albumInputRemarks{code: WarnInputDamage}
	noted := albumInputRemarks{code: WarnInputNote}
	var empty albumEmptyWarns
	for i, t := range tracks {
		if err := ensureParentDir(t.Output); err != nil {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: track %d (%s): %w", i, t.Input, err)
		}
		// Album mode writes through runner.Transcode rather than the pipeline, so
		// nothing here computes the probes warnImplicitDownmix reads. Without this
		// the fold is doubly silent, since the engine's own log line is demoted.
		in := probeAudio(ctx, runner, t.Input)
		empty.observe(t.Input, in.empty)
		tres, err := runner.Transcode(ctx, t.Input, t.Output, tspec)
		if err != nil {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: track %d (%s): %w", i, t.Input, err)
		}
		// The probe's damage list covers the headers; the measurement's and
		// the encode's cover the whole read, the probe's entries included.
		damaged.observe(t.Input, inputDamageNote(mergeRemarks(in.damage, perTrack[i].Warnings, tres.InputWarnings)))
		noted.observe(t.Input, inputNote(in.notes))
		levels.observe(t.Output, in.codec, tres.Levels)
		fold.observe(in.channels, probeAudio(ctx, runner, t.Output).channels)
		// Album tracks are always re-encoded, so no cut remap and no own-audio
		// restore apply. Carried ReplayGain would be wrong twice over here: the
		// gain just changed the loudness it describes.
		// Album tracks are written straight to their destinations, so the file
		// warnings name is the file that was written. The carry warnings run
		// through a per-track emitter so the album can fold them into one.
		tem := newEmitter(nil, "")
		res.TagCarry[i] = c.carryTags(ctx, t.Input, t.Output, t.Output, nil, false, tem)
		carry.observe(em, tem.collected())
		res.Outputs[i] = t.Output
	}
	fold.warn(em, codec)
	levels.warn(em, ao.peakMode)
	carry.warn(em)
	damaged.warn(em)
	noted.warn(em)
	empty.warn(em)
	res.Delivered = albumDelivered(ctx, runner, res.Outputs, album, tspec.GainDB, ao.peakMode)
	warnAlbumTargetMissed(em, target, ao.peakMode, album, perTrack, res.Delivered)
	// The album figure is one measurement over the whole timeline, and the
	// meter reports how much of it it read.
	// The album measurement has no frames behind it only when every track was
	// empty; one empty track among full ones leaves a measurable album.
	if cause := unmeasurableLoudnessCause(album.Duration, empty.all(len(tracks)), &album); cause != "" {
		em.warn(WarnLoudnessUnmeasurable, "album integrated loudness could not be measured: "+cause)
	}
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

// albumInputRemarks folds per-track input remarks of one class (damage, or the
// engine's notes on a well-formed file) into one album warning under code, the
// same way every other ProcessAlbum warning aggregates: the first track's
// detail leads and the rest are counted, so a batch of damaged rips does not
// bury the summary under one warning per file. observe takes the track's
// rendered detail, "" for none, so the class's wording stays with the
// single-file renderers (inputDamageNote, inputNote).
type albumInputRemarks struct {
	code  WarningCode
	first string
	n     int
}

func (a *albumInputRemarks) observe(track, detail string) {
	if detail == "" {
		return
	}
	a.n++
	if a.n == 1 {
		// Named per track: an album warning that did not say which file is
		// damaged would send the listener through the whole record to find it.
		a.first = filepath.Base(track) + ": " + detail
	}
}

func (a *albumInputRemarks) warn(em *emitter) {
	switch {
	case a.n == 0:
		return
	case a.n == 2:
		a.first += " (and 1 more track)"
	case a.n > 2:
		a.first += fmt.Sprintf(" (and %d more tracks)", a.n-1)
	}
	em.warn(a.code, a.first)
}

// albumEmptyWarns folds per-track empty inputs into one album warning, the same
// shape as albumDamageWarns: the first empty track is named and the rest are
// counted, so the listener has a place to start without one warning per file.
type albumEmptyWarns struct {
	first string
	n     int
}

func (a *albumEmptyWarns) observe(track string, empty bool) {
	if !empty {
		return
	}
	a.n++
	if a.n == 1 {
		a.first = filepath.Base(track)
	}
}

// all reports whether every one of n tracks came back empty, which is the only
// case where the album measurement itself has no frames behind it.
func (a *albumEmptyWarns) all(tracks int) bool { return tracks > 0 && a.n == tracks }

func (a *albumEmptyWarns) warn(em *emitter) {
	if a.n == 0 {
		return
	}
	detail := a.first + " contains no audio frames; its output holds no audio"
	switch {
	case a.n == 2:
		detail += " (and 1 more track)"
	case a.n > 2:
		detail += fmt.Sprintf(" (and %d more tracks)", a.n-1)
	}
	em.warn(WarnEmptyInput, detail)
}

// albumCarryWarns folds the per-track tag-carry warnings into one album
// warning, the albumFold rule: an album into WavPack drops the same chapters
// on every track, and one warning per track would print the near-identical
// sentence N times. The first track's detail speaks for the album, with a
// count of the others; any other warning code a track emits passes through
// unfolded.
type albumCarryWarns struct {
	first string
	n     int
}

func (a *albumCarryWarns) observe(em *emitter, ws []Warning) {
	for _, w := range ws {
		if w.Code != WarnTagCarry {
			em.warn(w.Code, w.Detail)
			continue
		}
		a.n++
		if a.n == 1 {
			a.first = w.Detail
		}
	}
}

func (a *albumCarryWarns) warn(em *emitter) {
	switch {
	case a.n == 0:
		return
	case a.n == 2:
		a.first += " (and 1 more track)"
	case a.n > 2:
		a.first += fmt.Sprintf(" (and %d more tracks)", a.n-1)
	}
	em.warn(WarnTagCarry, a.first)
}

// albumLevels aggregates per-track level measurements into one album warning,
// the way albumFold folds the downmix observation: the worst track speaks for
// the album, with a count of the others that clipped. A track whose decode
// can overshoot never counts, for the reason warnOutputClipping gives.
type albumLevels struct {
	worst     media.Levels
	worstPath string
	n         int
}

func (a *albumLevels) observe(path, srcCodec string, l media.Levels) {
	if l.Note() == "" || decodeOvershoots(srcCodec) {
		return
	}
	a.n++
	if a.n == 1 || worseLevels(l, a.worst) {
		a.worst, a.worstPath = l, path
	}
}

// worseLevels orders two level measurements by damage: more clamped samples,
// then a higher true peak when neither clamped.
func worseLevels(l, r media.Levels) bool {
	if l.ClippedSamples != r.ClippedSamples {
		return l.ClippedSamples > r.ClippedSamples
	}
	return l.TruePeak > r.TruePeak
}

func (a *albumLevels) warn(em *emitter, mode PeakMode) {
	if a.n == 0 {
		return
	}
	detail := a.worstPath + ": " + a.worst.Note()
	if a.n == 2 {
		detail += " (and 1 more track)"
	} else if a.n > 2 {
		detail += fmt.Sprintf(" (and %d more tracks)", a.n-1)
	}
	// The album always normalizes, so the single-file remedy split reduces to
	// its applied half: point at the peak mode, or at nothing.
	if mode == PeakLimit {
		detail += clipRemedyCap
	}
	em.warn(WarnOutputClipping, detail)
}

// probeChannels reports a file's channel count, or 0 when it cannot be probed.
// It is best-effort on purpose: it exists to describe a fold, and failing to
// describe one must not fail the album.
// timelineMemberRe matches the index in WaxFlow's "timeline member N" album
// errors. Kept deliberately narrow: a non-match only means the error goes out
// without a filename, never that it goes missing.
var timelineMemberRe = regexp.MustCompile(`timeline member (\d+)`)

// albumTrackError names the track file behind a WaxFlow timeline error, which
// reports members by index. An album error that says "member 1" makes the user
// count their inputs; one that names the file does not. Anything the pattern
// does not match passes through unchanged.
func albumTrackError(err error, inputs []string) error {
	m := timelineMemberRe.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	idx, aerr := strconv.Atoi(m[1])
	if aerr != nil || idx < 0 || idx >= len(inputs) {
		return err
	}
	return fmt.Errorf("track %s: %w", filepath.Base(inputs[idx]), err)
}

// albumProbe is what album mode reads off a track's probe: enough to describe
// a fold, an empty input, and the input's damage and the engine's notes on it,
// all best-effort, because failing to describe a file must not fail the album.
type albumProbe struct {
	channels int
	codec    string
	damage   []string
	notes    []string
	empty    bool
}

func probeAudio(ctx context.Context, r *media.Runner, path string) albumProbe {
	pr, err := r.Probe(ctx, path)
	if err != nil {
		return albumProbe{}
	}
	p := albumProbe{damage: pr.Warnings, notes: pr.Notes}
	if a, ok := pr.AudioStream(); ok {
		// Exactly 0 frames. -1 means the container states no length, which is
		// not a claim that there is nothing there.
		p.channels, p.codec, p.empty = a.Channels, a.CodecName, a.Samples == 0
	}
	return p
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
	// written, so a failed measurement must not fail the album. No lengths
	// are handed over: the outputs were just encoded, so all but a raw ADTS
	// or a Matroska state a countable length, and the opener walks and
	// counts those two itself. The write loop's Levels.Samples is not that
	// number for ADTS, whose container carries no gapless trim, so the walk
	// delivers the encoder's priming and padding on top of it.
	med, closer, err := runner.OpenAlbumConcat(ctx, outputs, nil)
	if err != nil {
		return nil
	}
	defer closer()
	out, err := runner.AnalyzeMedia(ctx, med, "", 0)
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
