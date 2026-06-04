package waxtap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxtap/cut"
	"github.com/colespringer/waxtap/download"
	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/internal/pipeline"
	"github.com/colespringer/waxtap/sponsorblock"
	"github.com/colespringer/waxtap/transcode"
	"github.com/colespringer/waxtap/waxerr"
	"github.com/colespringer/waxtap/youtube"
)

// --- pure mapping tests (offline) ---

func TestTranscodeCodecMapping(t *testing.T) {
	cases := []struct {
		f    TranscodeFormat
		want transcode.Codec
	}{
		{FormatCopy, transcode.CodecCopy},
		{FormatFLAC, transcode.CodecFLAC},
		{FormatALAC, transcode.CodecALAC},
		{FormatWAV, transcode.CodecWAV},
		{FormatMP3, transcode.CodecMP3},
		{FormatAAC, transcode.CodecAAC},
		{FormatOpus, transcode.CodecOpus},
		{FormatVorbis, transcode.CodecVorbis},
	}
	for _, c := range cases {
		if got := transcodeCodec(c.f); got != c.want {
			t.Errorf("transcodeCodec(%v) = %v, want %v", c.f, got, c.want)
		}
	}
}

func TestTranscodeTargetMapping(t *testing.T) {
	cases := []struct {
		name string
		spec *TranscodeSpec
		want format.Target
	}{
		{"nil", nil, format.Target{}},
		{"copy", &TranscodeSpec{Format: FormatCopy}, format.Target{}},
		{"flac-lossless", &TranscodeSpec{Format: FormatFLAC}, format.Target{Lossless: true}},
		{"wav-lossless", &TranscodeSpec{Format: FormatWAV}, format.Target{Lossless: true}},
		{"aac-family", &TranscodeSpec{Format: FormatAAC}, format.Target{Codec: "aac"}},
		{"opus-family", &TranscodeSpec{Format: FormatOpus}, format.Target{Codec: "opus"}},
		{"vorbis-family", &TranscodeSpec{Format: FormatVorbis}, format.Target{Codec: "vorbis"}},
		{"mp3-no-native", &TranscodeSpec{Format: FormatMP3}, format.Target{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := transcodeTarget(c.spec); got != c.want {
				t.Errorf("transcodeTarget = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestCutModeMapping(t *testing.T) {
	if cutMode(CutSmart).String() != "smart" {
		t.Error("CutSmart should map to smart")
	}
	if cutMode(CutCopy).String() != "copy" {
		t.Error("CutCopy should map to copy")
	}
	if cutMode(CutAccurate).String() != "accurate" {
		t.Error("CutAccurate should map to accurate")
	}
}

func TestCutRangesMapping(t *testing.T) {
	if cutRanges(nil) != nil {
		t.Error("nil ranges should map to nil")
	}
	rs := cutRanges([]TimeRange{{Start: time.Second, End: 2 * time.Second}})
	if len(rs) != 1 || rs[0].Start != time.Second || rs[0].End != 2*time.Second {
		t.Errorf("cutRanges = %+v", rs)
	}
}

func TestNeedsProcessing(t *testing.T) {
	cases := []struct {
		name string
		spec ProcessSpec
		want bool
	}{
		{"empty", ProcessSpec{}, false},
		{"explicit-copy-remux", ProcessSpec{Transcode: &TranscodeSpec{Format: FormatCopy}}, true},
		{"transcode", ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3}}, true},
		{"cut-ranges", ProcessSpec{Cut: &CutSpec{Ranges: []TimeRange{{0, time.Second}}}}, true},
		{"cut-empty-no-sb", ProcessSpec{Cut: &CutSpec{}}, false},
		{"cut-sb-empty-slice", ProcessSpec{Cut: &CutSpec{SponsorBlock: []sponsorblock.Category{}}}, true},
		{"loudness", ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsProcessing(c.spec); got != c.want {
				t.Errorf("needsProcessing = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPipelineSpecRemux(t *testing.T) {
	if ps := pipelineSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatCopy}}, nil); !ps.Remux {
		t.Error("explicit FormatCopy should set pipeline Remux")
	}
	if ps := pipelineSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3}}, nil); ps.Remux {
		t.Error("a re-encode should not set Remux")
	}
	if ps := pipelineSpec(ProcessSpec{}, nil); ps.Remux {
		t.Error("nil Transcode should not set Remux")
	}
}

func TestSourceExtAndOutputExt(t *testing.T) {
	if got := sourceExt(Format{Extension: "m4a"}); got != ".m4a" {
		t.Errorf("sourceExt = %q, want .m4a", got)
	}
	if got := sourceExt(Format{}); got != ".webm" {
		t.Errorf("sourceExt fallback = %q, want .webm", got)
	}
	if got := outputExt(&TranscodeSpec{Format: FormatMP3}, ".webm"); got != ".mp3" {
		t.Errorf("outputExt transcode = %q, want .mp3", got)
	}
	if got := outputExt(&TranscodeSpec{Format: FormatCopy}, ".webm"); got != ".webm" {
		t.Errorf("outputExt copy = %q, want .webm (source)", got)
	}
	if got := outputExt(nil, ".m4a"); got != ".m4a" {
		t.Errorf("outputExt nil = %q, want .m4a (source)", got)
	}
}

func TestToSourceRangeStrategy(t *testing.T) {
	gv := toSource(youtube.ResolvedStream{URL: "https://rr3---sn-abc.googlevideo.com/videoplayback?x=1"})
	if _, ok := gv.RangeStrategy.(download.QueryRange); !ok {
		t.Errorf("googlevideo host should use QueryRange, got %T", gv.RangeStrategy)
	}
	other := toSource(youtube.ResolvedStream{URL: "https://cdn.example.com/a.webm"})
	if other.RangeStrategy != nil {
		t.Errorf("non-googlevideo host should use default (nil) strategy, got %T", other.RangeStrategy)
	}
}

func TestSelectIndex(t *testing.T) {
	formats := []Format{
		{Itag: 140, MIMEType: `audio/mp4; codecs="mp4a.40.2"`, Codec: "mp4a.40.2", AverageBitrate: 128000, IsOriginal: format.Yes},
		{Itag: 251, MIMEType: `audio/webm; codecs="opus"`, Codec: "opus", AverageBitrate: 160000, IsOriginal: format.Yes},
	}
	// Best audio prefers the higher effective bitrate (opus 251).
	idx, err := selectIndex(BestAudio(), MinimizeLoss(), format.Target{}, formats)
	if err != nil {
		t.Fatalf("selectIndex: %v", err)
	}
	if formats[idx].Itag != 251 {
		t.Errorf("best audio itag = %d, want 251", formats[idx].Itag)
	}
	// Itag override.
	idx, err = selectIndex(Itag(140), MinimizeLoss(), format.Target{}, formats)
	if err != nil || formats[idx].Itag != 140 {
		t.Errorf("itag(140) = %d (err %v), want 140", formats[idx].Itag, err)
	}
	// Empty list -> ErrNoAudioFormats.
	if _, err := selectIndex(BestAudio(), MinimizeLoss(), format.Target{}, nil); !errors.Is(err, waxerr.ErrNoAudioFormats) {
		t.Errorf("empty list err = %v, want ErrNoAudioFormats", err)
	}
	// Itag miss -> ErrNoAudioFormats (translated from ErrNoMatch).
	if _, err := selectIndex(Itag(99), MinimizeLoss(), format.Target{}, formats); !errors.Is(err, waxerr.ErrNoAudioFormats) {
		t.Errorf("itag miss err = %v, want ErrNoAudioFormats", err)
	}
}

func TestSponsorBlockContributed(t *testing.T) {
	const total = 60 * time.Second
	r := func(s, e int) cut.Range {
		return cut.Range{Start: time.Duration(s) * time.Second, End: time.Duration(e) * time.Second}
	}
	cut1 := pipeline.Result{Cut: true, SourceDuration: total}

	cases := []struct {
		name     string
		explicit []cut.Range
		sb       []cut.Range
		pres     pipeline.Result
		want     bool
	}{
		{"sb-only-removes", nil, []cut.Range{r(0, 5)}, cut1, true},
		{"sb-adds-to-explicit", []cut.Range{r(0, 5)}, []cut.Range{r(50, 55)}, cut1, true},
		{"sb-covered-by-explicit", []cut.Range{r(0, 10)}, []cut.Range{r(2, 6)}, cut1, false},
		{"sb-clamps-away", nil, []cut.Range{r(100, 200)}, cut1, false},
		{"no-sb", []cut.Range{r(0, 5)}, nil, cut1, false},
		{"no-effective-cut", nil, []cut.Range{r(0, 5)}, pipeline.Result{Cut: false, SourceDuration: total}, false},
		{"unknown-duration", nil, []cut.Range{r(0, 5)}, pipeline.Result{Cut: true, SourceDuration: 0}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sponsorBlockContributed(c.explicit, c.sb, c.pres); got != c.want {
				t.Errorf("sponsorBlockContributed = %v, want %v", got, c.want)
			}
		})
	}
}

