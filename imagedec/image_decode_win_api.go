//go:build windows

package imagedec

// Native Windows decoders: WIC for pixel formats, the shell thumbnailer for
// what WIC cannot read (SVG, EMF/WMF, video). WIC decodes from memory; the
// shell renders a real file, so a virtual path is staged to a temp file.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/unxed/vtui"
	"golang.org/x/sys/windows"
)

var (
	modOle32   = windows.NewLazySystemDLL("ole32.dll")
	modShlwapi = windows.NewLazySystemDLL("shlwapi.dll")
	modShell32 = windows.NewLazySystemDLL("shell32.dll")
	modGdi32   = windows.NewLazySystemDLL("gdi32.dll")

	procCoCreateInstance  = modOle32.NewProc("CoCreateInstance")
	procSHCreateMemStream = modShlwapi.NewProc("SHCreateMemStream")

	procSHCreateItemFromParsingName = modShell32.NewProc("SHCreateItemFromParsingName")
	procGetObjectW                  = modGdi32.NewProc("GetObjectW")
	procGetDIBits                   = modGdi32.NewProc("GetDIBits")
	procCreateCompatibleDC          = modGdi32.NewProc("CreateCompatibleDC")
	procDeleteDC                    = modGdi32.NewProc("DeleteDC")
	procDeleteObject                = modGdi32.NewProc("DeleteObject")

	procPropVariantClear = modOle32.NewProc("PropVariantClear")
)

var (
	clsidWICImagingFactory2 = windows.GUID{
		Data1: 0x317d06e8, Data2: 0x5f24, Data3: 0x433d,
		Data4: [8]byte{0xbd, 0xf7, 0x79, 0xce, 0x68, 0xd8, 0xab, 0xc2},
	}
	iidWICImagingFactory2 = windows.GUID{
		Data1: 0x7b816b45, Data2: 0x1996, Data3: 0x4476,
		Data4: [8]byte{0xb1, 0x32, 0xde, 0x9e, 0x24, 0x7c, 0x8a, 0xf0},
	}
	guidWICPixelFormat32bppRGBA = windows.GUID{
		Data1: 0xf5c72304, Data2: 0x4e03, Data3: 0x4bfe,
		Data4: [8]byte{0xb1, 0x85, 0x3d, 0x77, 0x76, 0x8d, 0xc9, 0x0f},
	}
	guidWICPixelFormat32bppBGRA = windows.GUID{
		Data1: 0x6fddc324, Data2: 0x4e03, Data3: 0x4bfe,
		Data4: [8]byte{0xb1, 0x85, 0x3d, 0x77, 0x76, 0x8d, 0xc9, 0x0f},
	}
	// Premultiplied variants of the straight RGBA/BGRA formats. The converter
	// does not un-premultiply, so these are copied verbatim and straightened
	// in Go (see unpremultiplyRGBA); the last byte tells them apart from the
	// straight formats (0x0e/0x10 vs 0x0f).
	guidWICPixelFormat32bppPRGBA = windows.GUID{
		Data1: 0xf5c72304, Data2: 0x4e03, Data3: 0x4bfe,
		Data4: [8]byte{0xb1, 0x85, 0x3d, 0x77, 0x76, 0x8d, 0xc9, 0x0e},
	}
	guidWICPixelFormat32bppPBGRA = windows.GUID{
		Data1: 0x6fddc324, Data2: 0x4e03, Data3: 0x4bfe,
		Data4: [8]byte{0xb1, 0x85, 0x3d, 0x77, 0x76, 0x8d, 0xc9, 0x10},
	}
	iidShellItemImageFactory = windows.GUID{
		Data1: 0xbcc18b79, Data2: 0xba16, Data3: 0x442f,
		Data4: [8]byte{0x80, 0xc4, 0x8a, 0x59, 0xc3, 0x0c, 0x46, 0x3b},
	}

	// The progress notification is how a WIC decode is aborted mid-copy.
	iidBitmapCodecProgress = windows.GUID{
		Data1: 0x64c1024e, Data2: 0xc3cf, Data3: 0x4462,
		Data4: [8]byte{0x80, 0x78, 0x88, 0xc2, 0xb1, 0x1c, 0x46, 0xd9},
	}

	// Codec info is needed to enumerate installed formats at startup.
	iidBitmapDecoderInfo = windows.GUID{
		Data1: 0xd006a703, Data2: 0x92a7, Data3: 0x47e5,
		Data4: [8]byte{0xbd, 0x9a, 0x5f, 0x81, 0xd4, 0xe6, 0x9b, 0x48},
	}
)

