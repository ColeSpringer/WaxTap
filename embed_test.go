package waxtap

import (
	"bytes"
	"context"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/coverart"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/youtube"
)

// coverServer serves body at /cover.jpg and returns a Client pointed at it,
// along with a Video whose ladder already reaches 1280x720 so the walk stays
// local: a shorter ladder would prepend i.ytimg.com probes and leave the test
// reaching the real network.
func coverServer(t *testing.T, body []byte) (*Client, *youtube.Video) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	c, err := New(Options{HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	v := &youtube.Video{
		ID:         "dummyVideo0",
		Title:      "Fixture Track",
		Author:     "Fixture Channel",
		Thumbnails: []youtube.Thumbnail{{URL: srv.URL + "/cover.jpg", Width: 1280, Height: 720}},
	}
	return c, v
}

// webpBytes is a RIFF/WEBP header. waxlabel.IsRecognizedImage accepts it, so it
// reaches the square step, where the stdlib cannot decode it.
var webpBytes = append([]byte("RIFF"), append([]byte{0x20, 0, 0, 0}, []byte("WEBP")...)...)

func TestCoverPictureFrameIsByteIdentical(t *testing.T) {
	// The default mode must not spend a re-encode generation on a feature nobody
	// asked for: the embedded bytes are what the CDN sent.
	sent := mediatest.JPEGBytes(mediatest.ArtTrackCover(1280, 720, 720, color.RGBA{A: 255}), 90)
	c, v := coverServer(t, sent)

	img, note, err := c.coverPicture(context.Background(), v, CoverArtFrame)
	if err != nil || note != "" {
		t.Fatalf("coverPicture = %v, %q; want no error and no note", err, note)
	}
	if !bytes.Equal(img, sent) {
		t.Error("CoverArtFrame must embed the delivered bytes verbatim")
	}
}

func TestCoverPictureDegradesOnAnUndecodableImage(t *testing.T) {
	c, v := coverServer(t, webpBytes)

	img, note, err := c.coverPicture(context.Background(), v, CoverArtSquare)
	if err != nil {
		t.Fatalf("an unshapeable picture is still worth embedding: %v", err)
	}
	if !bytes.Equal(img, webpBytes) {
		t.Error("a failed shape must fall back to the fetched bytes")
	}
	if note == "" {
		t.Error("a degraded shape must be reported, never silent")
	}
}

func TestDoEmbedReportsAnUnshapeablePicture(t *testing.T) {
	c, v := coverServer(t, webpBytes)
	path := writeWAVFixture(t)

	skip, err := c.doEmbed(context.Background(), path, "wav", v,
		embedOptions{thumbnail: true, coverArt: CoverArtSquare})
	if err != nil {
		t.Fatalf("doEmbed: %v", err)
	}
	if skip == "" || !strings.Contains(skip, "unshaped") {
		t.Errorf("skipReason = %q, want a report that the picture went in unshaped", skip)
	}
}

func TestDoEmbedSquaresWithoutWarning(t *testing.T) {
	sent := mediatest.JPEGBytes(mediatest.ArtTrackCover(1280, 720, 720, color.RGBA{A: 255}), 90)
	c, v := coverServer(t, sent)
	path := writeWAVFixture(t)

	skip, err := c.doEmbed(context.Background(), path, "wav", v,
		embedOptions{thumbnail: true, metadata: true, coverArt: CoverArtSquare})
	if err != nil {
		t.Fatalf("doEmbed: %v", err)
	}
	// The over-trim guard and the near-square fast path both deliver a picture the
	// user asked for, so nothing on the success path may warn.
	if skip != "" {
		t.Errorf("skipReason = %q, want empty", skip)
	}

	// The picture actually landed, and landed square.
	out, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	pic := embeddedJPEG(out)
	if pic == nil {
		t.Fatal("no JPEG cover picture found in the tagged file")
	}
	w, h, derr := coverart.Dimensions(pic)
	if derr != nil {
		t.Fatalf("embedded picture: %v", derr)
	}
	if w != h || w < 716 || w > 724 {
		t.Errorf("embedded cover = %dx%d, want a square within 4px of the 720px art", w, h)
	}
}

// writeWAVFixture stages a short WAV, a container WaxLabel writes pictures into
// and WaxFlow probes without a remux.
func writeWAVFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "track.wav")
	if err := os.WriteFile(path, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// embeddedJPEG returns the first embedded JPEG in a tagged file, found by its
// SOI/EOI markers. It avoids re-parsing the container just to read one picture.
func embeddedJPEG(data []byte) []byte {
	start := bytes.Index(data, []byte{0xFF, 0xD8, 0xFF})
	if start < 0 {
		return nil
	}
	end := bytes.LastIndex(data[start:], []byte{0xFF, 0xD9})
	if end < 0 {
		return nil
	}
	return data[start : start+end+2]
}