// --- emitter tests (offline) ---

func TestEmitterAccumulatesAndDelivers(t *testing.T) {
	var got []Event
	em := newEmitter(func(e Event) { got = append(got, e) }, "vid123")

	em.stage(StageExtracting)
	em.progress(50, 100)
	em.warn(WarnSponsorBlockEmpty, "none")

	res := &Result{}
	em.finish(res, nil)

	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	if got[0].Stage != StageExtracting || got[0].VideoID != "vid123" {
		t.Errorf("event[0] = %+v", got[0])
	}
	if got[1].Stage != StageDownloading || got[1].Bytes != 50 || got[1].Total != 100 {
		t.Errorf("event[1] = %+v", got[1])
	}
	if got[2].Stage != StageWarning || got[2].Warning == nil || got[2].Warning.Code != WarnSponsorBlockEmpty {
		t.Errorf("event[2] = %+v", got[2])
	}
	if got[3].Stage != StageDone {
		t.Errorf("terminal event = %+v, want StageDone", got[3])
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != WarnSponsorBlockEmpty {
		t.Errorf("res.Warnings = %+v", res.Warnings)
	}
}

func TestEmitterFailedTerminal(t *testing.T) {
	var got []Event
	em := newEmitter(func(e Event) { got = append(got, e) }, "")
	sentinel := errors.New("boom")
	em.finish(nil, sentinel)
	if len(got) != 1 || got[0].Stage != StageFailed || !errors.Is(got[0].Err, sentinel) {
		t.Fatalf("failed terminal = %+v", got)
	}
}

func TestEmitterRecoversPanic(t *testing.T) {
	em := newEmitter(func(e Event) { panic("callback blew up") }, "")
	// Must not panic.
	em.stage(StageExtracting)
	em.finish(&Result{}, nil)
}

func TestEmitterNilCallback(t *testing.T) {
	em := newEmitter(nil, "")
	em.stage(StageExtracting)
	em.warn(WarnThrottled, "x")
	res := &Result{}
	em.finish(res, nil)
	if len(res.Warnings) != 1 {
		t.Errorf("warnings still accumulate with a nil callback: %+v", res.Warnings)
	}
}

// errReadCloser yields data then a non-EOF error, to model a mid-stream failure.
type errReadCloser struct {
	data []byte
	err  error
	pos  int
}

func (e *errReadCloser) Read(p []byte) (int, error) {
	if e.pos < len(e.data) {
		n := copy(p, e.data[e.pos:])
		e.pos += n
		return n, nil
	}
	return 0, e.err
}

func (e *errReadCloser) Close() error { return nil }

func TestDoneReaderEmitsDoneOnCleanRead(t *testing.T) {
	var got []Event
	em := newEmitter(func(e Event) { got = append(got, e) }, "v")
	r := &doneReader{ReadCloser: &errReadCloser{data: []byte("abc"), err: io.EOF}, em: em}

	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(got) != 1 || got[0].Stage != StageDone {
		t.Fatalf("clean stream events = %+v, want one StageDone", got)
	}
}

func TestDoneReaderEmitsFailedOnReadError(t *testing.T) {
	var got []Event
	em := newEmitter(func(e Event) { got = append(got, e) }, "v")
	boom := errors.New("network stall")
	r := &doneReader{ReadCloser: &errReadCloser{data: []byte("abc"), err: boom}, em: em}

	// Drain; the reader surfaces the error instead of EOF.
	_, _ = io.ReadAll(r)
	_ = r.Close()

	if len(got) != 1 || got[0].Stage != StageFailed || !errors.Is(got[0].Err, boom) {
		t.Fatalf("failed stream events = %+v, want one StageFailed carrying the error", got)
	}
}

func TestMapPipelineStage(t *testing.T) {
	cases := map[pipeline.Stage]Stage{
		pipeline.StageProbing:     StageProbing,
		pipeline.StageAnalyzing:   StageAnalyzing,
		pipeline.StageCutting:     StageCutting,
		pipeline.StageNormalizing: StageNormalizing,
		pipeline.StageTranscoding: StageTranscoding,
	}
	for in, want := range cases {
		if got := mapPipelineStage(in); got != want {
			t.Errorf("mapPipelineStage(%v) = %v, want %v", in, got, want)
		}
	}
}

// --- facade routing/validation (offline; returns before any network/ffmpeg) ---

func newOfflineClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestDownloadRejectsPlaylistURL(t *testing.T) {
	c := newOfflineClient(t)
	_, err := c.Download(context.Background(), Request{
		URL:         "https://www.youtube.com/playlist?list=PLabcdefghij",
		ProcessSpec: ProcessSpec{Output: ToFile("out.opus")},
	})
	if !errors.Is(err, waxerr.ErrIsPlaylist) {
		t.Errorf("Download(playlist) err = %v, want ErrIsPlaylist", err)
	}
}

func TestDownloadRequiresOutput(t *testing.T) {
	c := newOfflineClient(t)
	_, err := c.Download(context.Background(), Request{URL: "dQw4w9WgXcQ"})
	if err == nil {
		t.Fatal("Download without Output should error")
	}
}

func TestDownloadInvalidURL(t *testing.T) {
	c := newOfflineClient(t)
	_, err := c.Download(context.Background(), Request{URL: "!!!", ProcessSpec: ProcessSpec{Output: ToFile("o")}})
	if err == nil {
		t.Fatal("invalid URL should error")
	}
}

func TestDownloadSkipIfExists(t *testing.T) {
	c := newOfflineClient(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "exists.opus")
	if err := os.WriteFile(out, []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := c.Download(context.Background(), Request{
		URL:         "dQw4w9WgXcQ",
		ProcessSpec: ProcessSpec{Output: ToFile(out), SkipIfExists: true},
	})
	if err != nil {
		t.Fatalf("skip-if-exists Download: %v", err)
	}
	if res.OutputPath != out {
		t.Errorf("skipped OutputPath = %q, want %q", res.OutputPath, out)
	}
}

func TestProcessValidation(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()

	if _, err := c.Process(ctx, ProcessRequest{ProcessSpec: ProcessSpec{Output: ToFile("o")}}); err == nil {
		t.Error("empty Input should error")
	}
	if _, err := c.Process(ctx, ProcessRequest{Input: "in.flac"}); err == nil {
		t.Error("missing Output should error")
	}
	if _, err := c.Process(ctx, ProcessRequest{Input: "same.flac", ProcessSpec: ProcessSpec{Output: ToFile("same.flac")}}); !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("output==input err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestProcessSkipIfExists(t *testing.T) {
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in.flac")
	out := filepath.Join(dir, "out.mp3")
	if err := os.WriteFile(in, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), SkipIfExists: true, Transcode: &TranscodeSpec{Format: FormatMP3}},
	})
	if err != nil {
		t.Fatalf("Process skip: %v", err)
	}
	if res.SourceKind != SourceLocalFile || res.OutputPath != out {
		t.Errorf("skipped result = %+v", res)
	}
}

