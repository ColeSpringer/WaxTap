package coverart

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/draw"
	"testing"

	"github.com/colespringer/waxtap/v3/internal/mediatest"
)

var (
	black  = color.RGBA{A: 255}
	pillar = color.RGBA{R: 90, G: 30, B: 140, A: 255}
)

// flatten mirrors what Square does before trimming, so a trimBorders case can be
// stated as an image and asserted as an exact rectangle.
func flatten(img image.Image) *image.RGBA {
	b := img.Bounds()
	m := image.NewRGBA(b)
	draw.Draw(m, b, img, b.Min, draw.Src)
	return m
}

func TestTrimBorders(t *testing.T) {
	cases := []struct {
		name string
		img  image.Image
		want image.Rectangle
	}{
		{
			// The reported case: 640x480 is a 16:9 canvas letterboxed into 4:3, so no
			// column is uniform (it spans black rows and pillar). The rows peel first,
			// then the columns.
			name: "nested bars",
			img:  mediatest.ArtTrackCover(640, 480, 360, pillar),
			want: image.Rect(140, 60, 500, 420),
		},
		{
			name: "pillars only stop at the square",
			img:  mediatest.ArtTrackCover(1280, 720, 720, pillar),
			want: image.Rect(280, 0, 1000, 720),
		},
		{
			name: "letterbox only",
			img:  mediatest.ArtTrackCover(360, 480, 360, black),
			want: image.Rect(0, 60, 360, 420),
		},
		{
			// Peeling the longer axis first saves the art. Peeling rows first would
			// eat the art's own 40px black edges along with the pillars and leave a
			// 720x640 rectangle whose center crop shaves 40 real columns off each side.
			name: "dark-edged art on black pillars",
			img:  mediatest.DarkEdgedArtCover(1280, 720, 720, 40, black),
			want: image.Rect(280, 0, 1000, 720),
		},
		{
			// Each background column is one color, so the columns still peel even
			// though no two of them match.
			name: "gradient pillars",
			img:  mediatest.GradientPillarCover(1280, 720, 720),
			want: image.Rect(280, 0, 1000, 720),
		},
		{
			// The art sits 100px left of center. A plain center crop would take
			// (200,0)-(600,400) and lose a quarter of it; the trim finds it.
			name: "off-center art",
			img:  mediatest.OffsetArtCover(800, 400, 400, 100, 0, pillar),
			want: image.Rect(100, 0, 500, 400),
		},
		{
			// A backdrop that varies along both axes has no uniform line at all.
			name: "blurred backdrop peels nothing",
			img:  mediatest.BlurredBackdropCover(1280, 720, 720),
			want: image.Rect(0, 0, 1280, 720),
		},
		{
			name: "no bars",
			img:  mediatest.OffsetArtCover(400, 300, 400, 0, 0, black),
			want: image.Rect(0, 0, 400, 300),
		},
		{
			// The square stop is what keeps a fully uniform image from collapsing to
			// a single pixel.
			name: "solid stops at the square",
			img:  mediatest.SolidCover(640, 480, pillar),
			want: image.Rect(80, 0, 560, 480),
		},
		{
			name: "one pixel",
			img:  mediatest.SolidCover(1, 1, pillar),
			want: image.Rect(0, 0, 1, 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimBorders(flatten(tc.img)); got != tc.want {
				t.Errorf("trimBorders = %v, want %v", got, tc.want)
			}
		})
	}
}

// decodedSize reports the pixel size of encoded image bytes.
func decodedSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	w, h, err := Dimensions(data)
	if err != nil {
		t.Fatalf("Dimensions: %v", err)
	}
	return w, h
}

func TestSquarePNG(t *testing.T) {
	cases := []struct {
		name       string
		img        image.Image
		wantW      int
		wantH      int
		wantFormat string
	}{
		{"art track", mediatest.ArtTrackCover(640, 480, 360, pillar), 360, 360, "png"},
		// Nothing peels here, so the center crop alone has to deliver the art.
		{"blurred backdrop", mediatest.BlurredBackdropCover(1280, 720, 720), 720, 720, "png"},
		// The over-trim guard discards a peel down to the 100x100 mark and leaves
		// the plain center crop.
		{"small centered mark", mediatest.OffsetArtCover(640, 480, 100, 270, 190, black), 480, 480, "png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := mediatest.PNGBytes(tc.img)
			out, err := Square(in)
			if err != nil {
				t.Fatalf("Square: %v", err)
			}
			w, h := decodedSize(t, out)
			if w != tc.wantW || h != tc.wantH {
				t.Errorf("Square = %dx%d, want %dx%d", w, h, tc.wantW, tc.wantH)
			}
			if _, format, derr := image.Decode(bytes.NewReader(out)); derr != nil || format != tc.wantFormat {
				t.Errorf("format = %q, %v; want %q", format, derr, tc.wantFormat)
			}
		})
	}
}

