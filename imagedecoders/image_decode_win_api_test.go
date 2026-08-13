//go:build windows

package imagedecoders

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWICDecodesPNGFromMemory(t *testing.T) {
	data := makeTestPNG(t, 5, 3, color.RGBA{R: 10, G: 20, B: 30, A: 255})

	surf, err := decodeImageWIC(context.Background(), "", data)
	if err != nil {
		t.Fatalf("WIC decode failed: %v", err)
	}
	if surf.Width != 5 || surf.Height != 3 {
		t.Fatalf("geometry = %dx%d, want 5x3", surf.Width, surf.Height)
	}
	r, g, b, a := surf.PixelAt(2, 1)
	if r != 10 || g != 20 || b != 30 || a != 255 {
		t.Errorf("pixel = %d,%d,%d,%d, want 10,20,30,255", r, g, b, a)
	}
}

func TestWICDecodesScaled(t *testing.T) {
	data := makeTestPNG(t, 40, 20, color.RGBA{R: 200, G: 100, B: 50, A: 255})

	surf, err := decodeImageWICSize(context.Background(), "", data, 10, 10)
	if err != nil {
		t.Fatalf("scaled WIC decode failed: %v", err)
	}
	// Aspect fit inside a 10x10 box: 40x20 becomes 10x5.
	if surf.Width != 10 || surf.Height != 5 {
		t.Fatalf("scaled geometry = %dx%d, want 10x5", surf.Width, surf.Height)
	}
	r, g, b, a := surf.PixelAt(5, 2)
	if r != 200 || g != 100 || b != 50 || a != 255 {
		t.Errorf("scaled pixel = %d,%d,%d,%d", r, g, b, a)
	}
}

func TestWICDecoderClaimsItsFormats(t *testing.T) {
	for _, path := range []string{"a.jpg", "b.webp", "c.tiff", "d.ico"} {
		if !IsImageFile(path) {
			t.Errorf("%s must be viewable through WIC", path)
		}
	}
	if list := ImageDecodersFor("a.jpg"); len(list) == 0 || list[0].Name != "wic" {
		t.Fatalf("WIC must win for jpg, got %v", list)
	}
}

func TestShellDecoderClaimsMediaFormats(t *testing.T) {
	for _, path := range []string{"v.svg", "w.emf", "m.mp4", "k.mkv"} {
		if !IsImageFile(path) {
			t.Errorf("%s must be viewable through the shell", path)
		}
	}
	if list := ImageDecodersFor("v.svg"); len(list) == 0 || list[0].Name != "shell" {
		t.Fatalf("the shell must win for svg, got %v", list)
	}
}

// The shell decoder shows up in the F4 cycle under its official name (the
// Windows Shell), while the registry key stays "shell" for the config and
// the pin.
func TestShellDecoderShownWithOfficialName(t *testing.T) {
	choices := ImageDecoderChoices("clip.mp4")
	if len(choices) == 0 {
		t.Fatal("a video file must offer the shell decoder in the cycle")
	}
	if choices[0].key != "shell" || choices[0].label != "Shell" {
		t.Fatalf("shell choice = %+v, want key=shell label=Shell", choices[0])
	}
	d, ok := findDecoder("shell")
	if !ok {
		t.Fatal("the shell decoder must be registered")
	}
	if got := decoderDisplayName(d, nil); got != "Shell" {
		t.Errorf("shell decoder display name = %q, want Shell", got)
	}
}

// The shell renders a real file itself: the pipeline must be able to hand it
// the path without pulling a video's bytes into memory (PathDecoderFor).
func TestShellDecoderIsPathRendered(t *testing.T) {
	d, ok := PathDecoderFor("clip.mp4")
	if !ok || d.Name != "shell" {
		t.Fatalf("PathDecoderFor(clip.mp4) = %q %v, want shell", d.Name, ok)
	}
	// Every video container the shell claims is flagged as video, so the
	// preview surfaces skip them; the shell's non-video claims are not.
	for _, p := range shellImageExtensions {
		if IsVideoFile("f."+p) != isVideoExtension(p) {
			t.Errorf("IsVideoFile(f.%s) disagrees with the shell claim", p)
		}
	}
}

func isVideoExtension(ext string) bool {
	for _, e := range VideoFileExtensions {
		if e == ext {
			return true
		}
	}
	return false
}

func TestLocalImagePathKeepsExtension(t *testing.T) {
	name, cleanup := localImagePath("", []byte("x"))
	if name == "" {
		t.Fatal("a temp path must be produced")
	}
	cleanup()
	if got := ImageExtension(name); got != "img" {
		t.Errorf("extension = %q, want img", got)
	}

	// A nonexistent path falls back to a temp file whose extension follows
	// the requested name, so the shell picks the right handler.
	name, cleanup = localImagePath("video.mkv", []byte("x"))
	defer cleanup()
	if got := ImageExtension(name); got != "mkv" {
		t.Errorf("extension = %q, want mkv", got)
	}

	// A real file is used as-is, no copy.
	real := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, cleanup = localImagePath(real, []byte("y"))
	defer cleanup()
	if name != real {
		t.Errorf("an existing path must be used as-is, got %q", name)
	}
}