// --- ffmpeg-gated Process integration ---

func ffmpegOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
}

// synthSine writes a steady stereo sine fixture (never committed).
func synthSine(t *testing.T, dir, name string, seconds int, encoder string) string {
	t.Helper()
	ffmpegOrSkip(t)
	out := filepath.Join(dir, name)
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi",
		"-i", "sine=frequency=440:sample_rate=44100:duration=" + strconv.Itoa(seconds),
		"-af", "volume=-6dB", "-ac", "2", "-c:a", encoder, out,
	}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("synth sine: %v: %s", err, b)
	}
	return out
}

func TestProcessLocalTranscode(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.mp3")

	res, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatMP3}},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.SourceKind != SourceLocalFile || res.InputPath != in {
		t.Errorf("result source = %+v", res)
	}
	if !res.Transcoded || res.OutputFormat.Codec != "mp3" {
		t.Errorf("Transcoded=%v OutputFormat=%+v, want mp3", res.Transcoded, res.OutputFormat)
	}
	if !fileExists(out) || res.OutputBytes <= 0 {
		t.Errorf("output not written: exists=%v bytes=%d", fileExists(out), res.OutputBytes)
	}
}

func TestProcessLocalCutTranscode(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.flac")

	res, err := c.Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: time.Second, End: 2 * time.Second}}},
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.CutApplied {
		t.Error("CutApplied = false, want true")
	}
	runner, _ := c.ffmpeg()
	probe, err := runner.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	// 4s minus a 1s cut => ~3s.
	if d := probe.Format.Duration; d < 2500*time.Millisecond || d > 3500*time.Millisecond {
		t.Errorf("output duration = %v, want ~3s", d)
	}
}

