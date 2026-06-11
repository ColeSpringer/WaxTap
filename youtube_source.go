package waxtap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/colespringer/waxtap/cut"
	"github.com/colespringer/waxtap/download"
	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/internal/httpx"
	"github.com/colespringer/waxtap/internal/pipeline"
	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/sponsorblock"
	"github.com/colespringer/waxtap/youtube"
)

// SponsorBlockSegments returns skip segments for videoURL using the client's
// SponsorBlock settings and shared HTTP client. It does not cut or download
// media.
func (c *Client) SponsorBlockSegments(ctx context.Context, videoURL string, categories []sponsorblock.Category) ([]sponsorblock.Segment, error) {
	id, err := youtube.ExtractVideoID(videoURL)
	if err != nil {
		return nil, err
	}
	d := c.opts.Timeouts.SponsorBlock
	if c.opts.SponsorBlock.Timeout > 0 {
		d = c.opts.SponsorBlock.Timeout
	}
	sbCtx, cancel := withTimeout(ctx, d)
	defer cancel()
	return c.sb.FetchSegments(sbCtx, id, categories)
}

// acquired contains the selected format and the backend that delivers it.
type acquired struct {
	video    *youtube.Video
	fmtSel   Format
	transfer mediaTransfer
}

// webContextCooldown is how long acquire skips the WEB player-context attempt
// after a provider failure, so a dead or hanging sidecar taxes a batch once
// per window instead of paying the full provider budget on every video.
const webContextCooldown = 30 * time.Second

// acquire extracts, selects, and resolves one video, then builds the appropriate
// transfer backend.
func (c *Client) acquire(ctx context.Context, req Request, id string, em *emitter) (*acquired, error) {
	target := transcodeTarget(req.Transcode)

	// Opt-in WEB path: when an attested player-context provider is configured, try
	// it first (full WEB audio, status 1), even over a forced Options.Client, whose
	// chain stays the fallback. On any failure, warn and fall back to the default
	// tokenless chain so the download still succeeds; the provider call is bounded
	// by Timeouts.WebContext inside the youtube client, so a dead provider can't
	// eat the fallback budget. Caller cancellation is propagated, never warned and
	// never "recovered" by running the fallback chain on a dead context.
	if c.yt.WebContextConfigured() && !c.webContextCoolingDown() {
		a, err := c.acquireWebContext(ctx, req, id, target, em)
		if err == nil {
			return a, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		em.warn(WarnWebContextFallback, "WEB player-context failed: "+err.Error()+"; falling back to the default client chain")
	}

	em.stage(StageExtracting)
	ectx, ecancel := withTimeout(ctx, c.opts.Timeouts.Extraction)
	defer ecancel()
	ext, err := c.yt.Extract(ectx, id)
	if err != nil {
		return nil, err
	}

	video, selFmt, plan, err := c.selectAndResolve(ctx, req, target, ext, em)
	if err != nil {
		return nil, err
	}

	// SABR streams handle their own refresh and retry behavior.
	if plan.SABR != nil {
		return &acquired{video: video, fmtSel: selFmt, transfer: sabrTransfer{dl: c.dl, handle: plan.SABR}}, nil
	}

	// Signed URL expiry lives in the player response. Refreshing after a 403/410
	// therefore starts with a new extraction, then resolves the originally chosen
	// itag so resumed byte ranges stay on the same encoding.
	pinnedItag := selFmt.Itag
	refresh := func(fctx context.Context, failure *potoken.HTTPFailure) (download.Source, error) {
		rext, rerr := func() (*youtube.Extraction, error) {
			fectx, cancel := withTimeout(fctx, c.opts.Timeouts.Extraction)
			defer cancel()
			return c.yt.Extract(fectx, id)
		}()
		if rerr != nil {
			return download.Source{}, rerr
		}
		ridx, rerr := selectIndex(Itag(pinnedItag), req.SourcePolicy, target, rext.Video().Formats)
		if rerr != nil {
			// The pinned itag is gone from the fresh extraction; fall back to the
			// original selector rather than failing the refresh outright.
			ridx, rerr = selectIndex(req.Audio, req.SourcePolicy, target, rext.Video().Formats)
			if rerr != nil {
				return download.Source{}, rerr
			}
		}
		rrctx, cancel := withTimeout(fctx, c.opts.Timeouts.Resolve)
		defer cancel()
		nplan, rerr := c.yt.ResolveWithFailure(rrctx, rext, ridx, failure)
		if rerr != nil {
			return download.Source{}, rerr
		}
		if nplan.Direct == nil {
			return download.Source{}, fmt.Errorf("waxtap: stream refresh resolved itag %d to SABR", pinnedItag)
		}
		em.warn(WarnURLReResolved, "stream URL re-resolved after expiry")
		return toSource(*nplan.Direct), nil
	}

	return &acquired{video: video, fmtSel: selFmt, transfer: urlTransfer{dl: c.dl, src: toSource(*plan.Direct), refresh: refresh}}, nil
}

// acquireWebContext builds the transfer from an attested WEB /player context.
// It always yields a SABR stream (the context's formats carry no direct URL), so
// there is no signed-URL refresh path here; a mid-stream reload re-fetches a
// fresh context (see SABRStream.reextract). Any error is returned so acquire can
// warn and fall back to the default chain. Only a context failure trips the
// provider cooldown: a per-video selection or resolve failure says nothing
// about the provider's health.
func (c *Client) acquireWebContext(ctx context.Context, req Request, id string, target format.Target, em *emitter) (*acquired, error) {
	em.stage(StageExtracting)
	ext, err := c.yt.ExtractWebContext(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			c.noteWebContextFailure()
		}
		return nil, err
	}
	c.noteWebContextSuccess()

	video, selFmt, plan, err := c.selectAndResolve(ctx, req, target, ext, em)
	if err != nil {
		return nil, err
	}
	if plan.SABR == nil {
		return nil, fmt.Errorf("WEB player-context did not resolve to a SABR stream")
	}
	return &acquired{video: video, fmtSel: selFmt, transfer: sabrTransfer{dl: c.dl, handle: plan.SABR}}, nil
}

