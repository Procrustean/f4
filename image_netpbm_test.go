package main

import (
	"bytes"
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestNetpbmSurface(t *testing.T) {
	ppm := []byte("P6\n2 1\n255\n\x01\x02\x03\x04\x05\x06")
	surf, err := decodeNetpbmSurface(ppm)
	if err != nil {
		t.Fatalf("decodeNetpbmSurface: %v", err)
	}
	if surf.Width != 2 || surf.Height != 1 {
		t.Fatalf("geometry %dx%d, want 2x1", surf.Width, surf.Height)
	}
	r, g, b, a := surf.PixelAt(1, 0)
	if r != 4 || g != 5 || b != 6 || a != 255 {
		t.Errorf("pixel(1,0) = %d,%d,%d,%d, want 4,5,6,255", r, g, b, a)
	}
}

func TestDecodeNetpbmSurfaceStream(t *testing.T) {
	// The PAM pipe (converter stdout) path must read the same bytes.
	ppm := []byte("P6\n2 1\n255\n\x01\x02\x03\x04\x05\x06")
	surf, err := decodeNetpbmSurfaceStream(bytes.NewReader(ppm))
	if err != nil {
		t.Fatalf("decodeNetpbmSurfaceStream: %v", err)
	}
	if surf.Width != 2 || surf.Height != 1 {
		t.Fatalf("stream geometry %dx%d, want 2x1", surf.Width, surf.Height)
	}
}

func TestDecodeNetpbmRejectsGarbage(t *testing.T) {
	if _, err := decodeNetpbmSurface([]byte("P9 garbage")); err == nil {
		t.Error("an unreadable header must be refused, not hang")
	}
}

func decodePix(t *testing.T, data []byte) []byte {
	t.Helper()
	img, err := decodeNetpbm(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decodeNetpbm: %v", err)
	}
	n, ok := img.(*image.NRGBA)
	if !ok {
		t.Fatalf("decode type %T, want *image.NRGBA", img)
	}
	return n.Pix
}

func TestNetpbmP1AdjacentBits(t *testing.T) {
	// P1 bits need no separators: 01010101 must decode as 8 pixels.
	pix := decodePix(t, []byte("P1\n1 8\n01010101"))
	want := []byte{255, 255, 255, 255, 0, 0, 0, 255, 255, 255, 255, 255, 0, 0, 0, 255,
		255, 255, 255, 255, 0, 0, 0, 255, 255, 255, 255, 255, 0, 0, 0, 255}
	if !bytes.Equal(pix, want) {
		t.Errorf("P1 adjacent bits = % x, want % x", pix, want)
	}
}

func TestNetpbmP1WhitespaceAndComments(t *testing.T) {
	pix := decodePix(t, []byte("P1\r\n2 2\r\n# header note\r\n1\t0\r\n# inline\r\n0\n1"))
	want := []byte{0, 0, 0, 255, 255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 255}
	if !bytes.Equal(pix, want) {
		t.Errorf("P1 spaced pix = % x, want % x", pix, want)
	}
}

func TestNetpbmRasterBoundary(t *testing.T) {
	// Only maxval's leftover line terminator sits between header and raster:
	// a first pixel of 0x23 is data, not a comment line.
	for name, c := range map[string]struct {
		header string
		raster []byte
	}{
		"LF":          {"P5\n2 2\n255\n", []byte{5, 5, 5, 5}},
		"CRLF":        {"P5\n2 2\n255\r\n", []byte{5, 5, 5, 5}},
		"hash_first":  {"P6\n1 2\n255\n", []byte{0x23, 1, 2, 4, 5, 6}},
		"space_first": {"P6\n1 1\n255\n", []byte{0x20, 0x20, 0x20}},
	} {
		t.Run(name, func(t *testing.T) {
			pix := decodePix(t, append([]byte(c.header), c.raster...))
			if name == "hash_first" {
				want := []byte{0x23, 1, 2, 255, 4, 5, 6, 255}
				if !bytes.Equal(pix, want) {
					t.Fatalf("pix = % x, want % x", pix, want)
				}
			}
		})
	}
	// Bytes after the raster header (blanks/comments) belong to the picture.
	for _, data := range [][]byte{
		[]byte("P7\nWIDTH 1\nHEIGHT 1\nDEPTH 1\nMAXVAL 255\nENDHDR\n# x"),
		[]byte("P7\nWIDTH 2\nHEIGHT 1\nDEPTH 1\nMAXVAL 255\nENDHDR\n\x00\x00"),
	} {
		pix := decodePix(t, data)
		if pix[0] != 0x23 && pix[0] != 0x00 {
			t.Fatalf("pix = % x", pix)
		}
	}
}

func TestNetpbmP4Boundary(t *testing.T) {
	// P4 bits are MSB-first; raster starts right after the header line.
	for _, data := range [][]byte{
		[]byte("P4\n8 1\n\xff"), // all-0xFF row = all black
	} {
		pix := decodePix(t, data)
		if len(pix) != 32 {
			t.Fatalf("len %d", len(pix))
		}
		for i := 0; i < 32; i += 4 {
			if pix[i] != 0 || pix[i+1] != 0 || pix[i+2] != 0 || pix[i+3] != 255 {
				t.Fatalf("P4 byte %d = % x, want black", i, pix[i:i+4])
			}
		}
	}
	// '#' right after header is raster data: 0x23's first bit 0 = white pixel.
	pix := decodePix(t, []byte("P4\n8 1\n\x23"))
	if pix[0] != 0xff || pix[1] != 0xff || pix[2] != 0xff {
		t.Fatalf("first P4 pixel = % x, want ff ff ff", pix[:3])
	}
}

func TestNetpbmP7TupleTypeValidation(t *testing.T) {
	good := []byte("P7\nWIDTH 2\nHEIGHT 1\nDEPTH 4\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n" +
		"\x01\x02\x03\x04\x05\x06\x07\x08")
	pix := decodePix(t, good)
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	if !bytes.Equal(pix, want) {
		t.Fatalf("P7 pix = % x, want % x", pix, want)
	}
	if _, err := decodeNetpbm(bytes.NewReader([]byte(
		"P7\nWIDTH 2\nHEIGHT 1\nDEPTH 3\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\n\x01\x02\x03\x04\x05\x06"))); err == nil {
		t.Error("TUPLTYPE RGB_ALPHA with DEPTH 3 must be rejected")
	}
	// Unknown tuple types pass through on depth.
	if _, err := decodeNetpbm(bytes.NewReader([]byte(
		"P7\nWIDTH 1\nHEIGHT 1\nDEPTH 3\nMAXVAL 255\nTUPLTYPE CUSTOM\nENDHDR\n\x01\x02\x03"))); err != nil {
		t.Errorf("custom TUPLTYPE must be accepted: %v", err)
	}
}

func TestNetpbmP4OddWidth(t *testing.T) {
	// width 9 -> row padded to 2 bytes; last pixel is bit 7 of byte 2.
	pix := decodePix(t, []byte("P4\n9 1\n\xaa\x80"))
	want := []byte{
		0, 0, 0, 255, 255, 255, 255, 255, 0, 0, 0, 255, 255, 255, 255, 255,
		0, 0, 0, 255, 255, 255, 255, 255, 0, 0, 0, 255, 255, 255, 255, 255,
		0, 0, 0, 255,
	}
	if !bytes.Equal(pix, want) {
		t.Errorf("P4 odd width pix = % x, want % x", pix, want)
	}
}

func TestNetpbmP2CommentsBetweenPixels(t *testing.T) {
	pix := decodePix(t, []byte("P2\n2 1\n255\n10 # r\n20\n"))
	want := []byte{10, 10, 10, 255, 20, 20, 20, 255}
	if !bytes.Equal(pix, want) {
		t.Errorf("P2 pix = % x, want % x", pix, want)
	}
}

func TestNetpbmP3TrailingWhitespace(t *testing.T) {
	pix := decodePix(t, []byte("P3\n1 2\n255\n1 2 3\n\n4 5 6\n"))
	want := []byte{1, 2, 3, 255, 4, 5, 6, 255}
	if !bytes.Equal(pix, want) {
		t.Errorf("P3 pix = % x, want % x", pix, want)
	}
}

func TestNetpbmPFMLittleEndian(t *testing.T) {
	// Negative scale = little-endian floats (unsafe float32 view path).
	// 2x1 gray: values 0.5 and 1.0 with absScale 1.0 -> 128 and 255.
	data := []byte("Pf\n2 1\n-1.0\n\x00\x00\x00\x3f\x00\x00\x80\x3f")
	pix := decodePix(t, data)
	want := []byte{128, 128, 128, 255, 255, 255, 255, 255}
	if !bytes.Equal(pix, want) {
		t.Fatalf("PFM LE pix = % x, want % x", pix, want)
	}
}

func TestNetpbmPFMBigEndian(t *testing.T) {
	// 2 rows bottom-to-top: first file row (0.5) is the bottom (y=1).
	data := []byte("Pf\n1 2\n1.0\n\x3f\x00\x00\x00\x3f\x80\x00\x00")
	pix := decodePix(t, data)
	want := []byte{255, 255, 255, 255, 128, 128, 128, 255}
	if !bytes.Equal(pix, want) {
		t.Fatalf("PFM BE pix = % x, want % x", pix, want)
	}
}

func TestNetpbm16bitScaling(t *testing.T) {
	pix := decodePix(t, []byte("P6\n1 1\n65535\n\x00\x00\x80\x00\xff\xff"))
	// Channels 0x0000 -> 0, 0x8000=32768 -> (32768+128)/257 = 128, 0xffff -> 255.
	want := []byte{0, 128, 255, 255}
	if !bytes.Equal(pix, want) {
		t.Fatalf("16-bit pix = % x, want % x", pix, want)
	}
}

func TestNetpbmLargeDimensions(t *testing.T) {
	// Keep the default suite small: this validates a sizeable decoded surface
	// without allocating hundreds of MiB just to exercise the header guard.
	w, h := 1200, 1200
	img, err := decodeNetpbm(io.MultiReader(
		bytes.NewReader([]byte("P5\n1200 1200\n255\n")),
		io.LimitReader(zeroReader{}, int64(w*h)),
	))
	if err != nil {
		t.Fatalf("large decode failed: %v", err)
	}
	nrgba, ok := img.(*image.NRGBA)
	if !ok || nrgba.Bounds().Dx() != w || nrgba.Bounds().Dy() != h {
		t.Fatalf("bad result %T %v", img, img.Bounds())
	}
	if nrgba.Pix[0] != 0 || nrgba.Pix[1] != 0 || nrgba.Pix[2] != 0 || nrgba.Pix[3] != 255 {
		t.Fatalf("first pixel = %v", nrgba.Pix[:4])
	}
}

func TestNetpbmRejectsOversizedDimensionsBeforeAllocation(t *testing.T) {
	for _, header := range []string{
		"P5\n16385 1\n255\n",
		"P5\n20000 100\n255\n",
	} {
		if _, err := decodeNetpbm(bytes.NewReader([]byte(header))); err == nil {
			t.Errorf("header %q was accepted despite the allocation limit", header)
		}
	}
}

// zeroReader yields an endless stream of zeros without allocating.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func FuzzDecodeNetpbm(f *testing.F) {
	// Seed with the real fixtures so the corpus starts from valid files.
	entries, err := os.ReadDir(filepath.Join("testdata", "netpbm"))
	if err == nil {
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join("testdata", "netpbm", e.Name()))
			if err == nil {
				f.Add(b)
			}
		}
	}
	f.Add([]byte("P7\nWIDTH 2\nHEIGHT 1\nDEPTH 3\nMAXVAL 255\nTUPLTYPE RGB_ALPHA\nENDHDR\nABC"))
	f.Add([]byte("PF\n1 1\n-1.0\n\x00\x00\x00\x3f"))
	// Edge-case headers (corpus seeds).
	f.Add([]byte("P5\n100 100\n0\n"))                                          // maxVal = 0
	f.Add([]byte("P5\n100 100\n65536\n"))                                      // maxVal > 65535
	f.Add([]byte("P5\n100 100\n-1\n"))                                         // negative maxVal
	f.Add([]byte("P5\n0 100\n255\n"))                                          // width = 0
	f.Add([]byte("P5\n100 0\n255\n"))                                          // height = 0
	f.Add([]byte("P5\n20000 100\n255\n"))                                      // width > maxNetpbmDimension
	f.Add([]byte("P7\nWIDTH 1\nHEIGHT 1\nDEPTH 5\nMAXVAL 255\nENDHDR\nAAAAA")) // depth > 4
	f.Add([]byte("PF\n2 1\n0.0\n\x00\x00\x00\x00\x00\x00\x00\x00"))            // scale 0
	f.Add([]byte("P4\n9 1\n\xff\x00"))                                         // odd width with padding
	f.Add([]byte("P1\n3 1\n101"))                                              // adjacent bits
	f.Fuzz(func(t *testing.T, data []byte) {
		// Must never panic, even on truncated/corrupt headers.
		_, _ = decodeNetpbm(bytes.NewReader(data))
		_, _ = decodeNetpbmSurfaceStream(bytes.NewReader(data))
	})
}

