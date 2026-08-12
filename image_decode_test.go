package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/unxed/vtui"
)

func makeTestPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("cannot build the test image: %v", err)
	}
	return buf.Bytes()
}

func TestImageExtensionDetection(t *testing.T) {
	cases := map[string]bool{
		"photo.png":          true,
		"PHOTO.PNG":          true,
		"a/b/c.jpeg":         true,
		"archive.tar.gz":     false,
		"notes.txt":          false,
		"noextension":        false,
		"trailingdot.":       false,
		"dir.png/inner.txt":  false,
		"C:\\pics\\shot.JPG": true,
	}
	for path, want := range cases {
		if got := IsImageFile(path); got != want {
			t.Errorf("%q: got %v, want %v", path, got, want)
		}
	}
}

func TestDecodeImagePNG(t *testing.T) {
	data := makeTestPNG(t, 5, 3, color.RGBA{R: 10, G: 20, B: 30, A: 255})

	surf, name, err := DecodeImage("shot.png", data)
	if err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if name != "go-std" {
		t.Errorf("unexpected decoder %q", name)
	}
	if surf.Width != 5 || surf.Height != 3 {
		t.Fatalf("wrong geometry %dx%d", surf.Width, surf.Height)
	}
	r, g, b, a := surf.PixelAt(2, 1)
	if r != 10 || g != 20 || b != 30 || a != 255 {
		t.Errorf("wrong pixel %d,%d,%d,%d", r, g, b, a)
	}
}

func TestDecodeImageRejectsGarbage(t *testing.T) {
	if _, _, err := DecodeImage("shot.png", []byte("not a picture")); err == nil {
		t.Error("garbage must not decode")
	}
	if _, _, err := DecodeImage("notes.txt", makeTestPNG(t, 2, 2, color.RGBA{A: 255})); err == nil {
		t.Error("an unclaimed extension must be refused")
	}
}

func TestImageDecoderPriorityAndOverride(t *testing.T) {
	saved := imageDecoders
	defer func() { imageDecoders = saved }()

	called := ""
	RegisterImageDecoder(ImageDecoder{
		Name:       "test-high",
		Priority:   100,
		Extensions: []string{"png"},
		Decode: func(data []byte) (*vtui.ImageSurface, error) {
			called = "test-high"
			return vtui.NewImageSurface(1, 1), nil
		},
	})

	list := ImageDecodersFor("a.png")
	if len(list) < 2 || list[0].Name != "test-high" {
		t.Fatalf("priority order is wrong: %v", list)
	}
	before := len(list)

	if _, name, err := DecodeImage("a.png", nil); err != nil || name != "test-high" {
		t.Fatalf("the highest priority decoder must win, got %q %v", name, err)
	}
	if called != "test-high" {
		t.Error("the decoder was not actually invoked")
	}

	// Registering the same name again replaces it rather than duplicating.
	RegisterImageDecoder(ImageDecoder{
		Name:       "test-high",
		Priority:   100,
		Extensions: []string{"png"},
		Decode:     func(data []byte) (*vtui.ImageSurface, error) { return nil, nil },
	})
	if len(ImageDecodersFor("a.png")) != before {
		t.Error("re-registering a name must replace the old entry")
	}
}

func TestParseImageDecoderPriorities(t *testing.T) {
	got := ParseImageDecoderPriorities("go-std:5 | external:-10 ; nonsense ; bad:x")
	if len(got) != 2 || got["go-std"] != 5 || got["external"] != -10 {
		t.Fatalf("parsed %v", got)
	}
	if ParseImageDecoderPriorities("") != nil {
		t.Error("an empty setting must produce no overrides at all")
	}
}

func TestImageDecoderPrioritiesFromConfiguration(t *testing.T) {
	saved := imageDecoders
	t.Cleanup(func() {
		imageDecoders = saved
		SetImageDecoderPriorities(nil)
	})

	RegisterImageDecoder(ImageDecoder{
		Name:       "test-low",
		Priority:   -50,
		Extensions: []string{"png"},
		Decode:     func([]byte) (*vtui.ImageSurface, error) { return nil, nil },
	})

	if list := ImageDecodersFor("a.png"); list[0].Name != "go-std" {
		t.Fatalf("without an override go-std wins, got %q", list[0].Name)
	}
	SetImageDecoderPriorities(map[string]int{"test-low": 99})
	if list := ImageDecodersFor("a.png"); list[0].Name != "test-low" {
		t.Fatalf("the override must reorder the decoders, got %q", list[0].Name)
	}
	SetImageDecoderPriorities(nil)
	if list := ImageDecodersFor("a.png"); list[0].Name != "go-std" {
		t.Fatalf("clearing the overrides must restore the order, got %q", list[0].Name)
	}
}

