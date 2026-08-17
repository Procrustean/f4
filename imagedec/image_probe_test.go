package imagedec

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestProbeImageHead(t *testing.T) {
	// PNG
	png := makeTestPNG(t, 321, 123, color.RGBA{A: 255})
	if h, ok := ProbeImageHead(png); !ok || h.Width != 321 || h.Height != 123 {
		t.Errorf("png: %+v ok=%v", h, ok)
	}

	// JPEG with an EXIF orientation: the probe must report both.
	jpg := exifOrientationJPEG(makeTestJPEG(t, 40, 30), 6)
	if h, ok := ProbeImageHead(jpg); !ok || h.Width != 40 || h.Height != 30 || h.Orientation != 6 {
		t.Errorf("jpeg: %+v ok=%v", h, ok)
	}

	// GIF
	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, image.NewPaletted(image.Rect(0, 0, 17, 9), color.Palette{color.Black}), nil); err != nil {
		t.Fatalf("gif encode: %v", err)
	}
	if h, ok := ProbeImageHead(gifBuf.Bytes()); !ok || h.Width != 17 || h.Height != 9 {
		t.Errorf("gif: %+v ok=%v", h, ok)
	}

	// BMP
	bmp := bmpFile(5, 7, 24, nil, [][]byte{
		make([]byte, 20), make([]byte, 20), make([]byte, 20), make([]byte, 20), make([]byte, 20), make([]byte, 20), make([]byte, 20),
	})
	if h, ok := ProbeImageHead(bmp); !ok || h.Width != 5 || h.Height != 7 {
		t.Errorf("bmp: %+v ok=%v", h, ok)
	}

	// QOI
	if h, ok := ProbeImageHead(qoiFile()); !ok || h.Width != 2 || h.Height != 2 {
		t.Errorf("qoi: %+v ok=%v", h, ok)
	}

	// TIFF
	tiff := makeTestTIFF(t, 640, 480, 6)
	if h, ok := ProbeImageHead(tiff); !ok || h.Width != 640 || h.Height != 480 || h.Orientation != 6 {
		t.Errorf("tiff: %+v ok=%v", h, ok)
	}

	// NetPBM P6 and P7
	if h, ok := ProbeImageHead([]byte("P6\n# c\n10 9\n255\n")); !ok || h.Width != 10 || h.Height != 9 {
		t.Errorf("netpbm p6: %+v ok=%v", h, ok)
	}
	if h, ok := ProbeImageHead([]byte("P7\nWIDTH 12\nHEIGHT 8\nDEPTH 3\nMAXVAL 255\nENDHDR\n")); !ok || h.Width != 12 || h.Height != 8 {
		t.Errorf("netpbm p7: %+v ok=%v", h, ok)
	}

	// Garbage and short heads answer false, never a wrong size.
	if _, ok := ProbeImageHead([]byte("not an image")); ok {
		t.Error("garbage must not identify")
	}
	// A valid PNG signature with a non-IHDR first chunk is malformed; its
	// bytes are not dimensions.
	badPNG := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, make([]byte, 16)...)
	badPNG[12], badPNG[13], badPNG[14], badPNG[15] = 'I', 'D', 'A', 'T'
	if h, ok := ProbeImageHead(badPNG); ok {
		t.Errorf("a malformed png must not identify: %+v ok=%v", h, ok)
	}
	if _, ok := ProbeImageHead(nil); ok {
		t.Error("an empty head must not identify")
	}
	if _, ok := ProbeImageHead(png[:16]); ok {
		t.Error("a head that cannot reach the size fields must not identify")
	}
}

// makeTestTIFF builds a minimal little-endian TIFF with the given size and
// orientation in IFD0.
func makeTestTIFF(t *testing.T, w, h, orient int) []byte {
	t.Helper()
	le := binary.LittleEndian
	var out []byte
	out = append(out, 'I', 'I')
	out = le.AppendUint16(out, 42)
	out = le.AppendUint32(out, 8) // IFD0 right after the header
	out = le.AppendUint16(out, 3) // three entries
	entry := func(tag, typ uint16, val uint32) {
		out = le.AppendUint16(out, tag)
		out = le.AppendUint16(out, typ)
		out = le.AppendUint32(out, 1)
		out = le.AppendUint32(out, val)
	}
	entry(256, 4, uint32(w))      // ImageWidth
	entry(257, 4, uint32(h))      // ImageLength
	entry(274, 3, uint32(orient)) // Orientation
	out = le.AppendUint32(out, 0) // no next IFD
	return out
}

// probeBenchImage builds a picture of the given format for the benchmarks;
// TB covers both testing.T and testing.B.
func probeBenchImage(tb testing.TB, format string, w, h int) []byte {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 6), G: uint8(y * 8), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	switch format {
	case "png":
		if err := png.Encode(&buf, img); err != nil {
			tb.Fatalf("png encode: %v", err)
		}
	case "jpg":
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			tb.Fatalf("jpeg encode: %v", err)
		}
	}
	return buf.Bytes()
}

func BenchmarkProbeImageHead(b *testing.B) {
	heads := map[string][]byte{
		"png": probeBenchImage(b, "png", 640, 480),
		"jpg": probeBenchImage(b, "jpg", 640, 480),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, head := range heads {
			if _, ok := ProbeImageHead(head); !ok {
				b.Fatal("probe failed")
			}
		}
	}
}

// BenchmarkDecodeConfig is the alternative: the stdlib's header parse, which
// walks the same bytes through the registered decoders.
func BenchmarkDecodeConfig(b *testing.B) {
	inputs := map[string][]byte{
		"png": probeBenchImage(b, "png", 640, 480),
		"jpg": probeBenchImage(b, "jpg", 640, 480),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, data := range inputs {
			if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	}
}
