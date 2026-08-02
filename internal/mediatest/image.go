package mediatest

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// checkerCell is the checkerboard square size used by every art fixture. It is
// large enough that a JPEG encode of the art does not smear one cell into the
// next, and small enough that every row and every column of the art crosses a
// cell boundary. That second property is what the trim tests rely on: no line of
// the art is uniform, so a border peel stops exactly at the art's edge and the
// resulting rectangle can be asserted precisely.
const checkerCell = 40

// checkerLight and checkerDark are the checkerboard colors. They are far apart
// so no tolerance a border test picks could mistake the art for a flat border,
// and off black/white so a fixture's background color can never coincide with
// both.
var (
	checkerDark  = color.RGBA{R: 40, G: 40, B: 40, A: 255}
	checkerLight = color.RGBA{R: 220, G: 220, B: 220, A: 255}
)

// SolidCover returns a w x h image of a single color.
func SolidCover(w, h int, c color.RGBA) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(m, m.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	return m
}

// ArtTrackCover returns the shape a YouTube "Art Track" thumbnail carries: a
// square art x art checkerboard centered on a 16:9 band of the pillar color,
// with black rows filling the rest of the canvas. The band's height is art, so
// passing w == art yields letterbox-only and h == art yields pillars-only.
func ArtTrackCover(w, h, art int, pillar color.RGBA) image.Image {
	m := SolidCover(w, h, color.RGBA{A: 255}).(*image.RGBA)
	band := centered(image.Rect(0, 0, w, art), m.Bounds())
	draw.Draw(m, band, &image.Uniform{C: pillar}, image.Point{}, draw.Src)
	drawChecker(m, centered(image.Rect(0, 0, art, art), m.Bounds()))
	return m
}

// DarkEdgedArtCover is ArtTrackCover whose art carries its own uniform border of
// edge rows top and bottom, in the pillar color. It is the case that makes the
// trim's axis preference load-bearing: those rows are contiguous with the
// surrounding bars, so peeling the short axis first would eat into the art.
func DarkEdgedArtCover(w, h, art, edge int, pillar color.RGBA) image.Image {
	m := ArtTrackCover(w, h, art, pillar).(*image.RGBA)
	box := centered(image.Rect(0, 0, art, art), m.Bounds())
	top := image.Rect(box.Min.X, box.Min.Y, box.Max.X, box.Min.Y+edge)
	bottom := image.Rect(box.Min.X, box.Max.Y-edge, box.Max.X, box.Max.Y)
	draw.Draw(m, top, &image.Uniform{C: pillar}, image.Point{}, draw.Src)
	draw.Draw(m, bottom, &image.Uniform{C: pillar}, image.Point{}, draw.Src)
	return m
}

// GradientPillarCover returns centered checkerboard art on a horizontal
// gradient. Each background column is one color, so the columns still peel, but
// no two are the same color.
func GradientPillarCover(w, h, art int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		v := uint8(x * 255 / max(w-1, 1))
		draw.Draw(m, image.Rect(x, 0, x+1, h), &image.Uniform{C: color.RGBA{R: v, G: 0, B: 255 - v, A: 255}}, image.Point{}, draw.Src)
	}
	drawChecker(m, centered(image.Rect(0, 0, art, art), m.Bounds()))
	return m
}

// BlurredBackdropCover returns centered checkerboard art on a backdrop that
// varies along both axes, the way an art track that renders a blurred enlarged
// copy of its cover does. Nothing peels off an image like this, so it is the
// fixture for the center crop standing on its own.
func BlurredBackdropCover(w, h, art int) image.Image {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / max(w-1, 1)),
				G: uint8(y * 255 / max(h-1, 1)),
				B: uint8((x + y) * 255 / max(w+h-2, 1)),
				A: 255,
			})
		}
	}
	drawChecker(m, centered(image.Rect(0, 0, art, art), m.Bounds()))
	return m
}

// OffsetArtCover returns art x art checkerboard art placed at (x, y) on a solid
// background, for off-center and full-bleed cases. The art rectangle is clipped
// to the canvas, so an art size at or above the canvas dimensions fills it.
func OffsetArtCover(w, h, art, x, y int, bg color.RGBA) image.Image {
	m := SolidCover(w, h, bg).(*image.RGBA)
	drawChecker(m, image.Rect(x, y, x+art, y+art).Intersect(m.Bounds()))
	return m
}

// PNGBytes encodes an image as PNG. Encoding to memory cannot fail, so a fixture
// error is a programming mistake and panics.
func PNGBytes(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// JPEGBytes encodes an image as JPEG at the given quality. A lower quality
// widens the ringing at a hard edge, which is what makes it worth naming per
// call site rather than fixing here.
func JPEGBytes(img image.Image, quality int) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// centered returns inner centered inside outer, keeping inner's size.
func centered(inner, outer image.Rectangle) image.Rectangle {
	x := outer.Min.X + (outer.Dx()-inner.Dx())/2
	y := outer.Min.Y + (outer.Dy()-inner.Dy())/2
	return image.Rect(x, y, x+inner.Dx(), y+inner.Dy())
}

// drawChecker paints a checkerboard over r. The phase is anchored at r.Min so
// the art's own first row and column always cross a cell boundary.
func drawChecker(m *image.RGBA, r image.Rectangle) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := checkerDark
			if ((x-r.Min.X)/checkerCell+(y-r.Min.Y)/checkerCell)%2 == 0 {
				c = checkerLight
			}
			m.SetRGBA(x, y, c)
		}
	}
}