func TestImageDecoderCandidatesSharePriorityOrder(t *testing.T) {
	saved := imageDecoders
	t.Cleanup(func() {
		imageDecoders = saved
		SetImageDecoderPriorities(nil)
	})
	RegisterImageDecoder(ImageDecoder{
		Name:       externalImageDecoder,
		Priority:   -10,
		Extensions: []string{"png"},
		Decode:     func([]byte) (*vtui.ImageSurface, error) { return nil, nil },
	})
	RegisterImageDecoder(ImageDecoder{
		Name:       "test-equal-a",
		Priority:   7,
		Extensions: []string{"png"},
		Decode:     func([]byte) (*vtui.ImageSurface, error) { return nil, nil },
	})
	RegisterImageDecoder(ImageDecoder{
		Name:       "test-equal-b",
		Priority:   7,
		Extensions: []string{"jpg"},
		Decode:     func([]byte) (*vtui.ImageSurface, error) { return nil, nil },
	})

	claimed := imageDecoderCandidates("a.png")
	if len(claimed) < 3 {
		t.Fatalf("candidate chain = %v", claimed)
	}
	extension := imageDecodersForExtension("a.png")
	if extension[0].Name != claimed[0].Name {
		t.Fatalf("automatic and extension order disagree: %v vs %v", extension, claimed)
	}
	for i := 1; i < len(extension); i++ {
		if extension[i-1].Priority < extension[i].Priority {
			t.Fatalf("claimed decoder priorities are not deterministic: %v", extension)
		}
	}
	if claimed[len(claimed)-1].Name != externalImageDecoder {
		t.Fatalf("default external decoder must remain last resort: %v", claimed)
	}

	SetImageDecoderPriorities(map[string]int{externalImageDecoder: 100})
	overridden := imageDecoderCandidates("a.png")
	if overridden[0].Name != externalImageDecoder || imageDecodersForExtension("a.png")[0].Name != externalImageDecoder {
		t.Fatalf("external priority override diverged between automatic and claimed order: %v", overridden)
	}
}

func TestDecodeImageRejectsEmptyDecoderResult(t *testing.T) {
	saved := imageDecoders
	t.Cleanup(func() { imageDecoders = saved })

	RegisterImageDecoder(ImageDecoder{
		Name:       "test-empty",
		Priority:   100,
		Extensions: []string{".PNG", "png", ""},
		Decode:     func([]byte) (*vtui.ImageSurface, error) { return nil, nil },
	})
	_, _, err := decodeImagePreferred(nil, "a.png", nil, "test-empty")
	if !errors.Is(err, errEmptyDecoderResult) {
		t.Fatalf("error = %v, want errEmptyDecoderResult", err)
	}
	if got := len(ImageDecodersFor("a.png")); got < 2 {
		t.Fatalf("normalized registration unexpectedly changed decoder list: %d", got)
	}
}

// qoiFile builds the stream by hand, one chunk of each kind that matters.
func qoiFile() []byte {
	out := []byte("qoif")
	out = binary.BigEndian.AppendUint32(out, 2)
	out = binary.BigEndian.AppendUint32(out, 2)
	out = append(out, 4, 0)

	out = append(out, qoiOpRGBA, 10, 20, 30, 255)
	out = append(out, qoiOpRun|0)
	out = append(out, qoiOpDiff|(3<<4)|(2<<2)|1)
	out = append(out, qoiOpIndex|9)
	out = append(out, 0, 0, 0, 0, 0, 0, 0, 1)
	return out
}

func TestDecodeQOI(t *testing.T) {
	surf, err := decodeQOI(qoiFile())
	if err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if surf.Width != 2 || surf.Height != 2 {
		t.Fatalf("geometry: %dx%d", surf.Width, surf.Height)
	}
	want := [][4]byte{{10, 20, 30, 255}, {10, 20, 30, 255}, {11, 20, 29, 255}, {10, 20, 30, 255}}
	for i, w := range want {
		r, g, b, a := surf.PixelAt(i%2, i/2)
		if r != w[0] || g != w[1] || b != w[2] || a != w[3] {
			t.Errorf("pixel %d: got %d %d %d %d, want %v", i, r, g, b, a, w)
		}
	}
}

func TestDecodeQOIRejectsRubbish(t *testing.T) {
	if _, err := decodeQOI([]byte("not an image")); err == nil {
		t.Error("a file that is not QOI must be reported as such")
	}
	if _, err := decodeQOI(qoiFile()[:qoiHeaderSize+5]); err == nil {
		t.Error("a truncated stream must be reported as such")
	}
}