// COM interfaces are called through their vtable, indexed in IDL order after
// IUnknown. Offsets are stable for the documented interfaces used here.
const (
	wicVtblRelease = 2

	wicFactoryCreateDecoderFromStream = 4
	wicFactoryCreateFormatConverter   = 10
	wicFactoryCreateBitmapScaler      = 11
	wicFactoryCreateBitmapFlipRotator = 13

	wicDecoderGetFrame = 13

	wicSourceGetSize        = 3
	wicSourceGetPixelFormat = 4
	wicSourceCopyPixels     = 7

	wicConverterInitialize   = 8
	wicScalerInitialize      = 8
	wicFlipRotatorInitialize = 8

	wicDecodeMetadataCacheOnDemand = 0
	wicDecodeMetadataCacheOnLoad   = 1

	// The factory enumerates the codecs installed on the machine.
	wicFactoryCreateComponentEnumerator = 21
	wicEnumUnknownNext                  = 3
	wicCodecGetFileExtensions           = 17

	// A frame carries the embedded thumbnail and the Exif metadata.
	wicFrameGetThumbnail           = 10
	wicFrameGetMetadataQueryReader = 8
	wicMetadataGetMetadataByName   = 5

	// RegisterProgressNotification sits right after IUnknown.
	wicProgressRegisterProgressNotification = 3

	wicComponentTypeDecoder = 1

	// VT_UI2 (0x0012) is the type of the Exif orientation tag; VT_UI4 is
	// 0x0013, one past it, so a typo here silently disables orientation.
	vtUI2 = 0x0012

	// winCodecErrAborted (E_ABORT) is what a progress callback returns to
	// stop the codec; WINCODEC_ERR_ABORTED is the same value.
	winCodecErrAborted = 0x80004004

	// WICBitmapTransformOptions: how the flip rotator bakes EXIF orientation
	// into a copy. Rotate* are clockwise quarter turns; the flips mirror.
	wicTransformRotate0        = 0x0
	wicTransformRotate90       = 0x1
	wicTransformRotate180      = 0x2
	wicTransformRotate270      = 0x3
	wicTransformFlipHorizontal = 0x8
	wicTransformFlipVertical   = 0x10

	shellVtblGetImage = 3
	shellFlagsGen     = 0x09
	shellPriority     = 1000
)

type shellSize struct{ CX, CY int32 }

type shellBitmap struct {
	Type       int32
	Width      int32
	Height     int32
	WidthBytes int32
	Planes     uint16
	BitsPixel  uint16
	Bits       uintptr
}

type shellBitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type shellBitmapInfo struct {
	Header shellBitmapInfoHeader
	Colors [3]uint32
}

var shellBuckets = [...]int{256, 768, 1024, 1600, 2048}

func wicCall(obj uintptr, index int, args ...uintptr) error {
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(index)*unsafe.Sizeof(vtbl)))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{obj}, args...)...)
	if int32(r) >= 0 {
		return nil
	}
	return syscall.Errno(int32(r))
}

func wicRelease(obj uintptr) {
	if obj != 0 {
		wicCall(obj, wicVtblRelease)
	}
}

func initDecodeCOM() error {
	err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED)
	if err != nil {
		if e, ok := err.(syscall.Errno); ok && e == syscall.Errno(windows.RPC_E_CHANGED_MODE) {
			err = windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED)
		}
	}
	return err
}

func wicCreateFactory() (uintptr, error) {
	var factory uintptr
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidWICImagingFactory2)),
		0,
		1,
		uintptr(unsafe.Pointer(&iidWICImagingFactory2)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if int32(r) < 0 {
		return 0, syscall.Errno(int32(r))
	}
	if factory == 0 {
		return 0, fmt.Errorf("CoCreateInstance returned a null factory")
	}
	return factory, nil
}

// The factory lives on a pinned thread that keeps its COM apartment alive for
// the process; a factory created on a transient decode thread would die with
// its CoUninitialize and be recreated on every picture.
var (
	comKeepAliveOnce sync.Once
	wicCachedFactory uintptr
)

func ensureComKeepAlive() {
	comKeepAliveOnce.Do(func() {
		done := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			if err := initDecodeCOM(); err == nil {
				wicCachedFactory, _ = wicCreateFactory()
			}
			close(done)
			if wicCachedFactory == 0 {
				return
			}
			select {}
		}()
		<-done
	})
}

func wicFactory() uintptr {
	ensureComKeepAlive()
	return wicCachedFactory
}

func createWICScaler(factory, source uintptr, tw, th uint32) (uintptr, error) {
	var scaler uintptr
	if err := wicCall(factory, wicFactoryCreateBitmapScaler, uintptr(unsafe.Pointer(&scaler))); err != nil || scaler == 0 {
		if err == nil {
			err = fmt.Errorf("CreateBitmapScaler returned null")
		}
		return 0, err
	}
	// WICBitmapInterpolationModeFant = 3.
	if err := wicCall(scaler, wicScalerInitialize, source, uintptr(tw), uintptr(th), 3); err != nil {
		wicRelease(scaler)
		return 0, err
	}
	return scaler, nil
}

// wicOrientationTransform maps an EXIF orientation tag (2-8) to the
// WICBitmapTransformOptions the flip rotator needs. 1 and unknown map to no
// transform.
func wicOrientationTransform(orient int) int {
	switch orient {
	case 2:
		return wicTransformFlipHorizontal
	case 3:
		return wicTransformRotate180
	case 4:
		return wicTransformFlipVertical
	case 5:
		return wicTransformRotate270 | wicTransformFlipHorizontal
	case 6:
		return wicTransformRotate90
	case 7:
		return wicTransformRotate90 | wicTransformFlipHorizontal
	case 8:
		return wicTransformRotate270
	default:
		return wicTransformRotate0
	}
}

