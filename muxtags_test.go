package waxtap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/youtube"
)

// taggedWVFixture synthesizes a WavPack file with mux-time APEv2 tags (the
// source shape WaxLabel cannot parse), including one own-audio value.
func taggedWVFixture(t *testing.T, c *Client, dir string) string {
	t.Helper()
	wav := filepath.Join(t.TempDir(), "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.wv")
	spec := media.Spec{Codec: media.CodecWavPack, Tags: []media.Tag{
		{Key: "TITLE", Value: "From APEv2"},
		{Key: "REPLAYGAIN_TRACK_GAIN", Value: "-1.00 dB"},
	}}
	if _, err := c.engine().Transcode(context.Background(), wav, in, spec); err != nil {
		t.Fatalf("synth wv: %v", err)
	}
	return in
}

// A WaxLabel-readable source's text tags reach a WavPack output through the
// muxer, own-audio values are dropped (the encode re-derived the samples), and
// the picture and chapters that have no APEv2 form here are reported as carry
// losses.
func TestProcessFLACToWavPackCarriesTags(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedFLAC(t, dir)
	out := filepath.Join(dir, "out.wv")

	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatWavPack}},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}

	pr, err := c.engine().Probe(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	get := func(k string) string {
		if vs := pr.Tags[k]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	if got := get("TITLE"); got != "Carried Title" {
		t.Errorf("TITLE = %q, want carried (tags = %v)", got, pr.Tags)
	}
	if got := get("ARTIST"); got != "Carried Artist" {
		t.Errorf("ARTIST = %q, want carried", got)
	}
	if got := get("REPLAYGAIN_TRACK_GAIN"); got != "" {
		t.Errorf("ReplayGain = %q, want dropped on a re-encode", got)
	}

	var lossWarn string
	for _, w := range res.Warnings {
		if w.Code == WarnTagCarry {
			lossWarn = w.Detail
		}
	}
	if !strings.Contains(lossWarn, "picture") || !strings.Contains(lossWarn, "chapters") || !strings.Contains(lossWarn, "wavpack") {
		t.Errorf("warnings = %v, want a WarnTagCarry naming the picture and chapters a wavpack file cannot hold", res.Warnings)
	}
}

// A WavPack source's demuxer-read tags reach a WaxLabel-writable output (the
// lossless-library-to-FLAC migration shape): the probe fallback carries them
// when WaxLabel cannot parse the source itself.
func TestProcessWavPackToFLACCarriesProbedTags(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedWVFixture(t, c, dir)

	out := filepath.Join(dir, "out.flac")
	if _, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	}); err != nil {
		t.Fatalf("process: %v", err)
	}

	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if vs, _ := doc.Get(tag.Title); len(vs) == 0 || vs[0] != "From APEv2" {
		t.Errorf("TITLE = %v, want the probed source tag carried", vs)
	}
	if vs, _ := doc.Get(tag.ReplayGainTrackGain); len(vs) != 0 {
		t.Errorf("ReplayGain = %v, want dropped on a re-encode", vs)
	}
}

// A whole-file remux of a WavPack source keeps every tag through the facade,
// own-audio values included: the audio bytes are unchanged.
func TestProcessWavPackCopyKeepsAllTags(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedWVFixture(t, c, dir)

	out := filepath.Join(dir, "copy.wv")
	if _, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatCopy}},
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	pr, err := c.engine().Probe(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if vs := pr.Tags["TITLE"]; len(vs) == 0 || vs[0] != "From APEv2" {
		t.Errorf("TITLE = %v, want carried", vs)
	}
	if vs := pr.Tags["REPLAYGAIN_TRACK_GAIN"]; len(vs) == 0 || vs[0] != "-1.00 dB" {
		t.Errorf("ReplayGain = %v, want kept on a whole-file copy", vs)
	}
}