// bmpFile assembles a bottom-up image out of already packed rows.
func bmpFile(width, height, bits int, palette [][3]byte, rows [][]byte) []byte {
	info := make([]byte, bmpInfoHeaderSize)
	le := binary.LittleEndian
	le.PutUint32(info[0:4], bmpInfoHeaderSize)
	le.PutUint32(info[4:8], uint32(int32(width)))
	le.PutUint32(info[8:12], uint32(int32(height)))
	le.PutUint16(info[12:14], 1)
	le.PutUint16(info[14:16], uint16(bits))
	le.PutUint32(info[32:36], uint32(len(palette)))

	var pal []byte
	for _, c := range palette {
		pal = append(pal, c[2], c[1], c[0], 0)
	}
	offset := bmpFileHeaderSize + bmpInfoHeaderSize + len(pal)
	var pixels []byte
	for _, row := range rows {
		pixels = append(pixels, row...)
	}
	out := []byte{'B', 'M'}
	out = le.AppendUint32(out, uint32(offset+len(pixels)))
	out = le.AppendUint32(out, 0)
	out = le.AppendUint32(out, uint32(offset))
	out = append(out, info...)
	out = append(out, pal...)
	return append(out, pixels...)
}

func TestDecodeBMP24(t *testing.T) {
	lower := []byte{0, 0, 255, 255, 255, 255, 0, 0}
	upper := []byte{255, 0, 0, 0, 255, 0, 0, 0}
	surf, err := decodeBMP(bmpFile(2, 2, 24, nil, [][]byte{lower, upper}))
	if err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if r, g, b, a := surf.PixelAt(0, 0); r != 0 || g != 0 || b != 255 || a != 255 {
		t.Errorf("top left: got %d %d %d %d", r, g, b, a)
	}
	if r, g, b, _ := surf.PixelAt(1, 0); r != 0 || g != 255 || b != 0 {
		t.Errorf("top right: got %d %d %d", r, g, b)
	}
	if r, g, b, _ := surf.PixelAt(0, 1); r != 255 || g != 0 || b != 0 {
		t.Errorf("bottom left: got %d %d %d", r, g, b)
	}
	if r, g, b, _ := surf.PixelAt(1, 1); r != 255 || g != 255 || b != 255 {
		t.Errorf("bottom right: got %d %d %d", r, g, b)
	}
}

func TestDecodeBMPTopDownAndPalette(t *testing.T) {
	palette := [][3]byte{{10, 20, 30}, {40, 50, 60}}
	surf, err := decodeBMP(bmpFile(2, -1, 8, palette, [][]byte{{0, 1, 0, 0}}))
	if err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if r, g, b, a := surf.PixelAt(0, 0); r != 10 || g != 20 || b != 30 || a != 255 {
		t.Errorf("first pixel: got %d %d %d %d", r, g, b, a)
	}
	if r, g, b, _ := surf.PixelAt(1, 0); r != 40 || g != 50 || b != 60 {
		t.Errorf("second pixel: got %d %d %d", r, g, b)
	}
}

func TestDecodeBMP32AlphaAndChannels(t *testing.T) {
	cases := []struct {
		name       string
		row        []byte
		wantAlpha0 byte
		wantAlpha1 byte
	}{
		{"empty alpha channel", []byte{1, 2, 3, 0, 4, 5, 6, 0}, 255, 255},
		{"explicit alpha", []byte{1, 2, 3, 0, 4, 5, 6, 128}, 0, 128},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			surf, err := decodeBMP(bmpFile(2, -1, 32, nil, [][]byte{tc.row}))
			if err != nil {
				t.Fatalf("decoding failed: %v", err)
			}
			r, g, b, alpha := surf.PixelAt(0, 0)
			if r != 3 || g != 2 || b != 1 {
				t.Errorf("BGRA channels were not converted to RGBA: got %d,%d,%d", r, g, b)
			}
			if alpha != tc.wantAlpha0 {
				t.Errorf("first alpha = %d, want %d", alpha, tc.wantAlpha0)
			}
			_, _, _, alpha = surf.PixelAt(1, 0)
			if alpha != tc.wantAlpha1 {
				t.Errorf("second alpha = %d, want %d", alpha, tc.wantAlpha1)
			}
		})
	}
}

func TestDecodeBMPRejectsRubbish(t *testing.T) {
	if _, err := decodeBMP([]byte("not an image at all, really")); err == nil {
		t.Error("a file that is not BMP must be reported as such")
	}
	if _, err := decodeBMP(bmpFile(4, -4, 24, nil, [][]byte{{1, 2, 3}})); err == nil {
		t.Error("a truncated stream must be reported as such")
	}
}

