// Package avatar turns an uploaded image into a small, safe PNG.
//
// This is the only place in BitTabby that accepts a file, so the whole upload
// surface lives here and is worth stating plainly. Four things protect it:
//
//   - A byte cap read before anything else, so an enormous body is never held
//     in memory.
//   - A magic-byte allowlist. The declared content type and the filename are
//     both ignored -- neither is evidence, and the filename is never stored or
//     echoed anywhere, which is what makes path traversal structurally
//     impossible rather than merely handled.
//   - A pixel-dimension check taken from the header *before* decoding. This is
//     the control that matters most: a 30,000 x 30,000 PNG compresses to a few
//     kilobytes and expands to gigabytes, so a size cap alone does not stop a
//     decompression bomb. DecodeConfig reads only the header.
//   - A decode and re-encode. The bytes that get stored are ones this package
//     produced, not ones the uploader supplied, which strips EXIF (including
//     GPS), discards trailing data, and means a file crafted to be valid as two
//     formats at once cannot survive as either.
//
// The result is always PNG, so exactly one content type is ever served back.
package avatar

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"

	// Decoders for the formats the allowlist admits. Registered for their side
	// effect on image.Decode; GIF and JPEG are never written back out.
	_ "image/gif"
	_ "image/jpeg"
)

// Limits on what will be accepted.
const (
	// MaxUploadBytes bounds the request body. An avatar has no business being
	// larger, and the cap is applied while reading rather than after.
	MaxUploadBytes = 2 << 20 // 2 MiB
	// MaxPixels bounds width x height from the header, before any allocation.
	// 40 megapixels is past any real photograph a phone produces and far below
	// what it takes to exhaust memory.
	MaxPixels = 40 << 20
	// MaxDimension bounds either side on its own, so a 1 x 100,000,000 strip is
	// refused even though its area would pass.
	MaxDimension = 12000
	// Size is the stored edge length. Avatars render at 32-64 CSS pixels, so
	// 256 covers a 4x display with room to spare.
	Size = 256
)

// Errors returned by Process. They are distinguished so the handler can say
// something specific and true, rather than "that did not work".
var (
	ErrTooLarge     = errors.New("avatar: image is larger than the size limit")
	ErrUnsupported  = errors.New("avatar: not a PNG, JPEG, or GIF image")
	ErrTooManyPixel = errors.New("avatar: image dimensions are too large")
	ErrDecode       = errors.New("avatar: image could not be read")
	ErrEmpty        = errors.New("avatar: no image was uploaded")
)

// Process reads an uploaded image and returns the PNG to store.
//
// It never returns the uploader's bytes. The output is a square, at most
// Size x Size, re-encoded from decoded pixels.
func Process(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, ErrEmpty
	}

	// Read one byte past the cap so an oversized upload is detected rather than
	// silently truncated into a valid-looking image.
	raw, err := io.ReadAll(io.LimitReader(r, MaxUploadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if len(raw) == 0 {
		return nil, ErrEmpty
	}
	if len(raw) > MaxUploadBytes {
		return nil, ErrTooLarge
	}
	if !allowedMagic(raw) {
		return nil, ErrUnsupported
	}

	// Dimensions from the header, before decoding allocates anything.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, ErrDecode
	}
	if cfg.Width > MaxDimension || cfg.Height > MaxDimension ||
		int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, ErrTooManyPixel
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}

	out := square(src)

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("avatar: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// allowedMagic reports whether the leading bytes are one of the three formats
// admitted. The declared MIME type is not consulted: it is supplied by the
// client and is not evidence of anything.
func allowedMagic(b []byte) bool {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return true
	case len(b) >= 3 && bytes.Equal(b[:3], []byte{0xFF, 0xD8, 0xFF}): // JPEG SOI
		return true
	case len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a"))):
		return true
	}
	return false
}

// square center-crops to a square and downscales to at most Size.
//
// Cropping from the centre rather than stretching keeps faces the shape they
// were. It only ever shrinks: a 64 x 64 upload is stored at 64 x 64 rather than
// being blown up to 256, since enlarging invents detail and looks worse than
// letting the browser scale it.
func square(src image.Image) *image.RGBA {
	b := src.Bounds()
	side := min(b.Dx(), b.Dy())
	if side <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	crop := image.Rect(0, 0, side, side).Add(image.Point{
		X: b.Min.X + (b.Dx()-side)/2,
		Y: b.Min.Y + (b.Dy()-side)/2,
	})

	out := min(side, Size)
	dst := image.NewRGBA(image.Rect(0, 0, out, out))

	if out == side {
		draw.Draw(dst, dst.Bounds(), src, crop.Min, draw.Src)
		return dst
	}

	// Box filter: every destination pixel is the mean of the source pixels it
	// covers. For downscaling this is the right averaging filter and it is what
	// keeps a shrunken photograph from going speckled, which is what sampling a
	// single source pixel per destination pixel would do.
	//
	// Color.RGBA returns alpha-premultiplied components, so averaging them
	// directly is correct premultiplied averaging -- a transparent PNG does not
	// pick up dark halos around its edges.
	for y := range out {
		y0 := crop.Min.Y + y*side/out
		y1 := crop.Min.Y + (y+1)*side/out
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := range out {
			x0 := crop.Min.X + x*side/out
			x1 := crop.Min.X + (x+1)*side/out
			if x1 <= x0 {
				x1 = x0 + 1
			}

			var sr, sg, sb, sa, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, bl, a := src.At(sx, sy).RGBA()
					sr += uint64(r)
					sg += uint64(g)
					sb += uint64(bl)
					sa += uint64(a)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.Set(x, y, color.RGBA64{
				R: uint16(sr / n),
				G: uint16(sg / n),
				B: uint16(sb / n),
				A: uint16(sa / n),
			})
		}
	}
	return dst
}