// createWICFlipRotator wraps src in a flip rotator that applies the EXIF
// orientation during the copy, so no Go-side pixel pass runs. It returns 0
// when the rotator cannot be created or initialized; the caller falls back
// to the Go-side ApplyImageOrientation pass.
func createWICFlipRotator(factory, src uintptr, orient int) uintptr {
	if orient < 2 || orient > 8 || src == 0 {
		return 0
	}
	var rotator uintptr
	if err := wicCall(factory, wicFactoryCreateBitmapFlipRotator, uintptr(unsafe.Pointer(&rotator))); err != nil {
		if rotator != 0 {
			wicRelease(rotator)
		}
		return 0
	}
	if rotator == 0 {
		return 0
	}
	if err := wicCall(rotator, wicFlipRotatorInitialize, src, uintptr(wicOrientationTransform(orient))); err != nil {
		wicRelease(rotator)
		return 0
	}
	return rotator
}

// wicAspectFit keeps the aspect ratio inside the box: the scaler distorts if
// asked for an arbitrary size. Integer division could round a side to zero on
// extreme aspect ratios, so both are floored at one.
func wicAspectFit(w, h, tw, th int) (int, int) {
	if w <= tw && h <= th {
		return w, h
	}
	if tw*h <= th*w {
		if fw, fh := tw, h*tw/w; fh > 0 {
			return fw, fh
		}
		return tw, 1
	}
	if fw, fh := w*th/h, th; fw > 0 {
		return fw, fh
	}
	return 1, th
}

// swapRB reorders BGRA into the RGBA order the surfaces hold, one uint32 at
// a time: the byte swap of the R and B slots leaves G and A untouched.
func swapRB(p []byte) {
	for i := 0; i+3 < len(p); i += 4 {
		u := *(*uint32)(unsafe.Pointer(&p[i]))
		u = (u & 0xFF00FF00) | ((u & 0xFF) << 16) | ((u >> 16) & 0xFF)
		*(*uint32)(unsafe.Pointer(&p[i])) = u
	}
}

// unpremultiplyRGBA straightens premultiplied alpha in place (RGBA byte
// order, which the copy holds after swapRB). The WIC format converter does
// not un-premultiply, so 32bppPBGRA/PRGBA would otherwise blend too dark.
func unpremultiplyRGBA(p []byte) {
	for i := 0; i+3 < len(p); i += 4 {
		a := uint32(p[i+3])
		if a == 0 {
			p[i], p[i+1], p[i+2] = 0, 0, 0
			continue
		}
		if a == 255 {
			continue
		}
		p[i] = byte((uint32(p[i])*255 + a/2) / a)
		p[i+1] = byte((uint32(p[i+1])*255 + a/2) / a)
		p[i+2] = byte((uint32(p[i+2])*255 + a/2) / a)
	}
}

// wicRect is the source rectangle CopyPixels can be limited to; copying in
// scanline bands is what lets a long decode report a percentage.
type wicRect struct {
	X, Y, Width, Height int32
}

// propVariant is the PROPVARIANT WIC fills in for a metadata query; the
// union is 16 bytes because DECIMAL is its largest member.
type propVariant struct {
	vt         uint16
	r1, r2, r3 uint16
	val        [16]byte
}

// wicMemStream builds an IStream over the bytes in memory.
func wicMemStream(data []byte) uintptr {
	stream, _, _ := procSHCreateMemStream.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	return stream
}

// wicOpenDecoder creates a decoder over the bytes in memory. On failure the
// stream is released here: the caller sees only the error, never a dangling
// IStream to clean up.
func wicOpenDecoder(data []byte) (stream, decoder uintptr, err error) {
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("empty image data")
	}
	factory := wicFactory()
	if factory == 0 {
		return 0, 0, fmt.Errorf("CoCreateInstance failed")
	}
	stream = wicMemStream(data)
	if stream == 0 {
		return 0, 0, fmt.Errorf("SHCreateMemStream failed")
	}
	err = wicCall(factory, wicFactoryCreateDecoderFromStream, stream, 0, wicDecodeMetadataCacheOnDemand, uintptr(unsafe.Pointer(&decoder)))
	if err != nil {
		// Some codecs refuse the lazy metadata mode; retry with eager cache on
		// a fresh stream, since the failed attempt may have moved the cursor.
		wicRelease(stream)
		stream = wicMemStream(data)
		if stream == 0 {
			return 0, 0, fmt.Errorf("SHCreateMemStream failed")
		}
		err = wicCall(factory, wicFactoryCreateDecoderFromStream, stream, 0, wicDecodeMetadataCacheOnLoad, uintptr(unsafe.Pointer(&decoder)))
	}
	if err != nil {
		wicRelease(stream)
		return 0, 0, fmt.Errorf("CreateDecoderFromStream: %w", err)
	}
	if decoder == 0 {
		wicRelease(stream)
		return 0, 0, fmt.Errorf("CreateDecoderFromStream returned a null decoder")
	}
	return stream, decoder, nil
}