func TestDecodeImageFallsBackWhenTheExtensionLies(t *testing.T) {
	saved := imageDecoders
	defer func() { imageDecoders = saved }()

	RegisterImageDecoder(ImageDecoder{
		Name:       "test-broken",
		Priority:   100,
		Extensions: []string{"png"},
		Decode:     func(data []byte) (*vtui.ImageSurface, error) { return nil, nil },
	})

	data := makeTestPNG(t, 2, 2, color.RGBA{R: 1, A: 255})
	surf, name, err := DecodeImage("a.png", data)
	if err != nil {
		t.Fatalf("the fallback decoder should have succeeded: %v", err)
	}
	if name != "go-std" || surf.Width != 2 {
		t.Errorf("got %q %v", name, surf)
	}
}

// surfaceCheck compares the decoded pixels against an RGBA reference that
// went through image.NewRGBA().At: any conversion error shows up as a
// channel mismatch, and the opaque flag must be set on by-construction opaque
// formats.
func surfaceCheck(t *testing.T, surf *vtui.ImageSurface, want func(x, y int) color.RGBA) {
	t.Helper()
	if surf == nil || !surf.Valid() {
		t.Fatal("surface is invalid")
	}
	if !surf.Opaque {
		t.Error("surface must be marked opaque")
	}
	for y := 0; y < surf.Height; y++ {
		for x := 0; x < surf.Width; x++ {
			r, g, b, a := surf.PixelAt(x, y)
			w := want(x, y)
			if r != w.R || g != w.G || b != w.B || a != w.A {
				t.Errorf("pixel %d,%d: got %d %d %d %d, want %d %d %d %d",
					x, y, r, g, b, a, w.R, w.G, w.B, w.A)
			}
		}
	}
}

func TestSurfaceFromYCbCr(t *testing.T) {
	m := image.NewYCbCr(image.Rect(0, 0, 4, 3), image.YCbCrSubsampleRatio420)
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			m.Y[m.YOffset(x, y)] = byte(16 + x*40 + y*8)
			m.Cb[m.COffset(x, y)] = byte(64 + x*30)
			m.Cr[m.COffset(x, y)] = byte(128 + y*50)
		}
	}
	ref := image.NewRGBA(m.Bounds())
	for y := m.Rect.Min.Y; y < m.Rect.Max.Y; y++ {
		for x := m.Rect.Min.X; x < m.Rect.Max.X; x++ {
			ref.Set(x, y, m.At(x, y))
		}
	}
	surf, err := surfaceFromDecodedImage(m)
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	surfaceCheck(t, surf, func(x, y int) color.RGBA {
		return color.RGBAModel.Convert(ref.At(x, y)).(color.RGBA)
	})
}

func TestSurfaceFromGray(t *testing.T) {
	m := image.NewGray(image.Rect(0, 0, 5, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 5; x++ {
			m.Pix[y*m.Stride+x] = byte(x*50 + y*20)
		}
	}
	surf, err := surfaceFromDecodedImage(m)
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	surfaceCheck(t, surf, func(x, y int) color.RGBA {
		g := byte(x*50 + y*20)
		return color.RGBA{R: g, G: g, B: g, A: 255}
	})
}

func TestSurfaceFromCMYK(t *testing.T) {
	m := image.NewCMYK(image.Rect(0, 0, 3, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			o := y*m.Stride + x*4
			m.Pix[o], m.Pix[o+1], m.Pix[o+2], m.Pix[o+3] =
				byte(x*70), byte(y*90), byte(30), byte(40)
		}
	}
	surf, err := surfaceFromDecodedImage(m)
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	surfaceCheck(t, surf, func(x, y int) color.RGBA {
		o := y*m.Stride + x*4
		r, g, b := color.CMYKToRGB(m.Pix[o], m.Pix[o+1], m.Pix[o+2], m.Pix[o+3])
		return color.RGBA{R: r, G: g, B: b, A: 255}
	})
}

func TestSurfaceFromAlphaKeepsOpaqueFlag(t *testing.T) {
	// NRGBA with an alpha byte that is not 255 must not be marked opaque.
	nrgba := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			o := y*nrgba.Stride + x*4
			nrgba.Pix[o], nrgba.Pix[o+1], nrgba.Pix[o+2], nrgba.Pix[o+3] = 1, 2, 3, 128
		}
	}
	surf, err := surfaceFromDecodedImage(nrgba)
	if err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	if surf.Opaque {
		t.Error("a translucent NRGBA must not be opaque")
	}
	r, g, b, a := surf.PixelAt(1, 1)
	if r != 1 || g != 2 || b != 3 || a != 128 {
		t.Errorf("pixel: got %d %d %d %d", r, g, b, a)
	}
}

func TestSurfaceIsOpaqueHonorsStride(t *testing.T) {
	pix := make([]byte, 16)
	pix[3], pix[11] = 255, 255
	surf := vtui.NewImageSurfaceFromPix(1, 2, 8, pix)
	if !surfaceIsOpaque(surf) {
		t.Error("padding bytes must not be treated as pixels")
	}
}
