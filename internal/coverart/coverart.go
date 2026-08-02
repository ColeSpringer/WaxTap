// Package coverart reshapes a cover-art image without scaling it. YouTube
// composites an Art Track's square release art onto a 16:9 canvas and letterboxes
// that canvas into a 4:3 variant, so the delivered bytes carry background pillars
// and black rows as pixels rather than metadata. Peeling those bars and center-
// cropping what remains recovers the art exactly; a resize would squash it.
//
// It is stdlib-only and pure: bytes in, bytes out, no network and no filesystem.
package coverart

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
)

const (
	// jpegQuality re-encodes a cropped JPEG. The stdlib default of 75 is visibly
	// soft as a second generation on an already-lossy source.
	jpegQuality = 90

	// maxCoverSide and maxCoverPixels bound a decode. 8192 keeps the product
	// inside int on a 32-bit build (65535 squared overflows int32; 8192 squared
	// does not). 4M pixels covers 2560x1600, comfortably above any real cover, and
	// bounds the scratch buffer at 16 MB.
	maxCoverSide   = 8192
	maxCoverPixels = 4 << 20

	// borderTolerance is the per-channel range a line may span and still count as
	// a uniform border. Flat JPEG regions measure around 3/255 of deviation and
	// ringing at a bar boundary reaches 20-40/255, so twelve clears the noise floor
	// and still stops at real content.
	borderTolerance = 12
)

// Square crops an image down to its art: it peels uniform-color borders, then
// center-crops what remains to a square. It never scales, so the art keeps its
// proportions.
//
// The result is square within 1%, not exactly square: an input already that
// close is returned untouched rather than re-encoded for a one-pixel difference.
// Output keeps the input's format, JPEG in / JPEG out and PNG in / PNG out.
//
// It never returns nil bytes. On any failure it returns data unchanged along
// with a non-nil error, so a caller can embed the result unconditionally and
// treat the error as a report of a degraded result rather than a dropped one.
func Square(data []byte) ([]byte, error) {
	w, h, err := Dimensions(data)
	if err != nil {
		return data, err
	}
	// The side check runs first: it rejects a lopsided 65535x1 cheaply, and it
	// bounds w and h for everything below, keeping nearSquare's d*100 inside int
	// on a 32-bit build.
	if w > maxCoverSide || h > maxCoverSide {
		return data, fmt.Errorf("cover is %dx%d, past the %d-pixel side limit", w, h, maxCoverSide)
	}
	// Already square within 1%: free fast path, and it makes a legitimately square
	// cover unreachable by the trim below. It comes before the pixel budget
	// because it needs no decode: a large square cover has nothing to shape, so
	// reporting it as too big to shape would be a warning about work that was
	// never needed.
	if nearSquare(w, h) {
		return data, nil
	}
	// Bound the decode. The fetch's byte cap is not enough on its own: a 40 KB
	// JPEG can declare 65535x65535 and cost gigabytes to decode.
	if w*h > maxCoverPixels {
		return data, fmt.Errorf("cover is %dx%d, past the %d-pixel limit", w, h, maxCoverPixels)
	}

	src, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, fmt.Errorf("decode cover: %w", err)
	}
	b := src.Bounds()

	// Flatten once into an RGBA scratch and scan its Pix slice directly. A JPEG
	// decodes to *image.YCbCr, whose At(x, y) boxes a color.YCbCr into an
	// interface and runs a full YCbCr-to-RGB conversion per call; a uniform cover
	// peels nearly every line and would make millions of those calls. image/draw
	// has bulk fast paths for every type image/jpeg produces and all but paletted
	// of image/png's, all gated on the destination being *image.RGBA. The scratch
	// takes the source's bounds rather than a zero origin, so the rectangle the
	// trim returns is already in the original's coordinate space for SubImage.
	// Decoded JPEG and PNG always have Min == (0, 0), so this is invisible in
	// tests either way, which is why it is worth stating.
	flat := image.NewRGBA(b)
	draw.Draw(flat, b, src, b.Min, draw.Src)

	r := trimBorders(flat)
	// Over-trim guard, as a short-side ratio floor rather than a per-axis cap: the
	// legitimate horizontal trim is 43.75% by geometry (square art on a 16:9
	// canvas), so a per-axis cap would be the wrong shape. This catches the short
	// axis over-peeling because the long axis was blocked, e.g. content with 200px
	// uniform bands top and bottom of a 640x480 peeling to 640x80.
	if 2*min(r.Dx(), r.Dy()) < min(b.Dx(), b.Dy()) {
		r = b
	}
	// Center-crop unconditionally. On a 16:9 frame this is the primary mechanism,
	// not a fallback: an art track that renders a blurred enlarged copy of its art
	// as the backdrop peels nothing at all, and the plain center crop of a 1280x720
	// frame is exactly the 720x720 art.
	r = centerSquare(r)
	if r == b {
		return data, nil
	}

	// Crop the decoded source, not the scratch: SubImage is O(1) and shares the
	// buffer, and encoding the original keeps a JPEG in YCbCr end to end instead of
	// forcing a YCbCr-to-RGB-to-YCbCr round trip. jpeg.Encode handles a non-zero
	// Min correctly, including an odd offset on a 4:2:0 source, because the MCU
	// grid is anchored at bounds.Min; it always writes 4:2:0 whatever the source
	// subsampling was.
	type subImager interface {
		SubImage(image.Rectangle) image.Image
	}
	var crop image.Image
	if si, ok := src.(subImager); ok {
		crop = si.SubImage(r)
	} else {
		crop = flat.SubImage(r)
	}

	// The input length is a starting capacity, not a bound: re-encoding a lightly
	// cropped low-quality source at jpegQuality routinely runs past it, and a real
	// 1280x720 art track measured 108 KB in and 135 KB out at 720x720. It sizes
	// the buffer for the common case and bytes.Buffer regrows for the rest.
	buf := bytes.NewBuffer(make([]byte, 0, len(data)))
	switch format {
	case "png":
		err = png.Encode(buf, crop)
	default:
		err = jpeg.Encode(buf, crop, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return data, fmt.Errorf("encode cover: %w", err)
	}
	return buf.Bytes(), nil
}

