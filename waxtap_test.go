package waxtap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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
	// An explicit itag miss names the available itags.
	_, err = selectIndex(Itag(99), MinimizeLoss(), format.Target{}, formats)
	if !errors.Is(err, waxerr.ErrRequestedFormatUnavailable) {
		t.Errorf("itag miss err = %v, want ErrRequestedFormatUnavailable", err)
	}
	if errors.Is(err, waxerr.ErrNoAudioFormats) {
		t.Errorf("itag miss err = %v, must not be ErrNoAudioFormats (formats exist)", err)
	}
	if rfe, ok := errors.AsType[*waxerr.RequestedFormatError](err); !ok {
		t.Errorf("itag miss err = %v, want *RequestedFormatError", err)
	} else if len(rfe.Itags) != 2 || len(rfe.Codecs) != 0 {
		t.Errorf("RequestedFormatError = %+v, want the two available itags named (no codecs for an itag miss)", rfe)
	}
	// An explicit codec miss names the available codecs, not itags.
	_, err = selectIndex(Codec("flac"), MinimizeLoss(), format.Target{}, formats)
	rfe, ok := errors.AsType[*waxerr.RequestedFormatError](err)
	if !ok {
		t.Fatalf("codec miss err = %v, want *RequestedFormatError", err)
	}
	if len(rfe.Codecs) == 0 || len(rfe.Itags) != 0 {
		t.Errorf("RequestedFormatError = %+v, want available codecs named (no itags for a codec miss)", rfe)
	}
	if msg := rfe.Error(); !strings.Contains(msg, "available codecs") {
		t.Errorf("codec miss message = %q, want it to list available codecs", msg)
	}
	// A best-audio miss on a non-empty but ineligible list stays ErrNoAudioFormats.
	videoOnly := []Format{{Itag: 137, MIMEType: `video/mp4; codecs="avc1.640028"`, Codec: "avc1.640028"}}
	if _, err := selectIndex(BestAudio(), MinimizeLoss(), format.Target{}, videoOnly); !errors.Is(err, waxerr.ErrNoAudioFormats) {
		t.Errorf("best-audio miss err = %v, want ErrNoAudioFormats", err)
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

func newOfflineClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestVideoMetadataForMapsChannelAndChapters checks the metadata mapping: with
// IncludeMetadata the result carries ChannelID and the (FullMetadata-populated)
// Chapters; without IncludeMetadata it is nil.
func TestVideoMetadataForMapsChannelAndChapters(t *testing.T) {
	v := &youtube.Video{
		Author:    "A",
		ChannelID: "UCabcdefghijklmnopqrstuv",
		Chapters:  []youtube.Chapter{{Title: "Intro", Start: 0, End: 30 * time.Second}},
	}
	if got := videoMetadataFor(Request{}, v); got != nil {
		t.Errorf("videoMetadataFor without IncludeMetadata = %+v, want nil", got)
	}
	req := Request{ProcessSpec: ProcessSpec{IncludeMetadata: true}}
	md := videoMetadataFor(req, v)
	if md == nil {
		t.Fatal("videoMetadataFor with IncludeMetadata = nil")
	}
	if md.ChannelID != "UCabcdefghijklmnopqrstuv" {
		t.Errorf("ChannelID = %q", md.ChannelID)
	}
	if len(md.Chapters) != 1 || md.Chapters[0].Title != "Intro" {
		t.Errorf("Chapters = %+v, want one Intro chapter", md.Chapters)
	}
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
	_, err := c.Download(context.Background(), Request{URL: "testVideo01"})
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
		URL:         "testVideo01",
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

func TestEnumerateRejectsNegativeMaxItems(t *testing.T) {
	c := newOfflineClient(t)
	// The guard runs before any network work, so a negative cap fails fast and is
	// classified as invalid config (exit 2 for the CLI), not a generic error.
	_, err := c.Enumerate(context.Background(), "https://www.youtube.com/playlist?list=PLxxxxxxxxxxxx", EnumerateOptions{MaxItems: -1})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("Enumerate with MaxItems < 0 = %v, want ErrInvalidConfig", err)
	}
}

func TestWarnEmptyCut(t *testing.T) {
	const dur = 200 * time.Second

	warned := func(cs *CutSpec, pres pipeline.Result, sbHadSegments bool) bool {
		var got bool
		em := newEmitter(func(e Event) {
			if e.Stage == StageWarning && e.Warning != nil && e.Warning.Code == WarnRangesEmpty {
				got = true
			}
		}, "")
		warnEmptyCut(em, cs, pres, sbHadSegments)
		return got
	}

	sbOnly := &CutSpec{SponsorBlock: []sponsorblock.Category{}}
	// SponsorBlock returned segments but they all fell outside the media: warn.
	if !warned(sbOnly, pipeline.Result{SourceDuration: dur}, true) {
		t.Error("SponsorBlock segments outside the media should emit WarnRangesEmpty")
	}
	// SponsorBlock returned no segments: WarnSponsorBlockEmpty already covered it,
	// so do not emit a duplicate WarnRangesEmpty.
	if warned(sbOnly, pipeline.Result{SourceDuration: dur}, false) {
		t.Error("no SponsorBlock segments must not also emit WarnRangesEmpty")
	}
	// Do not warn after an effective cut, for explicit ranges, without a cut, with
	// unknown duration, or for an empty CutSpec.
	if warned(sbOnly, pipeline.Result{SourceDuration: dur, Cut: true}, true) {
		t.Error("an effective cut must not warn")
	}
	if warned(&CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}}, pipeline.Result{SourceDuration: dur}, false) {
		t.Error("explicit ranges are handled in the pipeline, not warned here")
	}
	if warned(&CutSpec{}, pipeline.Result{SourceDuration: dur}, false) {
		t.Error("an empty CutSpec (no ranges, no SponsorBlock) is not a cut and must not warn")
	}
	if warned(nil, pipeline.Result{SourceDuration: dur}, false) {
		t.Error("nil cut must not warn")
	}
	if warned(sbOnly, pipeline.Result{}, true) {
		t.Error("unknown duration must not warn")
	}
}

