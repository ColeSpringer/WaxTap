package waxtap

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// The WavPack/APE outputs take their APEv2 metadata from the same WaxLabel
// post-pass as every other format: tags, cover art, and the loss reporting all
// ride carryTags/doEmbed, with no mux-time special case.

// taggedWVFixture synthesizes a WavPack file tagged through WaxLabel (an APEv2
// block, the file's only tag form), including one own-audio value.
func taggedWVFixture(t *testing.T, c *Client, dir string) string {
	t.Helper()
	ctx := context.Background()
	wav := filepath.Join(t.TempDir(), "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.wv")
	if _, err := c.engine().Transcode(ctx, wav, in, media.Spec{Codec: media.CodecWavPack}); err != nil {
		t.Fatalf("synth wv: %v", err)
	}
	if err := mediatest.TagFile(ctx, in,
		"TITLE", "From APEv2",
		"REPLAYGAIN_TRACK_GAIN", "-1.00 dB"); err != nil {
		t.Fatalf("tag fixture: %v", err)
	}
	return in
}

// A tagged source's metadata reaches a WavPack output through the carry pass:
// text tags and the cover picture land in the APEv2 block, own-audio values
// are dropped (the encode re-derived the samples), and the chapters that have
// no APEv2 form are reported as a carry loss.
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

	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	get := func(k tag.Key) string {
		if vs, _ := doc.Get(k); len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	if got := get(tag.Title); got != "Carried Title" {
		t.Errorf("TITLE = %q, want carried", got)
	}
	if got := get(tag.Artist); got != "Carried Artist" {
		t.Errorf("ARTIST = %q, want carried", got)
	}
	if got := get(tag.ReplayGainTrackGain); got != "" {
		t.Errorf("ReplayGain = %q, want dropped on a re-encode", got)
	}
	if n := len(doc.Pictures()); n != 1 {
		t.Errorf("pictures = %d, want the cover carried into the APEv2 Cover Art item", n)
	}

	var lossWarn string
	for _, w := range res.Warnings {
		if w.Code == WarnTagCarry {
			lossWarn = w.Detail
		}
	}
	if !strings.Contains(lossWarn, "chapters") {
		t.Errorf("warnings = %v, want a WarnTagCarry naming the chapters a wavpack file cannot hold", res.Warnings)
	}
	if strings.Contains(lossWarn, "picture dropped") || strings.Contains(lossWarn, "pictures dropped") {
		t.Errorf("warning %q claims the picture was dropped; it must carry", lossWarn)
	}
}

// A WavPack source's APEv2 metadata reaches a FLAC output through the same
// transfer as any readable source (the lossless-library migration shape).
func TestProcessWavPackToFLACCarriesTags(t *testing.T) {
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
		t.Errorf("TITLE = %v, want the source tag carried", vs)
	}
	if vs, _ := doc.Get(tag.ReplayGainTrackGain); len(vs) != 0 {
		t.Errorf("ReplayGain = %v, want dropped on a re-encode", vs)
	}
}

// A whole-file remux of a WavPack source keeps every tag through the facade,
// own-audio values included: the audio bytes are unchanged. The engine's remux
// writes no tags, so this is the carry pass restoring them.
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

// The album path carries metadata per track through the same pass and folds
// the per-track carry warnings into one album warning with a track count.
func TestProcessAlbumWavPackCarriesTags(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	in := taggedFLAC(t, dir)
	out1 := filepath.Join(dir, "out1.wv")
	out2 := filepath.Join(dir, "out2.wv")

	res, err := c.ProcessAlbum(ctx, []AlbumTrack{
		{Input: in, Output: out1},
		{Input: in, Output: out2},
	}, -16, TranscodeSpec{Format: FormatWavPack})
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	for _, out := range []string{out1, out2} {
		doc, err := waxlabel.ParseFile(ctx, out)
		if err != nil {
			t.Fatalf("parse %s: %v", out, err)
		}
		if vs, _ := doc.Get(tag.Title); len(vs) == 0 || vs[0] != "Carried Title" {
			t.Errorf("%s TITLE = %v, want carried", out, vs)
		}
		if vs, _ := doc.Get(tag.ReplayGainTrackGain); len(vs) != 0 {
			t.Errorf("%s ReplayGain = %v, want dropped (the gain changed the loudness)", out, vs)
		}
		if n := len(doc.Pictures()); n != 1 {
			t.Errorf("%s pictures = %d, want the cover carried", out, n)
		}
	}
	var carries []string
	for _, w := range res.Warnings {
		if w.Code == WarnTagCarry {
			carries = append(carries, w.Detail)
		}
	}
	if len(carries) != 1 {
		t.Fatalf("tag-carry warnings = %v, want the per-track losses folded into one", carries)
	}
	if !strings.Contains(carries[0], "chapters") || !strings.Contains(carries[0], "(and 1 more track)") {
		t.Errorf("detail = %q, want the chapter loss and the track count", carries[0])
	}
}

