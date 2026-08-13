package imagedecoders

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
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

func TestIsVideoFile(t *testing.T) {
	for _, path := range []string{"clip.mp4", "CLIP.MKV", "a/b/movie.avi", "webm.webm", "c:\\v\\x.mov", "film.ts", "3gp.3gp"} {
		if !IsVideoFile(path) {
			t.Errorf("%q: expected a video file, got false", path)
		}
	}
	for _, path := range []string{"photo.jpg", "shot.png", "icon.ico", "vector.svg", "notes.txt", "noextension"} {
		if IsVideoFile(path) {
			t.Errorf("%q: expected a non-video file, got true", path)
		}
	}
}

// A video file must be served by its path-rendering decoder (the shell)
// alone: no WIC, stdlib or external fallback may claim a movie, in the
// automatic chain or in the F4 cycle.
func TestVideoFilesOnlyUsePathRenderingDecoders(t *testing.T) {
	saved := imageDecoders
	imageDecoders = nil
	t.Cleanup(func() { imageDecoders = saved })

	RegisterImageDecoder(ImageDecoder{
		Name:       "test-vid-path",
		Priority:   100,
		Extensions: []string{"mp4"},
		FromPath:   true,
		DecodeCtx: func(ctx context.Context, path string, data []byte) (*vtui.ImageSurface, error) {
			return nil, nil
		},
	})
	RegisterImageDecoder(ImageDecoder{
		Name:       "test-vid-bytes",
		Priority:   50,
		Extensions: []string{"mp4"},
		Decode:     func(data []byte) (*vtui.ImageSurface, error) { return nil, nil },
	})
	RegisterImageDecoder(ImageDecoder{
		Name:       "test-png",
		Priority:   10,
		Extensions: []string{"png"},
		Decode:     func(data []byte) (*vtui.ImageSurface, error) { return nil, nil },
	})

	cands := imageDecoderCandidates("clip.mp4")
	if len(cands) != 1 || cands[0].Name != "test-vid-path" {
		t.Fatalf("video candidates = %v, want only the path-rendering decoder", cands)
	}
	cycle := ImageCycleDecoders("clip.mp4")
	if len(cycle) != 1 || cycle[0].Name != "test-vid-path" {
		t.Fatalf("video cycle = %v, want only the path-rendering decoder", cycle)
	}
	// A picture keeps its full chain: the claimed decoder plus the fallbacks.
	if got := imageDecoderCandidates("a.png"); len(got) < 2 {
		t.Fatalf("a picture's chain shrank to %v", got)
	}
}

// PathDecoderFor must pick a decoder marked FromPath when one claims the
// extension, and nothing for a format nobody renders from its path.
func TestPathDecoderFor(t *testing.T) {
	saved := imageDecoders
	t.Cleanup(func() { imageDecoders = saved })

	RegisterImageDecoder(ImageDecoder{
		Name:       "test-path",
		Priority:   50,
		Extensions: []string{"xyz"},
		FromPath:   true,
		DecodeCtx: func(ctx context.Context, path string, data []byte) (*vtui.ImageSurface, error) {
			return vtui.NewImageSurface(2, 2), nil
		},
	})
	RegisterImageDecoder(ImageDecoder{
		Name:       "test-bytes",
		Priority:   100,
		Extensions: []string{"abc"},
		Decode:     func(data []byte) (*vtui.ImageSurface, error) { return nil, nil },
	})

	d, ok := PathDecoderFor("a.xyz")
	if !ok || d.Name != "test-path" {
		t.Fatalf("PathDecoderFor(a.xyz) = %q %v, want test-path", d.Name, ok)
	}
	surf, name, err := DecodeImageFromPath(context.Background(), "a.xyz", d)
	if err != nil || name != "test-path" || surf == nil || surf.Width != 2 {
		t.Fatalf("DecodeImageFromPath = %q %v %v", name, surf, err)
	}
	if _, ok := PathDecoderFor("a.abc"); ok {
		t.Error("a byte-rendering decoder must not be picked as a path decoder")
	}
	if _, ok := PathDecoderFor("a.png"); ok {
		t.Error("a format with no path decoder must not resolve to one")
	}
}