func TestValidateProcessSpec_Downmix(t *testing.T) {
	// Downmix requires a fixed mono or stereo target.
	for _, layout := range []ChannelLayout{LayoutSurround, LayoutAny} {
		if err := validateProcessSpec(ProcessSpec{Downmix: true, Channels: layout}); !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("Downmix+%s = %v, want ErrIncompatibleSpec", layout, err)
		}
	}
	for _, layout := range []ChannelLayout{LayoutMono, LayoutStereo} {
		if err := validateProcessSpec(ProcessSpec{Downmix: true, Channels: layout}); err != nil {
			t.Errorf("Downmix+%s = %v, want nil", layout, err)
		}
	}
	// Without Downmix the layout is only a selection hint, never rejected.
	if err := validateProcessSpec(ProcessSpec{Channels: LayoutSurround}); err != nil {
		t.Errorf("no downmix = %v, want nil", err)
	}
}

func TestValidateProcessSpec_LoudnessAndBitrate(t *testing.T) {
	apply := func(target float64) ProcessSpec {
		return ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: target}}
	}
	for _, target := range []float64{-4, -71, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := validateProcessSpec(apply(target)); !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("apply target %v = %v, want ErrIncompatibleSpec", target, err)
		}
	}
	for _, target := range []float64{-5, -70, -14} {
		if err := validateProcessSpec(apply(target)); err != nil {
			t.Errorf("apply target %v = %v, want nil", target, err)
		}
	}
	// Measure-only mode does not use the target.
	if err := validateProcessSpec(ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly, Target: 999}}); err != nil {
		t.Errorf("measure-only target = %v, want nil", err)
	}
	if err := validateProcessSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3, Bitrate: -1}}); !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("negative bitrate = %v, want ErrIncompatibleSpec", err)
	}
	if err := validateProcessSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3, Bitrate: 0}}); err != nil {
		t.Errorf("zero bitrate = %v, want nil", err)
	}
	if err := validateProcessSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3, Bitrate: maxBitrate + 1}}); !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("excessive lossy bitrate = %v, want ErrIncompatibleSpec", err)
	}
	if err := validateProcessSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3, Bitrate: 320000}}); err != nil {
		t.Errorf("realistic 320 kbps bitrate = %v, want nil", err)
	}
	// ffmpeg ignores bitrate for lossless targets, so the upper bound does not apply.
	if err := validateProcessSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatFLAC, Bitrate: maxBitrate + 1}}); err != nil {
		t.Errorf("high bitrate on a lossless target = %v, want nil (ignored, not an error)", err)
	}
}