func TestSquareLeavesSquareInputUntouched(t *testing.T) {
	cases := []struct {
		name string
		img  image.Image
	}{
		{"exactly square", mediatest.BlurredBackdropCover(600, 600, 400)},
		// Within 1%: not worth a whole re-encode generation for four pixels.
		{"near square", mediatest.BlurredBackdropCover(600, 596, 400)},
		// Past the pixel budget but already square, so there is nothing to shape.
		// The fast path runs before that budget precisely so this does not report a
		// failure to do work that was never needed.
		{"square and past the pixel budget", mediatest.SolidCover(2500, 2500, pillar)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := mediatest.PNGBytes(tc.img)
			out, err := Square(in)
			if err != nil {
				t.Fatalf("Square: %v", err)
			}
			if !bytes.Equal(out, in) {
				t.Error("an already-square cover must be returned byte-identical, not re-encoded")
			}
		})
	}
}

func TestSquareJPEGKeepsFormatAndLandsNearTheArt(t *testing.T) {
	in := mediatest.JPEGBytes(mediatest.ArtTrackCover(640, 480, 360, pillar), 90)
	out, err := Square(in)
	if err != nil {
		t.Fatalf("Square: %v", err)
	}
	if _, format, derr := image.Decode(bytes.NewReader(out)); derr != nil || format != "jpeg" {
		t.Fatalf("format = %q, %v; want jpeg", format, derr)
	}
	// A JPEG carries real ringing at the bar boundaries, so the peel can stop a
	// few pixels either side of the exact edge a PNG lands on.
	w, h := decodedSize(t, out)
	if w != h {
		t.Errorf("Square = %dx%d, want a square", w, h)
	}
	if w < 356 || w > 364 {
		t.Errorf("Square side = %d, want within 4px of the 360px art", w)
	}
}

func TestSquareReturnsInputOnFailure(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"not an image", []byte("this is not an image at all, not even close")},
		// WebP reaches here through waxlabel.IsRecognizedImage but the stdlib
		// cannot decode it. Degrading to the original bytes is the whole point of
		// the never-nil contract, and is why there is no x/image dependency.
		{"webp header", append([]byte("RIFF"), append([]byte{0x20, 0, 0, 0}, []byte("WEBP")...)...)},
		// A tiny header can declare an enormous image. The size guard must reject
		// it before anything is allocated.
		{"oversized header", pngHeader(30000, 30000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Square(tc.data)
			if err == nil {
				t.Fatal("want a non-nil error reporting the degraded result")
			}
			if !bytes.Equal(out, tc.data) {
				t.Error("a failed shape must return the input bytes, never nil")
			}
		})
	}
}

func TestSquareWebPReportsAFormatError(t *testing.T) {
	webp := append([]byte("RIFF"), append([]byte{0x20, 0, 0, 0}, []byte("WEBP")...)...)
	if _, err := Square(webp); !errors.Is(err, image.ErrFormat) {
		t.Errorf("Square(webp) = %v, want an image.ErrFormat", err)
	}
}

// pngHeader builds a valid PNG signature plus IHDR chunk and nothing else, so
// DecodeConfig reports the declared size without any image data existing.
func pngHeader(w, h uint32) []byte {
	var ihdr bytes.Buffer
	ihdr.WriteString("IHDR")
	binary.Write(&ihdr, binary.BigEndian, w)
	binary.Write(&ihdr, binary.BigEndian, h)
	ihdr.Write([]byte{8, 2, 0, 0, 0}) // 8-bit truecolor, no interlace

	var out bytes.Buffer
	out.Write([]byte("\x89PNG\r\n\x1a\n"))
	binary.Write(&out, binary.BigEndian, uint32(ihdr.Len()-4))
	out.Write(ihdr.Bytes())
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(ihdr.Bytes()))
	return out.Bytes()
}

func TestDimensions(t *testing.T) {
	in := mediatest.PNGBytes(mediatest.SolidCover(320, 180, pillar))
	w, h, err := Dimensions(in)
	if err != nil || w != 320 || h != 180 {
		t.Errorf("Dimensions = %d, %d, %v; want 320, 180, nil", w, h, err)
	}
	if _, _, err := Dimensions([]byte("nope")); err == nil {
		t.Error("Dimensions on non-image bytes should error")
	}
}

// BenchmarkSquareUniform is the case that would expose a regression back to
// per-pixel At() calls: a fully uniform frame peels nearly every column.
func BenchmarkSquareUniform(b *testing.B) {
	in := mediatest.JPEGBytes(mediatest.SolidCover(1920, 1080, pillar), 90)
	b.ResetTimer()
	for b.Loop() {
		if _, err := Square(in); err != nil {
			b.Fatal(err)
		}
	}
}