// The album path embeds tags the same way per track and reports losses through
// the album's collected warnings.
func TestProcessAlbumWavPackCarriesTags(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedFLAC(t, dir)
	out := filepath.Join(dir, "out.wv")

	res, err := c.ProcessAlbum(ctx, []AlbumTrack{{Input: in, Output: out}}, -16, TranscodeSpec{Format: FormatWavPack})
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	pr, err := c.engine().Probe(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if vs := pr.Tags["TITLE"]; len(vs) == 0 || vs[0] != "Carried Title" {
		t.Errorf("album TITLE = %v, want carried", vs)
	}
	if vs := pr.Tags["REPLAYGAIN_TRACK_GAIN"]; len(vs) != 0 {
		t.Errorf("album ReplayGain = %v, want dropped (the gain changed the loudness)", vs)
	}
	found := false
	for _, w := range res.Warnings {
		if w.Code == WarnTagCarry && strings.Contains(w.Detail, "picture") {
			found = true
		}
	}
	if !found {
		t.Errorf("album warnings = %v, want the picture carry loss reported", res.Warnings)
	}
}

func TestVideoMuxTags(t *testing.T) {
	if got := videoMuxTags(nil); got != nil {
		t.Errorf("nil video = %v, want nil", got)
	}
}

// TestVideoMuxTagsMapping pins the field-to-key mapping to the same spellings
// the WaxLabel embed pass writes (doEmbed): TITLE, ARTIST from the channel
// name, RECORDINGDATE from the publish date.
func TestVideoMuxTagsMapping(t *testing.T) {
	v := &youtube.Video{
		Title:       "A Title",
		Author:      "A Channel",
		PublishDate: time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC),
	}
	got := videoMuxTags(v)
	want := []media.Tag{
		{Key: "TITLE", Value: "A Title"},
		{Key: "ARTIST", Value: "A Channel"},
		{Key: "RECORDINGDATE", Value: "2026-03-04"},
	}
	if len(got) != len(want) {
		t.Fatalf("videoMuxTags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("videoMuxTags[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	// Absent fields are skipped rather than stamped as zero values.
	if got := videoMuxTags(&youtube.Video{Title: "T"}); len(got) != 1 || got[0].Key != "TITLE" {
		t.Errorf("sparse video = %v, want TITLE only", got)
	}
}

// TestWarnMuxEmbedRequests pins the download-side loss report: cover art
// always has no APEv2 form, chapters only matter when metadata was requested
// and the video has them, and the tags-only tail appears only when tags were
// in fact requested.
func TestWarnMuxEmbedRequests(t *testing.T) {
	v := &youtube.Video{Title: "T", Chapters: []youtube.Chapter{{Title: "One"}}}
	cases := []struct {
		name string
		o    embedOptions
		want []string // substrings of the single warning detail; nil = no warning
	}{
		{"nothing requested", embedOptions{}, nil},
		{"thumbnail only", embedOptions{thumbnail: true}, []string{"cover art", "wavpack"}},
		{"metadata with chapters", embedOptions{metadata: true}, []string{"chapters", "text tags only"}},
		{"both", embedOptions{thumbnail: true, metadata: true}, []string{"cover art and chapters", "text tags only"}},
	}
	for _, c := range cases {
		em := newEmitter(nil, "")
		warnMuxEmbedRequests(em, "out.wv", media.CodecWavPack, v, c.o)
		ws := em.collected()
		if c.want == nil {
			if len(ws) != 0 {
				t.Errorf("%s: warnings = %v, want none", c.name, ws)
			}
			continue
		}
		if len(ws) != 1 || ws[0].Code != WarnMetadataEmbed {
			t.Fatalf("%s: warnings = %v, want one WarnMetadataEmbed", c.name, ws)
		}
		for _, sub := range c.want {
			if !strings.Contains(ws[0].Detail, sub) {
				t.Errorf("%s: detail = %q, want %q in it", c.name, ws[0].Detail, sub)
			}
		}
	}
	// A thumbnail-only request must not claim tags were carried.
	em := newEmitter(nil, "")
	warnMuxEmbedRequests(em, "out.wv", media.CodecWavPack, v, embedOptions{thumbnail: true})
	if ws := em.collected(); len(ws) == 1 && strings.Contains(ws[0].Detail, "text tags") {
		t.Errorf("thumbnail-only detail = %q, must not mention tags it never wrote", ws[0].Detail)
	}
}

// TestMuxEmbedLossDetailWording: with no text tags carried, the detail must
// not claim a carry that never happened.
func TestMuxEmbedLossDetailWording(t *testing.T) {
	if got := muxEmbedLossDetail("out.wv", media.CodecWavPack, []string{"1 picture"}, 0); !strings.HasPrefix(got, "no metadata carried") {
		t.Errorf("carried=0 detail = %q, want the no-carry wording", got)
	}
	if got := muxEmbedLossDetail("out.wv", media.CodecWavPack, []string{"1 picture"}, 3); !strings.HasPrefix(got, "metadata carried") {
		t.Errorf("carried=3 detail = %q, want the carried-with-losses wording", got)
	}
	if got := muxEmbedLossDetail("out.wv", media.CodecWavPack, nil, 3); got != "" {
		t.Errorf("no losses detail = %q, want empty", got)
	}
}

// TestSourceCodecClassParity pins the probe-string classifiers (lossySource,
// losslessSource) to media.Codec.IsLossless, so the three losslessness tables
// cannot drift: every codec WaxTap writes classifies consistently under its
// own probe name, and the decode-only WMA stays lossy.
func TestSourceCodecClassParity(t *testing.T) {
	codecs := []media.Codec{
		media.CodecFLAC, media.CodecALAC, media.CodecWAV, media.CodecAIFF,
		media.CodecMP3, media.CodecAAC, media.CodecHEAAC, media.CodecOpus,
		media.CodecVorbis, media.CodecWavPack, media.CodecAPE,
	}
	for _, c := range codecs {
		name := c.String()
		if c.IsLossless() {
			if !losslessSource(name) || lossySource(name) {
				t.Errorf("%s: lossless codec classifies as lossless=%v lossy=%v", name, losslessSource(name), lossySource(name))
			}
		} else {
			if losslessSource(name) || !lossySource(name) {
				t.Errorf("%s: lossy codec classifies as lossless=%v lossy=%v", name, losslessSource(name), lossySource(name))
			}
		}
	}
	// Probe-only names: PCM sources are lossless; WMA decodes are lossy; an
	// unknown codec is neither, so warnings keyed on the split fail closed.
	if !losslessSource("pcm") || !losslessSource("pcm_s16le") || lossySource("pcm") {
		t.Error("pcm must classify lossless")
	}
	if !lossySource("wma") || losslessSource("wma") {
		t.Error("wma must classify lossy")
	}
	if lossySource("mystery") || losslessSource("mystery") {
		t.Error("an unknown codec must classify as neither")
	}
}

// A cut of a lossless source into a container its codec cannot enter promotes
// to a lossy encode; the promotion now warns, since nothing in the request
// said quality would be lost.
func TestProcessWarnsImplicitLossyPromotion(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedWVFixture(t, c, dir)

	out := filepath.Join(dir, "out.mka")
	res, err := c.Process(ctx, ProcessRequest{
		Input: in,
		ProcessSpec: ProcessSpec{
			Output: ToFile(out),
			Cut:    &CutSpec{Ranges: []TimeRange{{Start: 0, End: 500 * time.Millisecond}}},
		},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	var lossy *Warning
	for i, w := range res.Warnings {
		if w.Code == WarnImplicitLossy {
			lossy = &res.Warnings[i]
		}
	}
	if lossy == nil {
		t.Fatalf("warnings = %v, want WarnImplicitLossy", res.Warnings)
	}
	for _, sub := range []string{"wavpack", "lossy", ".wv"} {
		if !strings.Contains(lossy.Detail, sub) {
			t.Errorf("detail = %q, want %q in it", lossy.Detail, sub)
		}
	}

	// An explicit lossy format takes its cost knowingly and must not warn.
	out2 := filepath.Join(dir, "asked.opus")
	res, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out2), Transcode: &TranscodeSpec{Format: FormatOpus}},
	})
	if err != nil {
		t.Fatalf("explicit opus: %v", err)
	}
	for _, w := range res.Warnings {
		if w.Code == WarnImplicitLossy {
			t.Errorf("explicit lossy format warned: %v", w)
		}
	}
}