func TestValidateProcessSpec_NegativeCrossfade(t *testing.T) {
	if err := validateProcessSpec(ProcessSpec{Cut: &CutSpec{Crossfade: -1}}); !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("negative crossfade = %v, want ErrIncompatibleSpec (parity with the CLI)", err)
	}
	if err := validateProcessSpec(ProcessSpec{Cut: &CutSpec{Crossfade: 500 * time.Millisecond}}); err != nil {
		t.Errorf("non-negative crossfade = %v, want nil", err)
	}
}

func TestValidateProcessSpec_CheckOutputContainer(t *testing.T) {
	spec := func(f TranscodeFormat, out string) ProcessSpec {
		return ProcessSpec{Transcode: &TranscodeSpec{Format: f}, Output: ToFile(out)}
	}
	reject := []struct {
		name string
		f    TranscodeFormat
		out  string
	}{
		{"mp3 in flac", FormatMP3, "out.flac"},
		{"mp3 in wav", FormatMP3, "out.wav"},
		{"flac in opus", FormatFLAC, "out.opus"},
		{"opus in m4a", FormatOpus, "out.m4a"},
	}
	for _, c := range reject {
		if err := validateProcessSpec(spec(c.f, c.out)); !errors.Is(err, ErrIncompatibleSpec) {
			t.Errorf("%s: err = %v, want ErrIncompatibleSpec", c.name, err)
		}
	}

	pass := []struct {
		name string
		f    TranscodeFormat
		out  string
	}{
		{"mp3 in mp3", FormatMP3, "out.mp3"},
		{"flac in flac", FormatFLAC, "out.flac"},
		{"aac in m4a", FormatAAC, "out.m4a"},
		{"aac in mp4", FormatAAC, "out.mp4"},
		{"opus in webm", FormatOpus, "out.webm"},
		{"opus in mka", FormatOpus, "out.mka"},
		// WAV dual-name: canonical "wav" must satisfy the PCM-accepting branches.
		{"wav in mka", FormatWAV, "out.mka"},
		// Extension outside the table passes unchecked (ffmpeg validates).
		{"wav in w64", FormatWAV, "out.w64"},
		{"flac in aiff", FormatFLAC, "out.aiff"},
		// Force-muxed: codec-named and extensionless outputs are unconstrained.
		{"alac in .alac", FormatALAC, "out.alac"},
		{"flac extensionless", FormatFLAC, "out"},
		// Copy follows the source container, so it is never rejected here.
		{"copy in flac", FormatCopy, "out.flac"},
	}
	for _, c := range pass {
		if err := validateProcessSpec(spec(c.f, c.out)); err != nil {
			t.Errorf("%s: err = %v, want nil", c.name, err)
		}
	}

	// A writer sink is not container-checked (it stages with a derived extension).
	if err := validateProcessSpec(ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3}, Output: ToWriter(io.Discard)}); err != nil {
		t.Errorf("writer sink: err = %v, want nil (no path to check)", err)
	}
}