// A path decode (nil bytes) that is not a real file must be refused rather
// than staged as an empty temp file: the shell answers that with
// E_NOINTERFACE (0x80004002).
func TestLocalImagePathRefusesWithoutBytes(t *testing.T) {
	name, cleanup := localImagePath("definitely-missing-video.mkv", nil)
	defer cleanup()
	if name != "" {
		t.Fatalf("no temp file may be staged without bytes, got %q", name)
	}
}

// Shell rendering needs a real file; a missing one must fail cleanly.
func TestShellDecodeFailsWithoutAFile(t *testing.T) {
	surf, err := decodeImageShell(context.Background(), "", []byte("not a video"))
	if err == nil {
		t.Fatalf("expected an error, got %v", surf)
	}
	if surf != nil {
		t.Error("no surface may be returned on failure")
	}
}

// makeTestJPEG builds a small JPEG from pixels, the same way the standard
// encoder does.
func makeTestJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(30 + x*40), G: uint8(60 + y*60), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("cannot build the test JPEG: %v", err)
	}
	return buf.Bytes()
}

// exifOrientationJPEG wraps JPEG bytes in an APP1 Exif segment carrying the
// given orientation tag. The TIFF block mirrors a real camera's IFD0 (four
// entries, ASCII values at the end); WIC's JPEG metadata handler refuses
// one-entry stubs.
func exifOrientationJPEG(jpegData []byte, orient uint16) []byte {
	exif := make([]byte, 94)
	exif[0], exif[1] = 'I', 'I'
	binary.LittleEndian.PutUint16(exif[2:], 42)
	binary.LittleEndian.PutUint32(exif[4:], 8)
	binary.LittleEndian.PutUint16(exif[8:], 4) // four IFD0 entries
	b4 := func(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }
	set := func(off int, tag, typ uint16, count uint32, val []byte) {
		binary.LittleEndian.PutUint16(exif[off:], tag)
		binary.LittleEndian.PutUint16(exif[off+2:], typ)
		binary.LittleEndian.PutUint32(exif[off+4:], count)
		copy(exif[off+8:off+12], val)
	}
	set(10, 0x010F, 2, 6, b4(62))                        // Make
	set(22, 0x0110, 2, 6, b4(68))                        // Model
	set(34, 0x0112, 3, 1, []byte{byte(orient), 0, 0, 0}) // Orientation
	set(46, 0x0132, 2, 20, b4(74))                       // DateTime
	copy(exif[62:], "Canon")
	copy(exif[68:], "EOS R5")
	copy(exif[74:], "2026:08:13 12:00:00\x00")

	payload := append([]byte("Exif\x00\x00"), exif...)
	// The segment length counts itself: payload plus the two length bytes.
	segLen := len(payload) + 2
	out := []byte{0xFF, 0xD8, 0xFF, 0xE1, byte(segLen >> 8), byte(segLen)}
	out = append(out, payload...)
	return append(out, jpegData[2:]...)
}

func TestWICAppliesEXIFOrientation(t *testing.T) {
	data := exifOrientationJPEG(makeTestJPEG(t, 4, 2), 6)
	surf, err := decodeImageWIC(context.Background(), "a.jpg", data)
	if err != nil {
		t.Fatalf("WIC decode failed: %v", err)
	}
	// Orientation 6 (90 CW) turns 4x2 into 2x4.
	if surf.Width != 2 || surf.Height != 4 {
		t.Fatalf("geometry = %dx%d, want 2x4 (the EXIF orientation must be applied)", surf.Width, surf.Height)
	}
}

// TestWICScaledOrientation exercises the flip rotator chained after the
// scaler: a tile-sized decode of an oriented picture must still come out
// rotated, so the gallery's tiles show upright.
func TestWICScaledOrientation(t *testing.T) {
	data := exifOrientationJPEG(makeTestJPEG(t, 40, 20), 6)
	surf, err := decodeImageWICSize(context.Background(), "a.jpg", data, 10, 10)
	if err != nil {
		t.Fatalf("scaled oriented decode failed: %v", err)
	}
	// 40x20 aspect-fits to 10x5, then orientation 6 (90 CW) turns it into 5x10.
	if surf.Width != 5 || surf.Height != 10 {
		t.Fatalf("geometry = %dx%d, want 5x10", surf.Width, surf.Height)
	}
}