func TestProcessLocalCopyRemux(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "out.mka")

	res, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatCopy}},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Transcoded {
		t.Error("a copy remux is not a re-encode")
	}
	if !fileExists(out) || res.OutputBytes <= 0 {
		t.Fatalf("remux wrote no output: exists=%v bytes=%d", fileExists(out), res.OutputBytes)
	}
	// The audio was stream-copied (codec unchanged), but the container changed to
	// the requested matroska, proving a real remux rather than a raw byte copy.
	runner, _ := c.ffmpeg()
	probe, err := runner.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("probe remux output: %v", err)
	}
	if a, _ := probe.AudioStream(); a.CodecName != "flac" {
		t.Errorf("remux changed codec to %q, want flac (stream copy)", a.CodecName)
	}
	if !strings.Contains(probe.Format.FormatName, "matroska") {
		t.Errorf("remux container = %q, want matroska", probe.Format.FormatName)
	}
}

func TestProcessLocalMeasureOnly(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	out := filepath.Join(dir, "copy.flac")

	res, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly}},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.LoudnessMeasured || res.Loudness == nil || res.Loudness.Input == nil {
		t.Fatalf("measure-only result = %+v", res)
	}
	if res.Transcoded || res.LoudnessApplied {
		t.Errorf("measure-only must not transcode or apply: %+v", res)
	}
	// The unchanged input is copied to the destination.
	if !fileExists(out) {
		t.Error("measure-only should copy the source to the output path")
	}
	if !fileExists(in) {
		t.Error("measure-only must not remove the input")
	}
}