func TestValidateProcessSpec_CutWithoutExtension(t *testing.T) {
	// Extensionless copy-cut to a file output needs a container or --format.
	extensionless := ProcessSpec{
		Cut:    &CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}},
		Output: ToFile("clip"),
	}
	if err := validateProcessSpec(extensionless); !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("extensionless copy-cut = %v, want ErrIncompatibleSpec", err)
	}
	// The same copy-cut with a container extension is valid.
	withExt := extensionless
	withExt.Output = ToFile("clip.webm")
	if err := validateProcessSpec(withExt); err != nil {
		t.Errorf("copy-cut with .webm = %v, want nil", err)
	}
	// The same copy-cut with a re-encode target is valid even extensionless.
	withFormat := extensionless
	withFormat.Transcode = &TranscodeSpec{Format: FormatFLAC}
	if err := validateProcessSpec(withFormat); err != nil {
		t.Errorf("copy-cut with --format flac = %v, want nil", err)
	}

	// Accurate cut and crossfade always re-encode, so they need --format
	// regardless of the output extension.
	accurate := ProcessSpec{
		Cut:    &CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}, Mode: CutAccurate},
		Output: ToFile("clip.flac"),
	}
	if err := validateProcessSpec(accurate); !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("accurate cut without --format = %v, want ErrIncompatibleSpec", err)
	}
	crossfade := ProcessSpec{
		Cut:    &CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}, Crossfade: time.Second},
		Output: ToFile("clip.flac"),
	}
	if err := validateProcessSpec(crossfade); !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("crossfade without --format = %v, want ErrIncompatibleSpec", err)
	}
	// Both are fine once a re-encode target is supplied.
	accurate.Transcode = &TranscodeSpec{Format: FormatFLAC}
	if err := validateProcessSpec(accurate); err != nil {
		t.Errorf("accurate cut with --format = %v, want nil", err)
	}

	// A copy-cut to a writer sink is valid: the pipeline stages with the source
	// extension, so the empty path must not be read as "no extension".
	toWriter := ProcessSpec{
		Cut:    &CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}},
		Output: ToWriter(io.Discard),
	}
	if err := validateProcessSpec(toWriter); err != nil {
		t.Errorf("copy-cut to writer sink = %v, want nil", err)
	}
}

func TestValidateProcessSpec_CutWithDownmixDeferred(t *testing.T) {
	// Downmix defers these checks until probing. If the source needs folding, the
	// pipeline encodes after probing and these cuts are valid without --format. If
	// no fold is needed, the pipeline still applies its copy-mode checks before
	// writing. None of these should fail during ProcessSpec validation.
	cut := func(c *CutSpec, out Output) ProcessSpec {
		return ProcessSpec{Cut: c, Output: out, Downmix: true, Channels: LayoutMono}
	}
	ranges := []TimeRange{{Start: 0, End: time.Second}}
	deferred := []ProcessSpec{
		cut(&CutSpec{Ranges: ranges, Mode: CutAccurate}, ToFile("clip.flac")),
		cut(&CutSpec{Ranges: ranges, Crossfade: time.Second}, ToFile("clip.flac")),
		cut(&CutSpec{Ranges: ranges}, ToFile("clip")), // extensionless
	}
	for i, spec := range deferred {
		if err := validateProcessSpec(spec); err != nil {
			t.Errorf("deferred[%d] with downmix = %v, want nil (deferred to the pipeline)", i, err)
		}
	}
}

func TestNew_RejectsInvalidQPS(t *testing.T) {
	for _, q := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := New(Options{Politeness: Politeness{PerHostQPS: q}}); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("PerHostQPS %v: New err = %v, want ErrInvalidConfig", q, err)
		}
	}
	if _, err := New(Options{Politeness: Politeness{PerHostQPS: 2}}); err != nil {
		t.Errorf("PerHostQPS 2: New err = %v, want nil", err)
	}
}