// wicSourceRGBA yields a 32bppRGBA source: the source itself when already
// RGBA, else a converter to BGRA (WIC's converter refuses 32bppRGBA as a
// target, so the copy is reordered afterwards). swap says the channels need
// reordering; alpha says the picture carries alpha; premul says the alpha is
// premultiplied and must be straightened after the copy. The caller must run
// the returned release func, which frees a converter.
func wicSourceRGBA(factory, src uintptr, srcFmt windows.GUID) (uintptr, bool, bool, bool, func(), error) {
	switch srcFmt {
	case guidWICPixelFormat32bppRGBA:
		return src, false, true, false, func() {}, nil
	case guidWICPixelFormat32bppBGRA:
		return src, true, true, false, func() {}, nil
	case guidWICPixelFormat32bppPRGBA:
		return src, false, true, true, func() {}, nil
	case guidWICPixelFormat32bppPBGRA:
		return src, true, true, true, func() {}, nil
	}
	var converter uintptr
	if err := wicCall(factory, wicFactoryCreateFormatConverter, uintptr(unsafe.Pointer(&converter))); err != nil {
		return 0, false, false, false, nil, fmt.Errorf("CreateFormatConverter: %w", err)
	}
	if converter == 0 {
		return 0, false, false, false, nil, fmt.Errorf("CreateFormatConverter returned a null converter")
	}
	if err := wicCall(converter, wicConverterInitialize, src, uintptr(unsafe.Pointer(&guidWICPixelFormat32bppBGRA)), 0, 0, 0, 0); err != nil {
		wicRelease(converter)
		return 0, false, false, false, nil, fmt.Errorf("converter Initialize: %w", err)
	}
	return converter, true, false, false, func() { wicRelease(converter) }, nil
}

// wicCopyPixels copies the source into surf in one call, or in scanline bands
// when a progress sink is watching, so the toast can report a long decode and
// the context can stop between copies.
func wicCopyPixels(ctx context.Context, src uintptr, w, h int, surf *vtui.ImageSurface, pct *atomic.Int32) error {
	stride := uintptr(w) * 4
	if pct == nil {
		return wicCall(src, wicSourceCopyPixels, 0, stride, uintptr(len(surf.Pix)), uintptr(unsafe.Pointer(&surf.Pix[0])))
	}
	const bands = 20
	band := (h + bands - 1) / bands
	if band < 1 {
		band = 1
	}
	for y := 0; y < h; y += band {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := band
		if y+n > h {
			n = h - y
		}
		rect := wicRect{X: 0, Y: int32(y), Width: int32(w), Height: int32(n)}
		err := wicCall(src, wicSourceCopyPixels, uintptr(unsafe.Pointer(&rect)), stride, uintptr(n)*stride, uintptr(unsafe.Pointer(&surf.Pix[y*int(stride)])))
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isAbortError(err) {
				return ctxErr
			}
			return err
		}
		pct.Store(int32((y + n) * 100 / h))
	}
	return nil
}

// wicFrameThumbnail returns the embedded thumbnail when it fits the frame's
// aspect and is not much larger than the tile; a cropped camera thumbnail
// would distort the tile, so it is rejected and the scaler runs instead.
func wicFrameThumbnail(frame uintptr, w, h, tw, th int) (uintptr, bool) {
	var thumb uintptr
	if err := wicCall(frame, wicFrameGetThumbnail, uintptr(unsafe.Pointer(&thumb))); err != nil || thumb == 0 {
		return 0, false
	}
	var tw2, th2 uint32
	if err := wicCall(thumb, wicSourceGetSize, uintptr(unsafe.Pointer(&tw2)), uintptr(unsafe.Pointer(&th2))); err != nil || tw2 == 0 || th2 == 0 {
		wicRelease(thumb)
		return 0, false
	}
	// Aspect fit by cross multiplication; ~1.5% is the tolerance.
	prod := int64(tw2) * int64(h)
	prod2 := int64(th2) * int64(w)
	d := prod - prod2
	if d < 0 {
		d = -d
	}
	if d > prod/64 {
		wicRelease(thumb)
		return 0, false
	}
	if int(tw2) > tw*4 || int(th2) > th*4 {
		wicRelease(thumb)
		return 0, false
	}
	return thumb, true
}

// wicFrameOrientation reads the EXIF orientation tag; WIC does not apply it,
// so the decoder must. JPEG keeps its Exif under /app1, TIFF-based containers
// under /ifd.
func wicFrameOrientation(frame uintptr) int {
	var meta uintptr
	if err := wicCall(frame, wicFrameGetMetadataQueryReader, uintptr(unsafe.Pointer(&meta))); err != nil || meta == 0 {
		return 0
	}
	defer wicRelease(meta)
	for _, path := range [...]string{"/app1/ifd/{ushort=274}", "/ifd/{ushort=274}"} {
		wp, err := syscall.UTF16PtrFromString(path)
		if err != nil {
			continue
		}
		var pv propVariant
		if err := wicCall(meta, wicMetadataGetMetadataByName, uintptr(unsafe.Pointer(wp)), uintptr(unsafe.Pointer(&pv))); err != nil {
			continue
		}
		// PropVariantClear zeroes the variant, so read vt and value first.
		vt, o := pv.vt, int(pv.val[0])|int(pv.val[1])<<8
		procPropVariantClear.Call(uintptr(unsafe.Pointer(&pv)))
		if vt != vtUI2 {
			continue
		}
		if o >= 2 && o <= 8 {
			return o
		}
	}
	return 0
}