// A source whose only metadata is a chapter set carries nothing into an APEv2
// output; the warning must say so instead of claiming a carry with losses.
func TestProcessChaptersOnlyToWavPackSaysNoCarry(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	wav := filepath.Join(dir, "in.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(3, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.flac")
	if _, err := c.Process(ctx, ProcessRequest{
		Input:       wav,
		ProcessSpec: ProcessSpec{Output: ToFile(in), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	}); err != nil {
		t.Fatalf("fixture transcode: %v", err)
	}
	doc, err := waxlabel.ParseFile(ctx, in)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	plan, err := doc.Edit().SetChapters(
		waxlabel.Chapter{Title: "One"},
		waxlabel.Chapter{Start: time.Second, Title: "Two"},
	).Prepare()
	if err != nil {
		t.Fatalf("prepare chapters: %v", err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatalf("write chapters: %v", err)
	}

	out := filepath.Join(dir, "out.wv")
	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatWavPack}},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	var detail string
	for _, w := range res.Warnings {
		if w.Code == WarnTagCarry {
			detail = w.Detail
		}
	}
	if !strings.HasPrefix(detail, "no metadata carried to") || !strings.Contains(detail, "chapters") {
		t.Errorf("warnings = %v, want a no-carry warning naming the chapters", res.Warnings)
	}
}

// appendAPEv2 appends a minimal APEv2 tag (header, items, footer) to path. It
// exists to plant item names WaxLabel itself would never write.
func appendAPEv2(t *testing.T, path string, items [][2]string) {
	t.Helper()
	var body bytes.Buffer
	for _, kv := range items {
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(kv[1])))
		_ = binary.Write(&body, binary.LittleEndian, uint32(0))
		body.WriteString(kv[0])
		body.WriteByte(0)
		body.WriteString(kv[1])
	}
	const hasHeader, isHeader = uint32(1) << 31, uint32(1) << 29
	frame := func(flags uint32) []byte {
		b := []byte("APETAGEX")
		b = binary.LittleEndian.AppendUint32(b, 2000)
		b = binary.LittleEndian.AppendUint32(b, uint32(body.Len()+32)) // items + footer
		b = binary.LittleEndian.AppendUint32(b, uint32(len(items)))
		b = binary.LittleEndian.AppendUint32(b, flags)
		return append(b, make([]byte, 8)...)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, chunk := range [][]byte{frame(hasHeader | isHeader), body.Bytes(), frame(hasHeader)} {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
}

// An APEv2 item whose name the canonical vocabulary cannot represent never
// becomes a transfer item, so the carry warning relays the source's own read
// warning instead: the value left behind is reported, not silent.
func TestProcessWavPackCarryReportsUnprojectableKeys(t *testing.T) {
	c := newOfflineClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	wav := filepath.Join(t.TempDir(), "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "in.wv")
	if _, err := c.engine().Transcode(ctx, wav, in, media.Spec{Codec: media.CodecWavPack}); err != nil {
		t.Fatalf("synth wv: %v", err)
	}
	appendAPEv2(t, in, [][2]string{{"Title", "Ok"}, {"BAD~KEY", "x"}})

	out := filepath.Join(dir, "out.flac")
	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(out), Transcode: &TranscodeSpec{Format: FormatFLAC}},
	})
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	doc, err := waxlabel.ParseFile(ctx, out)
	if err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if vs, _ := doc.Get(tag.Title); len(vs) == 0 || vs[0] != "Ok" {
		t.Errorf("TITLE = %v, want the representable tag carried", vs)
	}
	var detail string
	for _, w := range res.Warnings {
		if w.Code == WarnTagCarry {
			detail = w.Detail
		}
	}
	if !strings.Contains(detail, "BAD~KEY") || !strings.Contains(detail, "not represented in canonical tags") {
		t.Errorf("warnings = %v, want the unprojectable key reported", res.Warnings)
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