func TestNew_RejectsUnknownClient(t *testing.T) {
	_, err := New(Options{Client: "bogus"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(Client:bogus) err = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "want one of") {
		t.Errorf("err = %q, want it to keep the 'want one of' client list", err)
	}
}

func TestProcessRejectedCutPreservesExistingOutput(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.mp3")
	if err := os.WriteFile(out, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The transcode would normally clobber out.mp3, but the out-of-range cut is
	// rejected before any write, so the pre-existing file must survive intact.
	_, err := c.Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Cut:       &CutSpec{Ranges: []TimeRange{{Start: 999 * time.Second, End: 1000 * time.Second}}},
			Transcode: &TranscodeSpec{Format: FormatMP3},
		},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("err = %v, want ErrIncompatibleSpec", err)
	}
	data, rerr := os.ReadFile(out)
	if rerr != nil || string(data) != "ORIGINAL" {
		t.Errorf("pre-existing output was destroyed: data=%q err=%v", data, rerr)
	}
}

func TestProcessExplicitCutOutOfRangeRejected(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.flac")

	// A cut whose only range lies entirely past the media must fail, not silently
	// return the whole file.
	_, err := c.Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output: ToFile(out),
			Cut:    &CutSpec{Ranges: []TimeRange{{Start: 999 * time.Second, End: 1000 * time.Second}}},
		},
	})
	if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
		t.Fatalf("out-of-range cut err = %v, want ErrIncompatibleSpec", err)
	}
	if fileExists(out) {
		t.Error("a rejected cut must not leave an output file")
	}
}

func TestProcessRemuxExtensionlessInfersContainer(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 1, "flac")

	// Extensionless and .copy destinations infer a container from the source codec.
	for _, out := range []string{filepath.Join(dir, "out"), filepath.Join(dir, "out.copy")} {
		res, err := c.Process(context.Background(), ProcessRequest{
			Input: in,
			ProcessSpec: ProcessSpec{
				Output:    ToFile(out),
				Transcode: &TranscodeSpec{Format: FormatCopy},
			},
		})
		if err != nil {
			t.Fatalf("remux to %q = %v, want success (inferred container)", out, err)
		}
		if res.Transcoded {
			t.Errorf("remux to %q reported a re-encode; want a stream copy", out)
		}
		if !fileExists(out) || res.OutputBytes <= 0 {
			t.Fatalf("remux to %q wrote no output", out)
		}
		// The audio is stream-copied, so the codec is unchanged from the source.
		runner, _ := c.ffmpeg()
		probe, perr := runner.Probe(context.Background(), out)
		if perr != nil {
			t.Fatalf("probe %q: %v", out, perr)
		}
		if a, _ := probe.AudioStream(); a.CodecName != "flac" {
			t.Errorf("remux to %q changed codec to %q, want flac (stream copy)", out, a.CodecName)
		}
	}
}

func TestProcessCopyCutWithoutContainerRejected(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")

	// The removal creates two copied segments and exercises the multi-range path.
	for _, out := range []string{filepath.Join(dir, "mytrack"), filepath.Join(dir, "mytrack.copy")} {
		_, err := c.Process(context.Background(), ProcessRequest{
			Input: in,
			ProcessSpec: ProcessSpec{
				Output: ToFile(out),
				Cut:    &CutSpec{Ranges: []TimeRange{{Start: 800 * time.Millisecond, End: 1200 * time.Millisecond}}},
			},
		})
		if !errors.Is(err, waxerr.ErrIncompatibleSpec) {
			t.Errorf("copy cut to %q = %v, want ErrIncompatibleSpec", out, err)
		}
		if fileExists(out) {
			t.Errorf("rejected copy cut to %q wrote output", out)
		}
	}
}

func TestProcessTranscodeExtensionlessOutput(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 1, "flac")
	out := filepath.Join(dir, "track") // no extension

	// The preset supplies the muxer when the output path has no extension.
	res, err := c.Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output:    ToFile(out),
			Transcode: &TranscodeSpec{Format: FormatFLAC},
		},
	})
	if err != nil {
		t.Fatalf("extensionless transcode err = %v", err)
	}
	if !fileExists(out) || fileSize(out) == 0 {
		t.Fatalf("no output written to extensionless path %q", out)
	}
	if res.OutputFormat.Codec != "flac" {
		t.Errorf("output codec = %q, want flac", res.OutputFormat.Codec)
	}
}