func TestDecodeImagePNG(t *testing.T) {
	data := makeTestPNG(t, 5, 3, color.RGBA{R: 10, G: 20, B: 30, A: 255})

	surf, name, err := DecodeImage("shot.png", data)
	if err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	// WIC wins on Windows, the stdlib everywhere else.
	if name != "go-std" && name != "wic" {
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
		Priority:   1000,
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
		Priority:   1000,
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

	// Without an override the low-priority decoder sits at the very bottom.
	if list := ImageDecodersFor("a.png"); list[len(list)-1].Name != "test-low" {
		t.Fatalf("without an override test-low must be last, got %q", list[len(list)-1].Name)
	}
	SetImageDecoderPriorities(map[string]int{"test-low": 1000})
	if list := ImageDecodersFor("a.png"); list[0].Name != "test-low" {
		t.Fatalf("the override must reorder the decoders, got %q", list[0].Name)
	}
	SetImageDecoderPriorities(nil)
	if list := ImageDecodersFor("a.png"); list[0].Name == "test-low" {
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
		Name:       ExternalImageDecoder,
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
	if claimed[len(claimed)-1].Name != ExternalImageDecoder {
		t.Fatalf("default external decoder must remain last resort: %v", claimed)
	}

	SetImageDecoderPriorities(map[string]int{ExternalImageDecoder: 1000})
	overridden := imageDecoderCandidates("a.png")
	if overridden[0].Name != ExternalImageDecoder || imageDecodersForExtension("a.png")[0].Name != ExternalImageDecoder {
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
	if !errors.Is(err, ErrEmptyDecoderResult) {
		t.Fatalf("error = %v, want ErrEmptyDecoderResult", err)
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

// icoFile wraps an embedded image (a PNG or a BMP payload) in a minimal ICO
// container with a single entry.
func icoFile(embedded []byte) []byte {
	out := make([]byte, 0, 6+16+len(embedded))
	out = append(out, 0, 0, 1, 0, 1, 0)          // reserved, icon, one entry
	out = append(out, 16, 16, 0, 0, 1, 0, 32, 0) // 16x16, 1 plane, 32 bpp
	out = binary.LittleEndian.AppendUint32(out, uint32(len(embedded)))
	out = binary.LittleEndian.AppendUint32(out, 22) // offset: dir + one entry
	return append(out, embedded...)
}

// icoBMPEntry builds the BMP payload of an ICO entry: the 40-byte header
// with the height doubled (XOR picture + AND mask), the bottom-up XOR rows
// and the AND mask.
func icoBMPEntry(width, height int, xor, and []byte) []byte {
	info := make([]byte, bmpInfoHeaderSize)
	le := binary.LittleEndian
	le.PutUint32(info[0:4], bmpInfoHeaderSize)
	le.PutUint32(info[4:8], uint32(int32(width)))
	le.PutUint32(info[8:12], uint32(int32(height*2)))
	le.PutUint16(info[12:14], 1)
	le.PutUint16(info[14:16], 32)
	out := make([]byte, 0, bmpInfoHeaderSize+len(xor)+len(and))
	out = append(out, info...)
	out = append(out, xor...)
	return append(out, and...)
}

// A classic ICO entry: a bottom-up 32 bpp XOR row (BGRA) whose AND mask
// makes the first pixel transparent. The decoder must honour the mask, which
// is where an ICO's real shape comes from.
func TestDecodeICOFromBMPEntry(t *testing.T) {
	// One row, two pixels: red (BGRA 00 00 FF 00) and blue (FF 00 00 00).
	xor := []byte{0, 0, 255, 0, 255, 0, 0, 0}
	// Mask row, padded to four bytes: pixel 0 set (transparent), pixel 1 clear.
	and := []byte{0x80, 0, 0, 0}
	surf, err := decodeICO(icoFile(icoBMPEntry(2, 1, xor, and)))
	if err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if surf.Width != 2 || surf.Height != 1 {
		t.Fatalf("geometry = %dx%d, want 2x1", surf.Width, surf.Height)
	}
	r, g, b, a := surf.PixelAt(0, 0)
	if r != 255 || g != 0 || b != 0 || a != 0 {
		t.Errorf("first pixel = %d,%d,%d,%d, want transparent red 255,0,0,0", r, g, b, a)
	}
	r, g, b, a = surf.PixelAt(1, 0)
	if r != 0 || g != 0 || b != 255 || a != 255 {
		t.Errorf("second pixel = %d,%d,%d,%d, want opaque blue 0,0,255,255", r, g, b, a)
	}
	if surf.Opaque {
		t.Error("an icon with a transparent pixel must not be marked opaque")
	}
}

// Modern icons embed a PNG; the container must hand it to the PNG decoder.
func TestDecodeICOFromPNGEntry(t *testing.T) {
	surf, err := decodeICO(icoFile(makeTestPNG(t, 2, 2, color.RGBA{R: 5, G: 6, B: 7, A: 255})))
	if err != nil {
		t.Fatalf("decoding failed: %v", err)
	}
	if surf.Width != 2 || surf.Height != 2 {
		t.Fatalf("geometry = %dx%d, want 2x2", surf.Width, surf.Height)
	}
	r, g, b, a := surf.PixelAt(1, 1)
	if r != 5 || g != 6 || b != 7 || a != 255 {
		t.Errorf("pixel = %d,%d,%d,%d, want 5,6,7,255", r, g, b, a)
	}
}

func TestDecodeICORejectsRubbish(t *testing.T) {
	if _, err := decodeICO([]byte("not an icon")); err == nil {
		t.Error("a file that is not ICO must be reported as such")
	}
	if _, err := decodeICO([]byte{0, 0, 1, 0, 0, 0}); err == nil {
		t.Error("an icon without entries must be reported as such")
	}
	if !IsImageFile("app.ico") {
		t.Error("an .ico file must be viewable through the built-in BMP decoder")
	}
}

// The F4 cycle offers the decoders that claim the extension, not the content
// sniffers the automatic chain may fall back to for a mislabelled file: a
// cycle that lands on go-bmp for a .jpg would pin a decoder that cannot read
// it and look like F4 did nothing.
func TestImageCycleOffersOnlyClaimingDecoders(t *testing.T) {
	for _, d := range ImageCycleDecoders("a.jpg") {
		claims := false
		for _, e := range d.Extensions {
			if e == "jpg" {
				claims = true
				break
			}
		}
		if !claims {
			t.Errorf("the cycle must not offer %q for a.jpg (it does not claim jpg)", d.Name)
		}
	}
	if got := len(ImageDecoderChoices("a.jpg")); got < 1 {
		t.Errorf("a jpg must still offer a cycle, got %d choices", got)
	}
}

// The F4 cycle advances one stop per press and wraps at the end; a single
// option has nothing to cycle to.
func TestImageNextDecoderCycles(t *testing.T) {
	opts := []ImageDecoderChoice{
		{key: "a", label: "a"},
		{key: "b", label: "b"},
		{key: "c", label: "c"},
	}
	// From the automatic chain (no pin), the cycle moves past the current.
	if got := ImageNextDecoder(opts, "", "a"); got != "b" {
		t.Errorf("from a, want b, got %q", got)
	}
	// A pinned decoder advances one step.
	if got := ImageNextDecoder(opts, "b", "b"); got != "c" {
		t.Errorf("from b, want c, got %q", got)
	}
	// The cycle wraps around to the first stop.
	if got := ImageNextDecoder(opts, "c", "c"); got != "a" {
		t.Errorf("from c, want a, got %q", got)
	}
	// One option cannot cycle.
	if got := ImageNextDecoder([]ImageDecoderChoice{{key: "a"}}, "", "a"); got != "" {
		t.Errorf("a single option must not cycle, got %q", got)
	}
}

func TestDecodeImageFallsBackWhenTheExtensionLies(t *testing.T) {
	saved := imageDecoders
	defer func() { imageDecoders = saved }()

	RegisterImageDecoder(ImageDecoder{
		Name:       "test-broken",
		Priority:   10000,
		Extensions: []string{"png"},
		Decode:     func(data []byte) (*vtui.ImageSurface, error) { return nil, nil },
	})

	data := makeTestPNG(t, 2, 2, color.RGBA{R: 1, A: 255})
	surf, name, err := DecodeImage("a.png", data)
	if err != nil {
		t.Fatalf("the fallback decoder should have succeeded: %v", err)
	}
	if name == "test-broken" {
		t.Errorf("the broken decoder must not win: %q", name)
	}
	if surf.Width != 2 {
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

// orientedSurface builds a 3x2 surface whose pixel values encode their
// coordinates, so a reorientation is easy to verify point by point.
func orientedSurface() *vtui.ImageSurface {
	surf := vtui.NewImageSurface(3, 2)
	for y := 0; y < 2; y++ {
		for x := 0; x < 3; x++ {
			o := y*surf.Stride + x*4
			surf.Pix[o], surf.Pix[o+1], surf.Pix[o+2], surf.Pix[o+3] = byte(10+x), byte(20+y), 0, 255
		}
	}
	return surf
}

func TestApplyImageOrientation(t *testing.T) {
	src := orientedSurface()
	src.Opaque = true

	// Orientation 1 (and anything out of range) must return the surface itself.
	if got := ApplyImageOrientation(src, 1); got != src {
		t.Fatal("orientation 1 must leave the surface untouched")
	}
	if got := ApplyImageOrientation(src, 0); got != src {
		t.Fatal("orientation 0 must leave the surface untouched")
	}

	// Each orientation maps (x,y) onto (sx,sy); 5-8 swap the sides.
	cases := []struct {
		orient int
		w, h   int
		sx, sy func(x, y int) int
	}{
		{2, 3, 2, func(x, y int) int { return 2 - x }, func(x, y int) int { return y }},
		{3, 3, 2, func(x, y int) int { return 2 - x }, func(x, y int) int { return 1 - y }},
		{4, 3, 2, func(x, y int) int { return x }, func(x, y int) int { return 1 - y }},
		{5, 2, 3, func(x, y int) int { return y }, func(x, y int) int { return x }},
		{6, 2, 3, func(x, y int) int { return 1 - y }, func(x, y int) int { return x }},
		{7, 2, 3, func(x, y int) int { return 1 - y }, func(x, y int) int { return 2 - x }},
		{8, 2, 3, func(x, y int) int { return y }, func(x, y int) int { return 2 - x }},
	}
	for _, tc := range cases {
		r := ApplyImageOrientation(src, tc.orient)
		if r.Width != tc.w || r.Height != tc.h {
			t.Errorf("orientation %d geometry = %dx%d, want %dx%d", tc.orient, r.Width, r.Height, tc.w, tc.h)
			continue
		}
		if !r.Opaque {
			t.Errorf("orientation %d lost the opaque flag", tc.orient)
		}
		for y := 0; y < 2; y++ {
			for x := 0; x < 3; x++ {
				rr, gg, _, aa := r.PixelAt(tc.sx(x, y), tc.sy(x, y))
				r0, g0, _, a0 := src.PixelAt(x, y)
				if rr != r0 || gg != g0 || aa != a0 {
					t.Errorf("orientation %d: (%d,%d) -> got %d,%d,%d want %d,%d,%d", tc.orient, x, y, rr, gg, aa, r0, g0, a0)
				}
			}
		}
	}
}

// pngHeader builds a structurally complete PNG that claims the given canvas
// but carries no pixel data. DecodeConfig reads only this far, so the
// dimension guard can be tested without allocating a picture of that size.
func pngHeader(w, h uint32) []byte {
	out := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8], ihdr[9], ihdr[10], ihdr[11], ihdr[12] = 8, 6, 0, 0, 0 // RGBA
	chunk := append([]byte("IHDR"), ihdr...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(ihdr)))
	out = append(out, chunk...)
	crc := crc32.NewIEEE()
	_, _ = crc.Write(chunk)
	out = binary.BigEndian.AppendUint32(out, crc.Sum32())
	// IEND so the stream parses as a whole PNG even without IDAT.
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, "IEND"...)
	return binary.BigEndian.AppendUint32(out, 0xAE426082)
}

// A tiny file that claims a huge canvas must be refused before the stdlib
// decoders allocate: 20000x20000 is 400M pixels, past imageMaxPixels, but a
// real file of that canvas would still be a few compressed bytes.
func TestDecodeImageWithStdlibRejectsDecompressionBomb(t *testing.T) {
	_, err := DecodeImageWithStdlib(pngHeader(20000, 20000))
	if err == nil || !strings.Contains(err.Error(), "pixel limit") {
		t.Fatalf("error = %v, want a pixel-limit refusal before allocation", err)
	}
}
