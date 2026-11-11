package waxtap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

// frontTrimmedVorbis writes an Ogg Vorbis stream that starts before time zero
// (mediatest.FrontTrimVorbis): a front trim Matroska states and a packet copy
// into Ogg cannot, so the engine declines that copy.
func frontTrimmedVorbis(t *testing.T, c *Client, dir string) string {
	t.Helper()
	ctx := context.Background()
	wav := filepath.Join(dir, "src.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(2, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.ogg")
	if _, err := c.Process(ctx, ProcessRequest{
		Input:       wav,
		ProcessSpec: ProcessSpec{Output: ToFile(plain), Transcode: &TranscodeSpec{Format: FormatVorbis}},
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	whole, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	trimmed, _, err := mediatest.FrontTrimVorbis(whole)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "trimmed.ogg")
	if err := os.WriteFile(in, trimmed, 0o644); err != nil {
		t.Fatal(err)
	}
	return in
}

// A request for the format a source is already in is delivered as a packet
// copy, which nobody asked for by name. Where the engine would decline that
// copy, the encoder the request did name delivers it: the front trim is
// applied by the decode, so the output holds exactly the audio the stream
// plays. An explicit copy of the same file is still refused, and a container
// that states the trim still gets the packets.
func TestProcessEncodesWhereAKeptCopyDeclines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newOfflineClient(t)
	in := frontTrimmedVorbis(t, c, dir)
	src, _ := mustProbeFile(t, c, in).AudioStream()
	if src.Delay <= 0 {
		t.Fatalf("the fixture states no front trim: %+v", src)
	}

	if ok, err := c.CanCopy(ctx, in, filepath.Join(dir, "x.ogg")); err != nil || ok {
		t.Errorf("CanCopy into Ogg = %v, %v; want false", ok, err)
	}
	if ok, err := c.CanCopy(ctx, in, filepath.Join(dir, "x.mka")); err != nil || !ok {
		t.Errorf("CanCopy into Matroska = %v, %v; want true", ok, err)
	}

	named := filepath.Join(dir, "named.ogg")
	res, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(named), Transcode: &TranscodeSpec{Format: FormatVorbis}},
	})
	if err != nil {
		t.Fatalf("a named vorbis into Ogg: %v, want the encoder the request named", err)
	}
	if !res.Transcoded {
		t.Error("Transcoded = false; the copy the engine declines cannot have been taken")
	}
	if out, _ := mustProbeFile(t, c, named).AudioStream(); out.CodecName != "vorbis" || out.Delay != 0 || out.Samples != src.Samples {
		t.Errorf("output = %s, %d samples after a %d-sample trim; want vorbis holding the %d the source plays, from time zero",
			out.CodecName, out.Samples, out.Delay, src.Samples)
	}

	_, err = c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "copy.ogg")), Transcode: &TranscodeSpec{Format: FormatCopy}},
	})
	if !errors.Is(err, ErrIncompatibleSpec) {
		t.Errorf("an explicit copy into Ogg = %v, want ErrIncompatibleSpec", err)
	}

	kept, err := c.Process(ctx, ProcessRequest{
		Input:       in,
		ProcessSpec: ProcessSpec{Output: ToFile(filepath.Join(dir, "named.mka")), Transcode: &TranscodeSpec{Format: FormatVorbis}},
	})
	if err != nil {
		t.Fatalf("a named vorbis into Matroska: %v", err)
	}
	if kept.Transcoded {
		t.Error("Transcoded = true into Matroska, which states the trim and takes the copy")
	}
}