func TestProcessPartialOverlapCutSucceeds(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 4, "flac")
	out := filepath.Join(dir, "out.flac")

	// A range that overruns the end still intersects the media, so clamping keeps
	// it valid and the cut succeeds.
	res, err := c.Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output: ToFile(out),
			Cut:    &CutSpec{Ranges: []TimeRange{{Start: 3 * time.Second, End: 999 * time.Second}}},
		},
	})
	if err != nil {
		t.Fatalf("partial-overlap cut: %v", err)
	}
	if !res.CutApplied || !fileExists(out) {
		t.Errorf("partial-overlap cut result = %+v, want CutApplied and an output file", res)
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

// TestProcessMeasureOnlyNoOutput verifies that pure measurement works without an
// Output: it reports loudness, writes no file, leaves the input in place, and
// still reaches StageFinalizing.
func TestProcessMeasureOnlyNoOutput(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")

	var stages []Stage
	res, err := c.Process(context.Background(), ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly},
			Events:   func(e Event) { stages = append(stages, e.Stage) },
		},
	})
	if err != nil {
		t.Fatalf("Process measure-only (no Output): %v", err)
	}
	if !res.LoudnessMeasured || res.Loudness == nil || res.Loudness.Input == nil {
		t.Fatalf("measure-only result = %+v", res)
	}
	if res.OutputPath != "" || res.OutputBytes != 0 {
		t.Errorf("measure-only with no Output should not write a file: path=%q bytes=%d", res.OutputPath, res.OutputBytes)
	}
	if res.Transcoded || res.LoudnessApplied {
		t.Errorf("measure-only must not transcode or apply: %+v", res)
	}
	// The lifecycle matches the with-Output measure: StageFinalizing fires.
	if !containsStage(stages, StageFinalizing) {
		t.Errorf("stages = %v, want StageFinalizing", stages)
	}
	if containsStage(stages, StageSkipped) {
		t.Errorf("stages = %v, measure-only must not be StageSkipped", stages)
	}
	// No stray output files were created alongside the input.
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir has %d entries, want only the input file", len(entries))
	}
}

func containsStage(stages []Stage, want Stage) bool {
	for _, s := range stages {
		if s == want {
			return true
		}
	}
	return false
}