// decodeSingleFrame decodes frame 0 of a WIC picture. A positive box asks
// for an aspect-fit copy — or the embedded thumbnail when it fits — so a
// tile never decodes the whole picture; the EXIF orientation is baked in.
func decodeSingleFrame(ctx context.Context, factory, decoder uintptr, tw, th int) (*vtui.ImageSurface, error) {
	if decoder == 0 {
		return nil, fmt.Errorf("no decoder")
	}
	var frame uintptr
	if err := wicCall(decoder, wicDecoderGetFrame, 0, uintptr(unsafe.Pointer(&frame))); err != nil {
		return nil, fmt.Errorf("GetFrame: %w", err)
	}
	if frame == 0 {
		return nil, fmt.Errorf("GetFrame returned a null frame")
	}
	defer wicRelease(frame)

	var srcFmt windows.GUID
	if err := wicCall(frame, wicSourceGetPixelFormat, uintptr(unsafe.Pointer(&srcFmt))); err != nil {
		return nil, fmt.Errorf("GetPixelFormat: %w", err)
	}
	var w, h uint32
	if err := wicCall(frame, wicSourceGetSize, uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&h))); err != nil {
		return nil, fmt.Errorf("GetSize: %w", err)
	}

	// The EXIF orientation is baked into the copy by a flip rotator below;
	// read it once up front so the fallback pass can reuse it.
	orient := wicFrameOrientation(frame)

	src := frame
	if tw > 0 && th > 0 && (int(w) > tw || int(h) > th) {
		if thumb, ok := wicFrameThumbnail(frame, int(w), int(h), tw, th); ok {
			src = thumb
			defer wicRelease(thumb)
			_ = wicCall(src, wicSourceGetPixelFormat, uintptr(unsafe.Pointer(&srcFmt)))
			_ = wicCall(src, wicSourceGetSize, uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&h)))
		}
		if int(w) > tw || int(h) > th {
			fw, fh := wicAspectFit(int(w), int(h), tw, th)
			if scaler, err := createWICScaler(factory, src, uint32(fw), uint32(fh)); err == nil {
				src = scaler
				w, h = uint32(fw), uint32(fh)
				defer wicRelease(scaler)
				_ = wicCall(src, wicSourceGetPixelFormat, uintptr(unsafe.Pointer(&srcFmt)))
			}
		}
	}
	if w == 0 || h == 0 || uint64(w)*uint64(h) > imageMaxPixels {
		return nil, fmt.Errorf("unsupported geometry %dx%d", w, h)
	}

	// Bake the orientation into the copy with a flip rotator, so the decoder
	// never re-walks the frame in Go. The rotator reports the rotated
	// geometry, so re-read it (orientations 5-8 swap the sides).
	rotated := false
	if rot := createWICFlipRotator(factory, src, orient); rot != 0 {
		src = rot
		defer wicRelease(rot)
		rotated = true
		_ = wicCall(src, wicSourceGetSize, uintptr(unsafe.Pointer(&w)), uintptr(unsafe.Pointer(&h)))
		if w == 0 || h == 0 || uint64(w)*uint64(h) > imageMaxPixels {
			return nil, fmt.Errorf("unsupported geometry %dx%d", w, h)
		}
	}

	src, swap, alpha, premul, release, err := wicSourceRGBA(factory, src, srcFmt)
	if err != nil {
		return nil, err
	}
	defer release()

	surf := vtui.NewImageSurface(int(w), int(h))
	if surf == nil {
		return nil, fmt.Errorf("unsupported geometry %dx%d", w, h)
	}
	if err := wicCopyPixels(ctx, src, int(w), int(h), surf, decodeProgressFrom(ctx)); err != nil {
		return nil, err
	}
	if swap {
		swapRB(surf.Pix)
	}
	if premul {
		unpremultiplyRGBA(surf.Pix)
	}
	surf.Opaque = !alpha

	// A codec without a flip rotator (or one whose Initialize failed) falls
	// back to the Go-side pass.
	if !rotated && orient > 1 {
		surf = ApplyImageOrientation(surf, orient)
	}
	return surf, nil
}

