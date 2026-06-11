package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxtap/internal/pipeline"
	"github.com/colespringer/waxtap/normalize"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/waxerr"
)

// Process runs the transcode/cut/normalize pipeline on a local file, with no
// YouTube access, through the same source-agnostic pipeline as Download.
// SponsorBlock is not used here: it is keyed by video ID, which a local file does
// not have, so only explicit Cut.Ranges apply.
//
// The input is validated up front (ffprobe); a corrupt or non-audio file fails
// with ErrUnsupportedInput. Writing the output over the input is rejected unless
// the caller targets a different path.
func (c *Client) Process(ctx context.Context, req ProcessRequest) (res *Result, err error) {
	em := newEmitter(req.Events, "")
	defer func() { em.finish(res, err) }()

	if req.Input == "" {
		return nil, fmt.Errorf("waxtap.Process: Input is required")
	}
	if req.Output.kind == outputNone {
		return nil, fmt.Errorf("waxtap.Process: an Output is required")
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

	runner, err := c.ffmpeg()
	if err != nil {
		return nil, err
	}

	jobDir, err := os.MkdirTemp(c.opts.TempDir, "waxtap-job-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(jobDir)

	srcExt := filepath.Ext(req.Input)
	pipeOut := req.Output.path
	if req.Output.kind == outputWriter {
		pipeOut = filepath.Join(jobDir, "output"+outputExt(req.Transcode, srcExt))
	}

	em.stage(StageStaging)
	ranges := cutRanges(processRanges(req.Cut))

	pres, err := pipeline.Run(ctx, runner, req.Input, pipeOut, pipelineSpec(req.ProcessSpec, ranges), em.pipelineStage)
	if err != nil {
		return nil, err
	}

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
	}

	em.stage(StageFinalizing)
	switch req.Output.kind {
	case outputFile:
		switch {
		case deliver == req.Output.path:
			// The pipeline wrote the destination directly.
		case measureOnly:
			// Measure-only: copy the unchanged input (never move it).
			if err := copyFile(req.Input, req.Output.path); err != nil {
				return nil, err
			}
		default:
			if err := moveFile(deliver, req.Output.path); err != nil {
				return nil, err
			}
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

// AlbumLoudnessResult reports a group loudness measurement plus per-track
// measurements, in input order. The album value is a true group EBU R128
// measurement, not a mean of the per-track LUFS.
type AlbumLoudnessResult struct {
	Album    LoudnessInfo
	PerTrack []LoudnessInfo
}

// MeasureAlbum measures local audio files as one album and also returns each
// track's loudness. It does not write output files; callers can use the album
// value for ReplayGain tags or playback gain.
//
// It requires ffmpeg. Use normalize.AlbumGainFilter to bake the same album gain
// into each track.
func (c *Client) MeasureAlbum(ctx context.Context, paths []string) (*AlbumLoudnessResult, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("waxtap.MeasureAlbum: no inputs")
	}
	runner, err := c.ffmpeg()
	if err != nil {
		return nil, err
	}
	album, perTrack, err := normalize.MeasureAlbum(ctx, runner, paths)
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
	Input  string
	Output string
}

// AlbumProcessResult reports the album loudness, the gain applied to every track,
// the input measurements, and the output paths.
type AlbumProcessResult struct {
	Album    LoudnessInfo
	GainDB   float64 // Target - album integrated LUFS, applied to every track (0 for a silent album)
	PerTrack []LoudnessInfo
	Outputs  []string
}

// ProcessAlbum measures local files as one album, then bakes the same gain into
// every track. The shared offset preserves track-to-track loudness differences;
// per-track normalization would flatten them.
//
// Album processing requires ffmpeg and a non-copy transcode format. A silent
// album bakes a no-op gain, leaving each track unchanged apart from re-encoding.
func (c *Client) ProcessAlbum(ctx context.Context, tracks []AlbumTrack, target float64, spec TranscodeSpec) (*AlbumProcessResult, error) {
	if len(tracks) == 0 {
		return nil, fmt.Errorf("waxtap.ProcessAlbum: no inputs")
	}
	if spec.Format == FormatCopy {
		return nil, fmt.Errorf("%w: album normalization requires an encode, not copy", waxerr.ErrIncompatibleSpec)
	}
	for _, t := range tracks {
		if t.Input == "" || t.Output == "" {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: each track needs an input and an output path")
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

	runner, err := c.ffmpeg()
	if err != nil {
		return nil, err
	}

	inputs := make([]string, len(tracks))
	for i, t := range tracks {
		inputs[i] = t.Input
	}
	album, perTrack, err := normalize.MeasureAlbum(ctx, runner, inputs)
	if err != nil {
		return nil, err
	}

	tspec := transcode.Spec{
		Codec:   transcodeCodec(spec.Format),
		Bitrate: spec.Bitrate,
		Filters: []string{normalize.AlbumGainFilter(target, album.IntegratedLUFS)},
	}

	res := &AlbumProcessResult{
		Album:    loudnessInfo(album),
		PerTrack: make([]LoudnessInfo, len(perTrack)),
		Outputs:  make([]string, len(tracks)),
	}
	if album.Finite() {
		res.GainDB = target - album.IntegratedLUFS
	}
	for i, l := range perTrack {
		res.PerTrack[i] = loudnessInfo(l)
	}
	for i, t := range tracks {
		if err := ensureParentDir(t.Output); err != nil {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: track %d (%s): %w", i, t.Input, err)
		}
		if _, err := runner.Transcode(ctx, t.Input, t.Output, tspec); err != nil {
			return nil, fmt.Errorf("waxtap.ProcessAlbum: track %d (%s): %w", i, t.Input, err)
		}
		res.Outputs[i] = t.Output
	}
	return res, nil
}