// TestWICFlipRotatorMatchesGoTransform locks in the native orientation path:
// for every EXIF orientation the WIC decode (which bakes the orientation into
// the copy via the flip rotator) must equal the Go-side ApplyImageOrientation
// of the same pixels, so the transform table cannot drift.
func TestWICFlipRotatorMatchesGoTransform(t *testing.T) {
	base := makeTestJPEG(t, 4, 2)
	ref, err := decodeImageWIC(context.Background(), "a.jpg", exifOrientationJPEG(base, 1))
	if err != nil {
		t.Fatalf("reference decode failed: %v", err)
	}
	for orient := 2; orient <= 8; orient++ {
		want := ApplyImageOrientation(ref, orient)
		got, err := decodeImageWIC(context.Background(), "a.jpg", exifOrientationJPEG(base, uint16(orient)))
		if err != nil {
			t.Fatalf("orient %d: decode failed: %v", orient, err)
		}
		if got.Width != want.Width || got.Height != want.Height {
			t.Fatalf("orient %d: geometry %dx%d, want %dx%d", orient, got.Width, got.Height, want.Width, want.Height)
		}
		for y := 0; y < got.Height; y++ {
			for x := 0; x < got.Width; x++ {
				r1, g1, b1, a1 := got.PixelAt(x, y)
				r2, g2, b2, a2 := want.PixelAt(x, y)
				if r1 != r2 || g1 != g2 || b1 != b2 || a1 != a2 {
					t.Fatalf("orient %d: pixel %d,%d = %d,%d,%d,%d, want %d,%d,%d,%d",
						orient, x, y, r1, g1, b1, a1, r2, g2, b2, a2)
				}
			}
		}
	}
}

// TestWICReadsOrientationTag checks the metadata query itself, so a broken
// vt comparison or an APP1 layout WIC cannot parse shows up here first.
func TestWICReadsOrientationTag(t *testing.T) {
	data := exifOrientationJPEG(makeTestJPEG(t, 4, 2), 6)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initDecodeCOM(); err != nil {
		t.Fatal(err)
	}
	defer windows.CoUninitialize()
	stream, decoder, err := wicOpenDecoder(data)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer wicRelease(stream)
	defer wicRelease(decoder)
	var frame uintptr
	if err := wicCall(decoder, wicDecoderGetFrame, 0, uintptr(unsafe.Pointer(&frame))); err != nil {
		t.Fatalf("frame: %v", err)
	}
	defer wicRelease(frame)
	var meta uintptr
	if err := wicCall(frame, wicFrameGetMetadataQueryReader, uintptr(unsafe.Pointer(&meta))); err != nil {
		t.Fatalf("metadata reader: %v", err)
	}
	defer wicRelease(meta)
	wp, _ := syscall.UTF16PtrFromString("/app1/ifd/{ushort=274}")
	var pv propVariant
	if err := wicCall(meta, wicMetadataGetMetadataByName, uintptr(unsafe.Pointer(wp)), uintptr(unsafe.Pointer(&pv))); err != nil {
		t.Fatalf("GetMetadataByName: %#x", uint32(err.(syscall.Errno)))
	}
	vt, val := pv.vt, int(pv.val[0])|int(pv.val[1])<<8
	procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))
	if vt != vtUI2 || val != 6 {
		t.Fatalf("orientation = vt=%#x val=%d, want vt=0x12 val=6", vt, val)
	}
}

func TestWICDecodesWithProgress(t *testing.T) {
	data := makeTestPNG(t, 40, 20, color.RGBA{R: 200, G: 100, B: 50, A: 255})
	var pct atomic.Int32
	ctx := WithDecodeProgress(context.Background(), &pct)
	surf, err := decodeImageWIC(ctx, "a.png", data)
	if err != nil {
		t.Fatalf("WIC decode failed: %v", err)
	}
	if surf.Width != 40 || surf.Height != 20 {
		t.Fatalf("geometry = %dx%d, want 40x20", surf.Width, surf.Height)
	}
	if got := pct.Load(); got != 100 {
		t.Errorf("progress = %d%%, want 100%%", got)
	}
}

func TestMergeWICExtensionsKeepsInProcessFormats(t *testing.T) {
	merged := mergeWICExtensions([]string{"png", "jpeg", "cr2", "nef", "BMP", "raw", ".arw", "cr2", "qoi"})
	for _, e := range []string{"cr2", "nef", "raw", "arw"} {
		if !containsString(merged, e) {
			t.Errorf("merged list must contain %q, got %v", e, merged)
		}
	}
	// The common formats are in the built-in list, so the merge keeps them
	// even though in-process decoders also claim them.
	for _, e := range []string{"png", "bmp", "jpeg"} {
		if !containsString(merged, e) {
			t.Errorf("the common WIC format %q must survive the merge, got %v", e, merged)
		}
	}
	// qoi has no WIC codec: it stays with its in-process decoder.
	if containsString(merged, "qoi") {
		t.Errorf("qoi must stay with its in-process decoder, got %q in %v", "qoi", merged)
	}
	// Duplicates are gone: .arw and cr2 were repeated.
	if !containsString(merged, "arw") {
		t.Errorf("enumerated extensions must be normalized, got %v", merged)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