// wicDecode runs a WIC decode from memory on a COM-initialized thread.
func wicDecode(ctx context.Context, data []byte, tw, th int) (*vtui.ImageSurface, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("there is nothing to decode")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initDecodeCOM(); err != nil {
		return nil, fmt.Errorf("CoInitializeEx: %w", err)
	}
	defer windows.CoUninitialize()

	// Some codecs (JPEG XL notably) answer the first GetFrame of a process
	// with a null frame; one retry with a fresh decoder covers that.
	factory := wicFactory()
	for attempt := 0; ; attempt++ {
		stream, decoder, err := wicOpenDecoder(data)
		if err != nil {
			return nil, err
		}
		cancel := registerWICProgressCancel(decoder, ctx)
		surf, derr := decodeSingleFrame(ctx, factory, decoder, tw, th)
		cancel()
		wicRelease(decoder)
		wicRelease(stream)
		if derr == nil || attempt > 0 || !isNullFrameError(derr) {
			return surf, derr
		}
	}
}

// registerWICProgressCancel turns a finished context into an abort via the
// codec's progress notification. Best-effort: codecs without one never call.
func registerWICProgressCancel(decoder uintptr, ctx context.Context) func() {
	noop := func() {}
	if decoder == 0 || ctx == nil {
		return noop
	}
	var prog uintptr
	if err := wicCall(decoder, 0, uintptr(unsafe.Pointer(&iidBitmapCodecProgress)), uintptr(unsafe.Pointer(&prog))); err != nil || prog == 0 {
		return noop
	}
	state := &wicProgressState{ctx: ctx}
	// WICProgressOperationAll | WICProgressNotificationAll: report every copy
	// step, begin and end included.
	if err := wicCall(prog, wicProgressRegisterProgressNotification, wicProgressAbortFn, uintptr(unsafe.Pointer(state)), 0x3FFFF); err != nil {
		wicRelease(prog)
		return noop
	}
	return func() {
		defer runtime.KeepAlive(state)
		wicRelease(prog)
	}
}

// wicProgressState is the per-decode payload the codec hands back to the
// callback; it lives until the registration is released, i.e. for the whole
// synchronous decode.
type wicProgressState struct {
	ctx context.Context
}

// wicProgressAbortFn is the stdcall callback. The double progress argument is
// not readable through Go's callback ABI (it rides an XMM register), but
// cancellation only needs the context.
var wicProgressAbortFn = syscall.NewCallback(wicProgressAbort)

func wicProgressAbort(pv, frame, op, progress uintptr) uintptr {
	st := (*wicProgressState)(unsafe.Pointer(pv))
	if st != nil && st.ctx != nil && st.ctx.Err() != nil {
		return winCodecErrAborted
	}
	return 0 // S_OK
}

// WIC facility errors read better than hex.
var wicErrorText = map[uint32]string{
	0x88982f04: "wrong state",
	0x88982f05: "value out of range",
	0x88982f07: "unknown image format",
	0x88982f0b: "unsupported version",
	0x88982f0c: "not initialized",
	0x88982f0d: "already locked",
	0x88982f40: "property not found",
	0x88982f41: "property not supported",
	0x88982f42: "property size",
	0x88982f43: "codec present",
	0x88982f44: "codec has no thumbnail",
	0x88982f45: "palette unavailable",
	0x88982f46: "codec has too many scan lines",
	0x88982f48: "internal error",
	0x88982f49: "source rectangle does not match dimensions",
	0x88982f50: "component not found",
	0x88982f51: "image size out of range",
	0x88982f52: "too much metadata",
	0x88982f60: "bad image",
	0x88982f61: "bad header",
	0x88982f62: "frame missing",
	0x88982f63: "bad metadata header",
	0x88982f70: "bad stream data",
	0x88982f71: "stream write",
	0x88982f72: "stream read",
	0x88982f73: "stream not available",
	0x88982f80: "unsupported pixel format",
	0x88982f81: "unsupported operation",
	0x88982f8a: "invalid registration",
	0x88982f8b: "component initialization failure",
	0x88982f8c: "insufficient buffer",
	0x88982f8d: "duplicate metadata",
	0x88982f8e: "property has an unexpected type",
	0x88982f8f: "unexpected size",
	0x88982f90: "invalid query request",
	0x88982f91: "unexpected metadata type",
	0x88982f92: "request only valid at metadata root",
	0x88982f93: "invalid query character",
	0x80004004: "decode aborted",
}

// describeWICError appends the readable text of a WIC failure to the raw
// error, so the viewer's title says what the hex means.
func describeWICError(err error) error {
	var e syscall.Errno
	if errors.As(err, &e) {
		if text, ok := wicErrorText[uint32(e)]; ok {
			return fmt.Errorf("%w (%s)", err, text)
		}
	}
	return err
}

func isAbortError(err error) bool {
	var e syscall.Errno
	return errors.As(err, &e) && uint32(e) == winCodecErrAborted
}

// isNullFrameError tells whether GetFrame answered with a null frame, the
// cold-start quirk of some WIC codecs worth one retry.
func isNullFrameError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "null frame")
}

func decodeImageWIC(ctx context.Context, path string, data []byte) (*vtui.ImageSurface, error) {
	return decodeImageWICSize(ctx, path, data, 0, 0)
}

// decodeImageWICSize serves gallery tiles; see decodeSingleFrame.
func decodeImageWICSize(ctx context.Context, path string, data []byte, w, h int) (*vtui.ImageSurface, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	surf, err := wicDecode(ctx, data, w, h)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, describeWICError(err)
	}
	return surf, nil
}