// selectAndResolve picks the format for req and resolves its delivery plan,
// emitting the standard stage event. It is the shared tail of acquire's
// default chain and the WEB player-context path.
func (c *Client) selectAndResolve(ctx context.Context, req Request, target format.Target, ext *youtube.Extraction, em *emitter) (*youtube.Video, Format, youtube.MediaPlan, error) {
	video := ext.Video()
	idx, err := selectIndex(req.Audio, req.SourcePolicy, target, video.Formats)
	if err != nil {
		return nil, Format{}, youtube.MediaPlan{}, err
	}

	em.stage(StageResolving)
	rctx, rcancel := withTimeout(ctx, c.opts.Timeouts.Resolve)
	defer rcancel()
	plan, err := c.yt.Resolve(rctx, ext, idx)
	if err != nil {
		return nil, Format{}, youtube.MediaPlan{}, err
	}
	return video, video.Formats[idx], plan, nil
}

// webContextCoolingDown reports whether the WEB player-context attempt is
// skipped because the provider recently failed.
func (c *Client) webContextCoolingDown() bool {
	c.webCtxMu.Lock()
	defer c.webCtxMu.Unlock()
	return time.Now().Before(c.webCtxDownUntil)
}

// noteWebContextFailure starts the provider cooldown window.
func (c *Client) noteWebContextFailure() {
	c.webCtxMu.Lock()
	c.webCtxDownUntil = time.Now().Add(webContextCooldown)
	c.webCtxMu.Unlock()
}

// noteWebContextSuccess clears any cooldown.
func (c *Client) noteWebContextSuccess() {
	c.webCtxMu.Lock()
	c.webCtxDownUntil = time.Time{}
	c.webCtxMu.Unlock()
}