// 8-bit fast path unrolls 64-bit groups (depths 1/2/3); 9 pixels exercise
// both the grouped and scalar-tail loops.

func TestNetpbmP5GrayFastPath(t *testing.T) {
	pix := decodePix(t, []byte("P5\n9 1\n255\n\x01\x02\x03\x04\x05\x06\x07\x08\x09"))
	var want []byte
	for _, g := range []byte{1, 2, 3, 4, 5, 6, 7, 8, 9} {
		want = append(want, g, g, g, 255)
	}
	if !bytes.Equal(pix, want) {
		t.Fatalf("P5 gray expand = %v, want %v", pix, want)
	}
}

func TestNetpbmP7GrayAlphaFastPath(t *testing.T) {
	// DEPTH 2, 16 pixels: four 4-pixel groups + tail.
	var body []byte
	var want []byte
	for i := 0; i < 16; i++ {
		g, a := byte(i), byte(255-i)
		body = append(body, g, a)
		want = append(want, g, g, g, a)
	}
	pix := decodePix(t, append([]byte("P7\nWIDTH 16\nHEIGHT 1\nDEPTH 2\nMAXVAL 255\nTUPLTYPE GRAYSCALE_ALPHA\nENDHDR\n"), body...))
	if !bytes.Equal(pix, want) {
		t.Fatalf("P7 gray+alpha expand = %v, want %v", pix, want)
	}
}

// 9 samples pin the 64-bit pair unroll + odd tail and exact /257 rounding.
func TestNetpbm16PackedScale(t *testing.T) {
	vals := []uint16{0x0000, 0x0001, 0x00FF, 0x4040, 0x7FFF, 0x8000, 0xFFFF, 0x8001, 0x0101}
	// 9 samples = 3 pixels x depth 3.
	var body []byte
	for _, v := range vals {
		body = append(body, byte(v>>8), byte(v))
	}
	pix := decodePix(t, append([]byte("P7\nWIDTH 3\nHEIGHT 1\nDEPTH 3\nMAXVAL 65535\nTUPLTYPE RGB\nENDHDR\n"), body...))
	var want []byte
	for i := 0; i < 3; i++ {
		for c := 0; c < 3; c++ {
			want = append(want, uint8((uint32(vals[i*3+c])+128)/257))
		}
		want = append(want, 255)
	}
	if !bytes.Equal(pix, want) {
		t.Fatalf("P7 16-bit pack = %v, want %v", pix, want)
	}
}