// localImagePath is a real on-disk path the shell can render: the path when
// it exists, otherwise a temp file with the bytes.
func localImagePath(path string, data []byte) (string, func()) {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path, func() {}
	}
	// No bytes to stage: refuse rather than stage an empty file the shell
	// answers with E_NOINTERFACE.
	if len(data) == 0 {
		return "", func() {}
	}
	ext := ImageExtension(path)
	if ext == "" {
		ext = "img"
	}
	f, err := os.CreateTemp("", "f4img-*."+ext)
	if err != nil {
		return "", func() {}
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", func() {}
	}
	f.Close()
	return name, func() { os.Remove(name) }
}

func shellBucketFor(w int) int {
	if w < 256 {
		return 256
	}
	for _, b := range shellBuckets {
		if w <= b {
			return b
		}
	}
	return 2048
}

// shellTargetWidth asks the shell for a thumbnail the screen can use, rather
// than a fixed 256px one. fallbackCellW mirrors the viewer's cell-width
// fallback (main cannot be imported from here).
const fallbackCellW = 8

func shellTargetWidth() int {
	scr := vtui.FrameManager.Screen()
	if scr == nil {
		return 1024
	}
	cw := fallbackCellW
	if g := scr.Graphics(); g != nil {
		if c, _ := g.CellSize(); c > 0 {
			cw = c
		}
	}
	if w := scr.Width() * cw; w > 0 {
		return shellBucketFor(w)
	}
	return 1024
}

func decodeImageShell(ctx context.Context, path string, data []byte) (*vtui.ImageSurface, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := initDecodeCOM(); err != nil {
		return nil, fmt.Errorf("CoInitializeEx: %w", err)
	}
	defer windows.CoUninitialize()

	local, cleanup := localImagePath(path, data)
	if local == "" {
		return nil, fmt.Errorf("no local file for the shell renderer")
	}
	defer cleanup()

	return shellDecodeThumbnail(local)
}

func shellDecodeThumbnail(local string) (*vtui.ImageSurface, error) {
	wpath, err := syscall.UTF16PtrFromString(local)
	if err != nil {
		return nil, err
	}

	var fac uintptr
	r, _, _ := procSHCreateItemFromParsingName.Call(
		uintptr(unsafe.Pointer(wpath)),
		0,
		uintptr(unsafe.Pointer(&iidShellItemImageFactory)),
		uintptr(unsafe.Pointer(&fac)),
	)
	if int32(r) < 0 || fac == 0 {
		return nil, fmt.Errorf("SHCreateItemFromParsingName failed: %#x", uint32(int32(r)))
	}
	defer wicRelease(fac)

	req := shellTargetWidth()
	sz := shellSize{CX: int32(req), CY: int32(req)}
	var hbm uintptr
	if err := wicCall(fac, shellVtblGetImage, uintptr(unsafe.Pointer(&sz)), shellFlagsGen, uintptr(unsafe.Pointer(&hbm))); err != nil {
		return nil, fmt.Errorf("GetImage failed: %w", err)
	}
	if hbm == 0 {
		return nil, fmt.Errorf("GetImage returned a null bitmap")
	}
	defer procDeleteObject.Call(hbm)

	var bm shellBitmap
	if r, _, _ := procGetObjectW.Call(hbm, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil, fmt.Errorf("GetObjectW failed")
	}
	w, h := int(bm.Width), int(bm.Height)
	if w <= 0 || h <= 0 || uint64(w)*uint64(h) > imageMaxPixels {
		return nil, fmt.Errorf("unsupported geometry %dx%d", w, h)
	}

	// A negative header height marks a top-down DIB, so GetDIBits copies
	// the bitmap into memory in the order the surface expects.
	bi := shellBitmapInfo{Header: shellBitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(shellBitmapInfoHeader{})),
		Width:       int32(w),
		Height:      -int32(h),
		Planes:      1,
		BitCount:    32,
		Compression: 0,
	}}
	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(hdc)

	surf := vtui.NewImageSurface(w, h)
	if surf == nil {
		return nil, fmt.Errorf("unsupported geometry %dx%d", w, h)
	}
	n, _, _ := procGetDIBits.Call(hdc, hbm, 0, uintptr(h), uintptr(unsafe.Pointer(&surf.Pix[0])), uintptr(unsafe.Pointer(&bi)), 0)
	if int32(n) <= 0 {
		return nil, fmt.Errorf("GetDIBits failed")
	}
	swapRB(surf.Pix)
	return surf, nil
}

// WIC's formats: natively read plus what Windows 10/11 ships codecs for. The
// common ones are listed outright so they decode when the codec enumeration
// cannot run; qoi/netpbm stay with their in-process decoders.
var wicImageExtensions = []string{
	"jpg", "jpeg", "jpe", "jfif", "gif", "png",
	"bmp", "dib",
	"tif", "tiff", "ico", "cur",
	"webp", "jxl", "heic", "heif", "hif", "avif",
	"jp2", "j2k", "wdp", "hdp",
}