// mediaTransfer delivers media from either a direct URL or a SABR stream.
type mediaTransfer interface {
	toFile(ctx context.Context, path string, progress download.ProgressFunc) (download.Result, error)
	toWriter(ctx context.Context, w io.Writer, progress download.ProgressFunc) (download.Result, error)
	stream(ctx context.Context, progress download.ProgressFunc) (io.ReadCloser, download.StreamInfo, error)
}

// urlTransfer delivers a signed URL through the chunked downloader.
type urlTransfer struct {
	dl      *download.Downloader
	src     download.Source
	refresh download.RefreshFunc
}

func (t urlTransfer) toFile(ctx context.Context, path string, progress download.ProgressFunc) (download.Result, error) {
	return t.dl.ToFile(ctx, t.src, path, t.refresh, progress)
}

func (t urlTransfer) toWriter(ctx context.Context, w io.Writer, progress download.ProgressFunc) (download.Result, error) {
	return t.dl.ToWriter(ctx, t.src, w, t.refresh, progress)
}

func (t urlTransfer) stream(ctx context.Context, progress download.ProgressFunc) (io.ReadCloser, download.StreamInfo, error) {
	return t.dl.Stream(ctx, t.src, t.refresh, progress)
}

// sabrTransfer delivers a sequential SABR stream. The SABR layer reports its own
// progress.
type sabrTransfer struct {
	dl     *download.Downloader
	handle *youtube.SABRStream
}

func (t sabrTransfer) toFile(ctx context.Context, path string, progress download.ProgressFunc) (download.Result, error) {
	rc, _, err := t.handle.Open(ctx, sabrProgress(progress))
	if err != nil {
		return download.Result{}, err
	}
	defer rc.Close()
	return t.dl.ReaderToFile(rc, path)
}

func (t sabrTransfer) toWriter(ctx context.Context, w io.Writer, progress download.ProgressFunc) (download.Result, error) {
	rc, _, err := t.handle.Open(ctx, sabrProgress(progress))
	if err != nil {
		return download.Result{}, err
	}
	defer rc.Close()
	n, err := io.Copy(w, rc)
	if err != nil {
		return download.Result{}, err
	}
	return download.Result{BytesWritten: n}, nil
}

func (t sabrTransfer) stream(ctx context.Context, progress download.ProgressFunc) (io.ReadCloser, download.StreamInfo, error) {
	rc, info, err := t.handle.Open(ctx, sabrProgress(progress))
	if err != nil {
		return nil, download.StreamInfo{}, err
	}
	return rc, download.StreamInfo{ContentLength: info.ContentLength, ContentType: info.ContentType}, nil
}

// sabrProgress adapts a download progress callback to SABR's byte counts.
func sabrProgress(p download.ProgressFunc) func(bytesWritten, total int64) {
	if p == nil {
		return nil
	}
	return func(bw, total int64) { p(download.Progress{BytesWritten: bw, Total: total}) }
}

// Download acquires and processes a single YouTube video to the configured sink.
// It is strictly single-video: a playlist URL returns ErrIsPlaylist (use
// Enumerate and loop).
//
// When no processing is requested it downloads straight to the sink with no temp
// file. When a cut, transcode, or loudness stage is requested it stages the source
// to a temp file, runs the fused pipeline, and finalizes to the sink.
func (c *Client) Download(ctx context.Context, req Request) (res *Result, err error) {
	em := newEmitter(req.Events, "")
	defer func() { em.finish(res, err) }()

	id, err := youtube.ExtractVideoID(req.URL)
	if err != nil {
		return nil, err
	}
	em.videoID = id
	// Report HTTP throttling as job warnings.
	ctx = httpx.WithThrottleHook(ctx, func(e httpx.ThrottleEvent) { emitThrottle(em, e) })

	if req.Output.kind == outputNone {
		return nil, fmt.Errorf("waxtap.Download: an Output is required (use Stream for reader delivery)")
	}
	if req.Output.kind == outputFile && req.SkipIfExists && fileExists(req.Output.path) {
		em.stage(StageSkipped)
		return &Result{SourceKind: SourceYouTube, VideoID: id, OutputPath: req.Output.path}, nil
	}

	a, err := c.acquire(ctx, req, id, em)
	if err != nil {
		return nil, err
	}

	if !needsProcessing(req.ProcessSpec) {
		return c.deliverSource(ctx, req, id, a, em)
	}
	return c.downloadAndProcess(ctx, req, a, em)
}