func TestProcessLocalToWriter(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 1, "flac")

	var buf bytes.Buffer
	res, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToWriter(&buf), Transcode: &TranscodeSpec{Format: FormatMP3}},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if buf.Len() == 0 || res.OutputBytes != int64(buf.Len()) {
		t.Errorf("writer got %d bytes, OutputBytes=%d", buf.Len(), res.OutputBytes)
	}
}

func TestProcessLocalLoudnessApply(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 3, "flac")
	out := filepath.Join(dir, "norm.flac")

	res, err := c.Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
			Loudness:  &LoudnessSpec{Mode: LoudnessApply, Target: -14},
		},
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.LoudnessApplied || res.Loudness == nil || res.Loudness.Output == nil {
		t.Fatalf("apply result = %+v", res)
	}
	if res.Loudness.Target != -14 {
		t.Errorf("target = %v, want -14", res.Loudness.Target)
	}
	// The normalized output should land within a couple LU of the target.
	if got := res.Loudness.Output.IntegratedLUFS; got < -16 || got > -12 {
		t.Errorf("output loudness = %v LUFS, want within 2 LU of -14", got)
	}
}

func TestProcessLoudnessApplyWithoutTranscodeRejected(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 1, "flac")
	out := filepath.Join(dir, "out.flac")

	_, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -14}},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Errorf("apply without transcode err = %v, want ErrIncompatibleSpec", err)
	}
}

func TestMeasureAlbum(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	a := synthSine(t, dir, "a.flac", 2, "flac")
	b := synthSine(t, dir, "b.flac", 2, "flac")

	res, err := c.MeasureAlbum(context.Background(), []string{a, b})
	if err != nil {
		t.Fatalf("MeasureAlbum: %v", err)
	}
	if len(res.PerTrack) != 2 {
		t.Fatalf("per-track count = %d, want 2", len(res.PerTrack))
	}
	// Two identical tracks: the album loudness should match each track closely.
	for i, tr := range res.PerTrack {
		if d := tr.IntegratedLUFS - res.Album.IntegratedLUFS; d < -1 || d > 1 {
			t.Errorf("track %d (%v) vs album (%v) differ by >1 LU", i, tr.IntegratedLUFS, res.Album.IntegratedLUFS)
		}
	}
}
