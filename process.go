package waxtap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxtap/internal/pipeline"
	"github.com/colespringer/waxtap/normalize"
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

// MeasureAlbum measures a set of local audio files as an album: the group
// loudness plus each track individually. It is the non-destructive,
// ReplayGain-oriented path for library-wide loudness consistency; callers apply
// the derived album gain at playback or tag time.
//
// It requires ffmpeg. Use normalize.AlbumGainFilter when the same album gain
// should be baked into each track.
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