// What the shell renders beyond WIC (SVG, EMF/WMF) plus the video containers,
// shared with VideoFileExtensions so the two lists never drift apart.
var shellImageExtensions = append([]string{"svg", "svgz", "emf", "wmf"}, VideoFileExtensions...)

func init() {
	RegisterImageDecoder(ImageDecoder{
		Name:       "wic",
		Priority:   100,
		Extensions: wicImageExtensions,
		DecodeCtx:  decodeImageWIC,
		DecodeSize: decodeImageWICSize,
	})
	// "shell" is the registry key; the interface shows "Shell"
	// (IShellItemImageFactory). FromPath: the shell renders the real file
	// itself, so the pipeline hands it the path and never pulls a video's
	// bytes into memory.
	RegisterImageDecoder(ImageDecoder{
		Name:       "shell",
		Priority:   shellPriority,
		Extensions: shellImageExtensions,
		DecodeCtx:  decodeImageShell,
		FromPath:   true,
		Label:      func([]byte) string { return "Shell" },
	})
	go ensureWICFormats()
}

var ensureWICFormatsOnce sync.Once

// ensureWICFormats widens the wic decoder to the codecs installed on the
// machine, so a third-party codec serves its formats without a rebuild.
// Runs off the startup path and keeps the built-in list on failure.
func ensureWICFormats() {
	ensureWICFormatsOnce.Do(func() {
		// COM is thread-affine: the enumeration must run on the thread that
		// initialized the apartment, so pin the goroutine before CoInitialize.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		factory := wicFactory()
		if factory == 0 {
			return
		}
		if err := initDecodeCOM(); err != nil {
			return
		}
		defer windows.CoUninitialize()
		exts, err := enumWICExtensions(factory)
		if err != nil || len(exts) == 0 {
			return
		}
		if d, ok := findDecoder("wic"); ok {
			d.Extensions = mergeWICExtensions(exts)
			RegisterImageDecoder(d)
		}
	})
}

// enumWICExtensions lists the file extensions of every WIC decoder installed
// on the machine, including third-party codecs.
func enumWICExtensions(factory uintptr) ([]string, error) {
	var enum uintptr
	if err := wicCall(factory, wicFactoryCreateComponentEnumerator, wicComponentTypeDecoder, 0, uintptr(unsafe.Pointer(&enum))); err != nil || enum == 0 {
		if err == nil {
			err = fmt.Errorf("CreateComponentEnumerator returned a null enumerator")
		}
		return nil, err
	}
	defer wicRelease(enum)

	var out []string
	for {
		var unk uintptr
		if err := wicCall(enum, wicEnumUnknownNext, 1, uintptr(unsafe.Pointer(&unk)), 0); err != nil || unk == 0 {
			break
		}
		var info uintptr
		if err := wicCall(unk, 0, uintptr(unsafe.Pointer(&iidBitmapDecoderInfo)), uintptr(unsafe.Pointer(&info))); err == nil && info != 0 {
			if exts, err := wicCodecFileExtensions(info); err == nil {
				out = append(out, exts...)
			}
			wicRelease(info)
		}
		wicRelease(unk)
	}
	return out, nil
}

// wicCodecFileExtensions reads one codec's comma-separated extension list,
// asking once for the size and once for the text.
func wicCodecFileExtensions(info uintptr) ([]string, error) {
	var needed uint32
	wicCall(info, wicCodecGetFileExtensions, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if needed == 0 || needed > 1<<16 {
		return nil, fmt.Errorf("codec reports no extensions")
	}
	buf := make([]uint16, needed)
	if err := wicCall(info, wicCodecGetFileExtensions, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&needed))); err != nil {
		return nil, err
	}
	var out []string
	for _, part := range strings.Split(windows.UTF16ToString(buf), ",") {
		part = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(part, ".")))
		if part != "" {
			out = append(out, part)
		}
	}
	return out, nil
}

// mergeWICExtensions keeps the built-in list and adds the installed codecs'
// formats, except what an in-process decoder already owns (a faster reader
// must not be displaced by WIC) and the video containers (the shell's alone).
func mergeWICExtensions(enumerated []string) []string {
	excluded := make(map[string]bool)
	for _, d := range allImageDecoders() {
		if d.Name == "wic" || d.Name == "shell" || d.Name == ExternalImageDecoder {
			continue
		}
		for _, e := range d.Extensions {
			excluded[e] = true
		}
	}
	for _, e := range VideoFileExtensions {
		excluded[e] = true
	}
	seen := make(map[string]bool, len(wicImageExtensions)+len(enumerated))
	merged := make([]string, 0, len(wicImageExtensions)+len(enumerated))
	add := func(e string, guarded bool) {
		e = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(e, ".")))
		if e == "" || seen[e] || (guarded && excluded[e]) {
			return
		}
		seen[e] = true
		merged = append(merged, e)
	}
	for _, e := range wicImageExtensions {
		add(e, false)
	}
	for _, e := range enumerated {
		add(e, true)
	}
	return merged
}
