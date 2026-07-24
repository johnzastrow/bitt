package avatar

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// pngOf builds a PNG of the given size, filled with a recognizable pattern.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, format, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if format != "png" {
		t.Fatalf("stored format is %q, want png -- exactly one type is ever served", format)
	}
	return img
}

func TestProcessDownscalesToSquare(t *testing.T) {
	out, err := Process(bytes.NewReader(pngOf(t, 1000, 600)))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	img := decode(t, out)
	b := img.Bounds()
	if b.Dx() != Size || b.Dy() != Size {
		t.Errorf("stored %dx%d, want %dx%d", b.Dx(), b.Dy(), Size, Size)
	}
}

// A small upload is stored at its own size rather than enlarged, since
// upscaling invents detail and looks worse than letting the browser scale.
func TestProcessDoesNotUpscale(t *testing.T) {
	out, err := Process(bytes.NewReader(pngOf(t, 64, 64)))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if b := decode(t, out).Bounds(); b.Dx() != 64 || b.Dy() != 64 {
		t.Errorf("stored %dx%d, want 64x64", b.Dx(), b.Dy())
	}
}

func TestProcessAcceptsJPEGAndGIF(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 400, 400))
	for y := range 400 {
		for x := range 400 {
			src.Set(x, y, color.RGBA{R: uint8(x / 2), G: uint8(y / 2), B: 200, A: 255})
		}
	}

	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, src, nil); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	var gbuf bytes.Buffer
	if err := gif.Encode(&gbuf, src, nil); err != nil {
		t.Fatalf("gif encode: %v", err)
	}

	for name, in := range map[string][]byte{"jpeg": jbuf.Bytes(), "gif": gbuf.Bytes()} {
		out, err := Process(bytes.NewReader(in))
		if err != nil {
			t.Errorf("%s rejected: %v", name, err)
			continue
		}
		// Whatever went in, PNG comes out.
		decode(t, out)
	}
}

// The stored bytes are always this package's own output, never the uploader's.
// That is what strips metadata and defeats a file crafted to be valid as two
// formats at once.
func TestProcessNeverReturnsTheInputBytes(t *testing.T) {
	in := pngOf(t, 300, 300)
	out, err := Process(bytes.NewReader(in))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if bytes.Equal(in, out) {
		t.Error("the uploaded bytes were stored verbatim")
	}
	// A comment chunk appended after IEND must not survive.
	withTrailer := append(append([]byte{}, in...), []byte("TRAILING PAYLOAD")...)
	out2, err := Process(bytes.NewReader(withTrailer))
	if err != nil {
		t.Fatalf("Process with trailer: %v", err)
	}
	if bytes.Contains(out2, []byte("TRAILING PAYLOAD")) {
		t.Error("data appended after the image survived re-encoding")
	}
}

func TestProcessRejectsNonImages(t *testing.T) {
	cases := map[string][]byte{
		"html":           []byte("<html><script>alert(1)</script></html>"),
		"svg":            []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"elf":            {0x7F, 'E', 'L', 'F', 1, 1, 1, 0},
		"zip":            {'P', 'K', 3, 4, 0, 0, 0, 0},
		"webp":           append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...),
		"bmp":            append([]byte("BM"), make([]byte, 32)...),
		"png magic only": []byte("\x89PNG\r\n\x1a\n"),
	}
	for name, in := range cases {
		_, err := Process(bytes.NewReader(in))
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if name == "png magic only" {
			// Right magic, unreadable content: caught at decode, not by the
			// allowlist. Either way it must not be stored.
			if !errors.Is(err, ErrDecode) {
				t.Errorf("%s gave %v, want ErrDecode", name, err)
			}
			continue
		}
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s gave %v, want ErrUnsupported", name, err)
		}
	}
}

func TestProcessRejectsOversizedBodies(t *testing.T) {
	big := make([]byte, MaxUploadBytes+1)
	copy(big, "\x89PNG\r\n\x1a\n")
	if _, err := Process(bytes.NewReader(big)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("got %v, want ErrTooLarge", err)
	}
}

// The control that a byte cap cannot provide: a small file that decodes huge.
func TestProcessRejectsADecompressionBomb(t *testing.T) {
	// A uniform image compresses to almost nothing but declares vast
	// dimensions. This must be refused from the header, before decoding.
	bomb := image.NewRGBA(image.Rect(0, 0, MaxDimension+1, 8))
	var buf bytes.Buffer
	if err := png.Encode(&buf, bomb); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if buf.Len() > MaxUploadBytes {
		t.Skipf("fixture is %d bytes, larger than the byte cap, so it would be "+
			"caught by size rather than by dimensions", buf.Len())
	}
	if _, err := Process(bytes.NewReader(buf.Bytes())); !errors.Is(err, ErrTooManyPixel) {
		t.Errorf("got %v, want ErrTooManyPixel", err)
	}
}

func TestProcessRejectsEmpty(t *testing.T) {
	if _, err := Process(bytes.NewReader(nil)); !errors.Is(err, ErrEmpty) {
		t.Errorf("got %v, want ErrEmpty", err)
	}
	if _, err := Process(nil); !errors.Is(err, ErrEmpty) {
		t.Errorf("nil reader gave %v, want ErrEmpty", err)
	}
	if _, err := Process(strings.NewReader("")); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty reader gave %v, want ErrEmpty", err)
	}
}

// Transparency must survive the box filter without dark halos, which is what
// averaging non-premultiplied components would produce.
func TestProcessPreservesTransparency(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := range 512 {
		for x := range 512 {
			src.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 0}) // fully transparent
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("encode: %v", err)
	}

	out, err := Process(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	img := decode(t, out)
	if _, _, _, a := img.At(Size/2, Size/2).RGBA(); a != 0 {
		t.Errorf("alpha at the centre is %d, want 0 -- transparency was lost", a)
	}
}