// Dimensions reports an image's pixel size from its header, without decoding it.
func Dimensions(data []byte) (w, h int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("decode cover header: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

// nearSquare reports whether the two sides are within 1% of each other.
func nearSquare(w, h int) bool {
	d := w - h
	if d < 0 {
		d = -d
	}
	return d*100 <= min(w, h)
}

// trimBorders returns the largest rectangle left after peeling uniform-color
// lines off m, in m's own coordinate space.
//
// It peels the longer axis first and stops as soon as the rectangle is square.
// That ordering is what stops the peel from eating art. Consider 1280x720 with
// black pillars around 720x720 art whose own top and bottom rows are black, which
// is ordinary for a dark cover: peeling rows first, the art's black rows are
// contiguous with the pillars, so rows peel to 1280x640, columns then stop at the
// art edge for 720x640, and the center crop shaves 40 columns of real art off
// each side. Peeling the longer axis first, column 0 is pure pillar, the columns
// peel straight to 720x720, and the square stop fires with the art intact.
//
// A blocked axis falls through to the other one, so a nested bar (black rows
// around a pillared band) still resolves: on a 640x480 art track no column is
// uniform, the rows peel to 640x360, and the columns then peel pure pillar to
// 360x360.
func trimBorders(m *image.RGBA) image.Rectangle {
	r := m.Bounds()
	// The stop is exact squareness, not the 1% nearSquare used for the untouched
	// fast path. Stopping 1% early leaves bar behind, and the center crop then
	// splits that remainder evenly across both sides even when it all sat on one,
	// shaving real art off the other.
	for r.Dx() != r.Dy() {
		var peeled bool
		if r.Dx() >= r.Dy() {
			peeled = peelCols(m, &r) || peelRows(m, &r)
		} else {
			peeled = peelRows(m, &r) || peelCols(m, &r)
		}
		// Every iteration either shrinks r strictly or breaks, so this terminates.
		if !peeled {
			break
		}
	}
	return r
}

// peelCols removes at most one uniform column from each side of r, always
// leaving at least one column. It reports whether anything was removed.
func peelCols(m *image.RGBA, r *image.Rectangle) bool {
	peeled := false
	if r.Dx() > 1 && uniformCol(m, *r, r.Min.X) {
		r.Min.X++
		peeled = true
	}
	if r.Dx() > 1 && uniformCol(m, *r, r.Max.X-1) {
		r.Max.X--
		peeled = true
	}
	return peeled
}

// peelRows removes at most one uniform row from each side of r, always leaving
// at least one row. It reports whether anything was removed.
func peelRows(m *image.RGBA, r *image.Rectangle) bool {
	peeled := false
	if r.Dy() > 1 && uniformRow(m, *r, r.Min.Y) {
		r.Min.Y++
		peeled = true
	}
	if r.Dy() > 1 && uniformRow(m, *r, r.Max.Y-1) {
		r.Max.Y--
		peeled = true
	}
	return peeled
}

// uniformCol reports whether column x, restricted to r's rows, is a border line.
func uniformCol(m *image.RGBA, r image.Rectangle, x int) bool {
	if r.Dy() <= 0 {
		return false
	}
	off := m.PixOffset(x, r.Min.Y)
	var s spread
	s.seed(m.Pix[off : off+4 : off+4])
	for y := r.Min.Y + 1; y < r.Max.Y; y++ {
		off += m.Stride
		if !s.add(m.Pix[off : off+4 : off+4]) {
			return false
		}
	}
	return true
}

// uniformRow reports whether row y, restricted to r's columns, is a border line.
func uniformRow(m *image.RGBA, r image.Rectangle, y int) bool {
	if r.Dx() <= 0 {
		return false
	}
	off := m.PixOffset(r.Min.X, y)
	line := m.Pix[off : off+r.Dx()*4]
	var s spread
	s.seed(line[:4])
	for p := 4; p < len(line); p += 4 {
		if !s.add(line[p : p+4 : p+4]) {
			return false
		}
	}
	return true
}

// spread tracks the per-channel minimum and maximum seen along a line.
//
// A line is a border when its range stays within borderTolerance. Range, rather
// than distance from a seed pixel: no pixel is privileged, so a JPEG ringing
// artifact at position 0 cannot decide the line, which is the failure a seed
// formulation invites. Alpha is compared alongside the color channels, so a fully
// transparent PNG border peels as one color; *image.RGBA being premultiplied is
// what makes that work.
type spread struct{ lo, hi [4]uint8 }

func (s *spread) seed(px []uint8) {
	s.lo = [4]uint8(px)
	s.hi = s.lo
}

// add folds one pixel in and reports whether the line is still within tolerance.
func (s *spread) add(px []uint8) bool {
	for c := range 4 {
		v := px[c]
		s.lo[c] = min(s.lo[c], v)
		s.hi[c] = max(s.hi[c], v)
		if int(s.hi[c])-int(s.lo[c]) > borderTolerance {
			return false
		}
	}
	return true
}

// centerSquare returns the largest square centered inside r.
func centerSquare(r image.Rectangle) image.Rectangle {
	s := min(r.Dx(), r.Dy())
	x := r.Min.X + (r.Dx()-s)/2
	y := r.Min.Y + (r.Dy()-s)/2
	return image.Rect(x, y, x+s, y+s)
}