// TestMeasure exercises the Client.Measure convenience wrapper.
func TestMeasure(t *testing.T) {
	ffmpegOrSkip(t)
	c := newOfflineClient(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")

	got, err := c.Measure(context.Background(), in)
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if math.IsNaN(got.IntegratedLUFS) || math.IsInf(got.IntegratedLUFS, 0) {
		t.Errorf("Measure returned non-finite loudness for a steady sine: %+v", got)
	}
	if got.IntegratedLUFS >= 0 {
		t.Errorf("Measure integrated LUFS = %v, want a negative value for audio with signal", got.IntegratedLUFS)
	}
	// Measure neither writes nor removes anything.
	if !fileExists(in) {
		t.Error("Measure must not remove the input")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir has %d entries, want only the input file", len(entries))
	}
}

// TestLoudnessInfoMarshalJSON verifies that non-finite measurements, such as
// digital silence yielding -Inf, marshal as null instead of failing json.Marshal.
func TestLoudnessInfoMarshalJSON(t *testing.T) {
	b, err := json.Marshal(LoudnessInfo{IntegratedLUFS: math.Inf(-1), TruePeakDBTP: -1.5, LRA: math.NaN(), Threshold: -24})
	if err != nil {
		t.Fatalf("marshal non-finite LoudnessInfo: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["IntegratedLUFS"] != nil {
		t.Errorf("IntegratedLUFS = %v, want null for -Inf", m["IntegratedLUFS"])
	}
	if m["LRA"] != nil {
		t.Errorf("LRA = %v, want null for NaN", m["LRA"])
	}
	if m["TruePeakDBTP"] != -1.5 {
		t.Errorf("TruePeakDBTP = %v, want -1.5 (finite preserved)", m["TruePeakDBTP"])
	}
	if m["Threshold"] != -24.0 {
		t.Errorf("Threshold = %v, want -24 (finite preserved)", m["Threshold"])
	}
}

// TestIsMeasureOnlySpec checks which specs may run without an Output. Only pure
// loudness measurement qualifies; anything that writes audio does not.
func TestIsMeasureOnlySpec(t *testing.T) {
	measure := func() *LoudnessSpec { return &LoudnessSpec{Mode: LoudnessMeasureOnly} }
	cases := []struct {
		name string
		spec ProcessSpec
		want bool
	}{
		{"pure measure-only", ProcessSpec{Loudness: measure()}, true},
		{"no loudness", ProcessSpec{}, false},
		{"apply not measure", ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -14}}, false},
		{"copy remux", ProcessSpec{Loudness: measure(), Transcode: &TranscodeSpec{Format: FormatCopy}}, false},
		{"transcode", ProcessSpec{Loudness: measure(), Transcode: &TranscodeSpec{Format: FormatMP3}}, false},
		{"downmix", ProcessSpec{Loudness: measure(), Downmix: true, Channels: LayoutStereo}, false},
		{"explicit cut ranges", ProcessSpec{Loudness: measure(), Cut: &CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}}}, false},
		{"sponsorblock", ProcessSpec{Loudness: measure(), Cut: &CutSpec{SponsorBlock: []Category{CategoryMusicOffTopic}}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMeasureOnlySpec(tc.spec); got != tc.want {
				t.Errorf("isMeasureOnlySpec(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestProcessNoOutputRequiresOutput verifies that audio-writing specs still need
// an Output.
func TestProcessNoOutputRequiresOutput(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	measure := func() *LoudnessSpec { return &LoudnessSpec{Mode: LoudnessMeasureOnly} }
	cases := []struct {
		name string
		spec ProcessSpec
	}{
		{"copy remux", ProcessSpec{Loudness: measure(), Transcode: &TranscodeSpec{Format: FormatCopy}}},
		{"transcode", ProcessSpec{Loudness: measure(), Transcode: &TranscodeSpec{Format: FormatMP3}}},
		{"downmix", ProcessSpec{Loudness: measure(), Downmix: true, Channels: LayoutStereo}},
		{"explicit cut", ProcessSpec{Loudness: measure(), Cut: &CutSpec{Ranges: []TimeRange{{Start: 0, End: time.Second}}}}},
		{"no loudness", ProcessSpec{Transcode: &TranscodeSpec{Format: FormatMP3}}},
		{"apply", ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessApply, Target: -14}, Transcode: &TranscodeSpec{Format: FormatMP3}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Process(ctx, ProcessRequest{Input: "in.flac", ProcessSpec: tc.spec})
			if err == nil || !strings.Contains(err.Error(), "an Output is required") {
				t.Errorf("Process(%s) err = %v, want 'an Output is required'", tc.name, err)
			}
		})
	}
}

// TestMeasureNoScratchDir verifies that pure measurement does not touch TempDir.
func TestMeasureNoScratchDir(t *testing.T) {
	ffmpegOrSkip(t)
	dir := t.TempDir()
	in := synthSine(t, dir, "in.flac", 2, "flac")
	tempDir := filepath.Join(dir, "scratch-never-created")
	c, err := New(Options{TempDir: tempDir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := c.Measure(context.Background(), in); err != nil {
		t.Fatalf("Measure: %v", err)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Errorf("Measure created the scratch dir %q (stat err = %v)", tempDir, err)
	}

	if _, err := c.Process(context.Background(), ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Loudness: &LoudnessSpec{Mode: LoudnessMeasureOnly}},
	}); err != nil {
		t.Fatalf("Process measure-only: %v", err)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Errorf("no-Output Process created the scratch dir %q (stat err = %v)", tempDir, err)
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