// deliverSource downloads the source straight to the sink without staging,
// preserving the keep-source, no-re-encode default.
func (c *Client) deliverSource(ctx context.Context, req Request, id string, a *acquired, em *emitter) (*Result, error) {
	res := &Result{
		SourceKind:   SourceYouTube,
		VideoID:      id,
		Title:        a.video.Title,
		SourceFormat: a.fmtSel,
		OutputFormat: a.fmtSel,
	}
	progress := func(p download.Progress) { em.progress(p.BytesWritten, p.Total) }

	em.stage(StageDownloading)
	switch req.Output.kind {
	case outputFile:
		r, err := a.transfer.toFile(ctx, req.Output.path, progress)
		if err != nil {
			return nil, err
		}
		res.OutputPath = req.Output.path
		res.SourceBytes, res.OutputBytes = r.BytesWritten, r.BytesWritten
	case outputWriter:
		r, err := a.transfer.toWriter(ctx, req.Output.writer, progress)
		if err != nil {
			return nil, err
		}
		res.SourceBytes, res.OutputBytes = r.BytesWritten, r.BytesWritten
	}
	em.stage(StageFinalizing)
	return res, nil
}

// downloadAndProcess stages the source to a temp file and runs the fused
// pipeline, then finalizes to the sink. For a file sink the pipeline writes the
// destination path directly (atomic), so only a measure-only pass needs a move.
func (c *Client) downloadAndProcess(ctx context.Context, req Request, a *acquired, em *emitter) (*Result, error) {
	jobDir, err := os.MkdirTemp(c.opts.TempDir, "waxtap-job-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(jobDir)

	pipeOut := ""
	if req.Output.kind == outputFile {
		pipeOut = req.Output.path
	}

	deliver, res, err := c.produce(ctx, req, a, jobDir, pipeOut, em)
	if err != nil {
		return nil, err
	}

	em.stage(StageFinalizing)
	switch req.Output.kind {
	case outputFile:
		if deliver != req.Output.path {
			// Measure-only/no-op: the pipeline wrote nothing, so move the staged
			// source into place.
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
	return res, nil
}

// produce downloads the source into jobDir, collects cut ranges, and runs the
// pipeline writing to pipeOut (or a temp inside jobDir when pipeOut is ""). It
// returns the deliverable file path and a Result with metadata and flags filled,
// leaving sink-specific fields to the caller.
func (c *Client) produce(ctx context.Context, req Request, a *acquired, jobDir, pipeOut string, em *emitter) (string, *Result, error) {
	srcExt := sourceExt(a.fmtSel)
	srcPath := filepath.Join(jobDir, "source"+srcExt)

	em.stage(StageDownloading)
	dlRes, err := a.transfer.toFile(ctx, srcPath, func(p download.Progress) {
		em.progress(p.BytesWritten, p.Total)
	})
	if err != nil {
		return "", nil, err
	}

	em.stage(StageStaging)
	ranges, sbRanges, err := c.collectRanges(ctx, req.Cut, a.video.ID, em)
	if err != nil {
		return "", nil, err
	}

	// A SponsorBlock-only request can resolve to no ranges. In that case, deliver
	// the staged source unchanged without requiring ffmpeg. Explicit FormatCopy
	// still goes through ffmpeg because it asks for a remux.
	if len(ranges) == 0 && req.Transcode == nil && req.Loudness == nil {
		res := &Result{
			SourceKind:   SourceYouTube,
			VideoID:      a.video.ID,
			Title:        a.video.Title,
			SourceFormat: a.fmtSel,
			OutputFormat: a.fmtSel,
			SourceBytes:  dlRes.BytesWritten,
		}
		return srcPath, res, nil
	}

	runner, err := c.ffmpeg()
	if err != nil {
		return "", nil, err
	}

	out := pipeOut
	if out == "" {
		out = filepath.Join(jobDir, "output"+outputExt(req.Transcode, srcExt))
	}

	pres, err := pipeline.Run(ctx, runner, srcPath, out, pipelineSpec(req.ProcessSpec, ranges), em.pipelineStage)
	if err != nil {
		return "", nil, err
	}

	deliver := pres.OutputPath
	if deliver == "" {
		deliver = srcPath // measure-only/no-op: deliver the original source
	}

	var explicit []cut.Range
	if req.Cut != nil {
		explicit = cutRanges(req.Cut.Ranges)
	}
	res := newProcessResult(SourceYouTube, pres, a.fmtSel, loudnessTarget(req.Loudness))
	res.VideoID = a.video.ID
	res.Title = a.video.Title
	res.SourceBytes = dlRes.BytesWritten
	res.SponsorBlockApplied = sponsorBlockContributed(explicit, sbRanges, pres)
	return deliver, res, nil
}

// collectRanges merges explicit removal ranges with any SponsorBlock segments,
// honoring the fetch timeout and OnError policy. It returns the combined ranges
// and, separately, the SponsorBlock-derived ranges (so the caller can set
// SponsorBlockApplied). A fetch failure is fatal only under FailDownload;
// otherwise it logs a ProceedUncut warning and continues.
func (c *Client) collectRanges(ctx context.Context, cs *CutSpec, videoID string, em *emitter) (all, sbRanges []cut.Range, err error) {
	if cs == nil {
		return nil, nil, nil
	}
	explicit := cutRanges(cs.Ranges)
	if cs.SponsorBlock == nil {
		return explicit, nil, nil
	}

	sbCtx, cancel := withTimeout(ctx, c.sponsorBlockTimeout(cs))
	defer cancel()
	segs, ferr := c.sb.FetchSegments(sbCtx, videoID, cs.SponsorBlock)
	if ferr != nil {
		if cs.OnError == FailDownload {
			return nil, nil, fmt.Errorf("waxtap: SponsorBlock fetch failed: %w", ferr)
		}
		em.warn(WarnProceedUncut, "SponsorBlock fetch failed; delivering uncut: "+ferr.Error())
		return explicit, nil, nil
	}
	if len(segs) == 0 {
		em.warn(WarnSponsorBlockEmpty, "SponsorBlock returned no segments")
		return explicit, nil, nil
	}

	sbRanges = cut.RangesFromSegments(segs)
	all = append(explicit, sbRanges...)
	return all, sbRanges, nil
}

// sponsorBlockTimeout resolves the SponsorBlock fetch timeout: the per-request
// CutSpec.Timeout takes precedence, then the SponsorBlock option, then the
// per-operation timeout. Zero means no extra deadline.
func (c *Client) sponsorBlockTimeout(cs *CutSpec) (d time.Duration) {
	switch {
	case cs.Timeout > 0:
		return cs.Timeout
	case c.opts.SponsorBlock.Timeout > 0:
		return c.opts.SponsorBlock.Timeout
	default:
		return c.opts.Timeouts.SponsorBlock
	}
}

// Stream acquires a single YouTube video and returns a reader for source-style
// delivery (pipe to disk or object storage). When processing is requested it
// stages and processes to a temp file first, then streams the result. Final byte
// counts are known only after the reader is drained and closed.
func (c *Client) Stream(ctx context.Context, req Request) (rc io.ReadCloser, info StreamInfo, err error) {
	em := newEmitter(req.Events, "")
	defer func() {
		if err != nil {
			em.failed(err)
		}
	}()

	id, err := youtube.ExtractVideoID(req.URL)
	if err != nil {
		return nil, StreamInfo{}, err
	}
	em.videoID = id
	// Report HTTP throttling as job warnings.
	ctx = httpx.WithThrottleHook(ctx, func(e httpx.ThrottleEvent) { emitThrottle(em, e) })

	a, err := c.acquire(ctx, req, id, em)
	if err != nil {
		return nil, StreamInfo{}, err
	}

	if !needsProcessing(req.ProcessSpec) {
		em.stage(StageDownloading)
		body, sinfo, derr := a.transfer.stream(ctx, func(p download.Progress) {
			em.progress(p.BytesWritten, p.Total)
		})
		if derr != nil {
			return nil, StreamInfo{}, derr
		}
		info = StreamInfo{VideoID: id, Title: a.video.Title, Format: a.fmtSel, ContentLength: sinfo.ContentLength}
		return &doneReader{ReadCloser: body, em: em}, info, nil
	}

	return c.streamProcessed(ctx, req, id, a, em)
}

// streamProcessed stages and processes to a temp file, then returns a reader over
// the result that cleans up the temp directory and fires the terminal event on
// Close.
func (c *Client) streamProcessed(ctx context.Context, req Request, id string, a *acquired, em *emitter) (io.ReadCloser, StreamInfo, error) {
	jobDir, err := os.MkdirTemp(c.opts.TempDir, "waxtap-job-*")
	if err != nil {
		return nil, StreamInfo{}, err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(jobDir)
		}
	}()

	deliver, res, err := c.produce(ctx, req, a, jobDir, "", em)
	if err != nil {
		return nil, StreamInfo{}, err
	}

	f, err := os.Open(deliver)
	if err != nil {
		return nil, StreamInfo{}, err
	}
	info := StreamInfo{VideoID: id, Title: a.video.Title, Format: res.OutputFormat, ContentLength: fileSize(deliver)}
	ok = true
	return &dirCleanupReader{File: f, dir: jobDir, em: em}, info, nil
}

// loudnessTarget returns the target LUFS, or 0 when no loudness work is requested.
func loudnessTarget(l *LoudnessSpec) float64 {
	if l == nil {
		return 0
	}
	return l.Target
}

// streamErr records the first non-EOF read error returned after Stream hands a
// reader to the caller. Close uses it to emit the terminal event, since transfer
// failures usually surface in caller-owned Read calls.
type streamErr struct {
	mu  sync.Mutex
	err error
}

func (s *streamErr) record(err error) {
	if err == nil || errors.Is(err, io.EOF) {
		return
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

// terminal emits Done when the stream closed cleanly, or Failed with the first
// read error.
func (s *streamErr) terminal(em *emitter) {
	s.mu.Lock()
	err := s.err
	s.mu.Unlock()
	if err != nil {
		em.failed(err)
		return
	}
	em.done()
}

// doneReader fires the terminal event once when closed, for the zero-disk
// streaming path: Done on a clean read-to-EOF, Failed if a read error occurred.
type doneReader struct {
	io.ReadCloser
	em   *emitter
	errs streamErr
	once sync.Once
}

func (r *doneReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.errs.record(err)
	return n, err
}

func (r *doneReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(func() { r.errs.terminal(r.em) })
	return err
}

// dirCleanupReader streams a processed temp file, removes its job directory, and
// fires the terminal event when closed (Failed if a read error occurred).
type dirCleanupReader struct {
	*os.File
	dir  string
	em   *emitter
	errs streamErr
	once sync.Once
}

func (r *dirCleanupReader) Read(p []byte) (int, error) {
	n, err := r.File.Read(p)
	r.errs.record(err)
	return n, err
}

func (r *dirCleanupReader) Close() error {
	err := r.File.Close()
	r.once.Do(func() {
		os.RemoveAll(r.dir)
		r.errs.terminal(r.em)
	})
	return err
}
