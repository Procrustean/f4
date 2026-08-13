package imagedecoders

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	_ "image/gif"
	_ "image/jpeg"

	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtui"
	"github.com/woozymasta/png"
)

// ImageDecoder turns file bytes into pixels; several may claim one extension.
type ImageDecoder struct {
	Name       string
	Priority   int
	Extensions []string
	Decode     func(data []byte) (*vtui.ImageSurface, error)

	// DecodeCtx: cancellable decoders also get the path; some (the Windows
	// shell) can only render from a real file.
	DecodeCtx func(ctx context.Context, path string, data []byte) (*vtui.ImageSurface, error)

	// DecodeSize is an optional cheap downscale (the WIC scaler); the gallery
	// uses it for tiles so a large picture never decodes whole.
	DecodeSize func(ctx context.Context, path string, data []byte, w, h int) (*vtui.ImageSurface, error)

	// FromPath marks a decoder that renders the file itself (the Windows
	// shell), so the pipeline hands it the path without pulling the bytes in,
	// which keeps a video larger than the byte cap decodable.
	FromPath bool

	// Label names the external tool in the interface.
	Label func(data []byte) string
}

func (d ImageDecoder) decode(ctx context.Context, path string, data []byte) (*vtui.ImageSurface, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if d.DecodeCtx != nil {
		return d.DecodeCtx(ctx, path, data)
	}
	return d.Decode(data)
}

// Guarded: a plugin may register while a picture is decoding.
var (
	imageDecodersMu sync.RWMutex
	imageDecoders   []ImageDecoder
)

func allImageDecoders() []ImageDecoder {
	imageDecodersMu.RLock()
	out := append([]ImageDecoder(nil), imageDecoders...)
	imageDecodersMu.RUnlock()

	// Applied here, not stored: clearing the setting restores built-in order.
	for i := range out {
		out[i].Priority = ImageDecoderPriorityOf(out[i].Name, out[i].Priority)
	}
	return out
}

func RegisterImageDecoder(d ImageDecoder) {
	if d.Name == "" || (d.Decode == nil && d.DecodeCtx == nil) {
		return
	}
	d.Extensions = normalizeImageExtensions(d.Extensions)
	imageDecodersMu.Lock()
	defer imageDecodersMu.Unlock()
	for i := range imageDecoders {
		if imageDecoders[i].Name == d.Name {
			imageDecoders[i] = d
			return
		}
	}
	imageDecoders = append(imageDecoders, d)
}

func UnregisterImageDecoder(name string) {
	imageDecodersMu.Lock()
	defer imageDecodersMu.Unlock()
	for i := range imageDecoders {
		if imageDecoders[i].Name == name {
			imageDecoders = append(imageDecoders[:i], imageDecoders[i+1:]...)
			return
		}
	}
}

var (
	imageDecoderPrioMu sync.RWMutex
	imageDecoderPrio   map[string]int
)

func SetImageDecoderPriorities(prio map[string]int) {
	imageDecoderPrioMu.Lock()
	defer imageDecoderPrioMu.Unlock()
	if len(prio) == 0 {
		imageDecoderPrio = nil
		return
	}
	imageDecoderPrio = make(map[string]int, len(prio))
	for name, value := range prio {
		imageDecoderPrio[name] = value
	}
}

func ImageDecoderPriorityOf(name string, registered int) int {
	imageDecoderPrioMu.RLock()
	defer imageDecoderPrioMu.RUnlock()
	if value, ok := imageDecoderPrio[name]; ok {
		return value
	}
	return registered
}

func imageDecoderPriorityOverridden(name string) bool {
	imageDecoderPrioMu.RLock()
	defer imageDecoderPrioMu.RUnlock()
	_, ok := imageDecoderPrio[name]
	return ok
}

// DecoderPriority pairs; a bad pair is dropped so a config typo cannot block images.
func ParseImageDecoderPriorities(spec string) map[string]int {
	out := make(map[string]int)
	for _, part := range strings.FieldsFunc(spec, func(r rune) bool {
		return r == ',' || r == ';' || r == '|'
	}) {
		colon := strings.LastIndex(part, ":")
		if colon <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:colon])
		value, err := strconv.Atoi(strings.TrimSpace(part[colon+1:]))
		if name == "" || err != nil {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func ImageExtension(path string) string {
	base := path
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	dot := strings.LastIndex(base, ".")
	if dot < 0 || dot == len(base)-1 {
		return ""
	}
	return strings.ToLower(base[dot+1:])
}

func normalizeImageExtensions(exts []string) []string {
	if len(exts) == 0 {
		return nil
	}
	out := make([]string, 0, len(exts))
	seen := make(map[string]struct{}, len(exts))
	for _, ext := range exts {
		ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
		if ext == "" {
			continue
		}
		if _, ok := seen[ext]; ok {
			continue
		}
		seen[ext] = struct{}{}
		out = append(out, ext)
	}
	return out
}

func sortImageDecoders(decoders []ImageDecoder) {
	// Stable sorting deliberately keeps registration order as the deterministic
	// tie-break for equal priorities.
	sort.SliceStable(decoders, func(i, j int) bool {
		return decoders[i].Priority > decoders[j].Priority
	})
}

func imageDecodersForExtensionFrom(path string, decoders []ImageDecoder) []ImageDecoder {
	ext := ImageExtension(path)
	if ext == "" {
		return nil
	}
	var out []ImageDecoder
	for _, d := range decoders {
		for _, e := range d.Extensions {
			if e == ext {
				out = append(out, d)
				break
			}
		}
	}
	sortImageDecoders(out)
	return out
}

func imageDecodersForExtension(path string) []ImageDecoder {
	return imageDecodersForExtensionFrom(path, allImageDecoders())
}

// imageDecoderCandidates is the automatic chain: extension claimants first,
// then the other decoders as content-sniffing fallbacks in priority order.
// External tools are last resort unless an explicit priority opts them in.
// The F4 cycle (ImageCycleDecoders) offers only the claimants, so a sniffing
// fallback cannot get pinned.
func imageDecoderCandidates(path string) []ImageDecoder {
	all := allImageDecoders()
	claimed := imageDecodersForExtensionFrom(path, all)
	if len(claimed) == 0 {
		return nil
	}
	// A video belongs to the shell alone: only path-rendering decoders can
	// read a movie, so the rest are dropped.
	if IsVideoFile(path) {
		return pathDecodersOnly(claimed)
	}
	seen := make(map[string]struct{}, len(claimed))
	var external ImageDecoder
	hasExternal := false
	nonExternalClaimed := make([]ImageDecoder, 0, len(claimed))
	for _, d := range claimed {
		seen[d.Name] = struct{}{}
		if d.Name == ExternalImageDecoder {
			external, hasExternal = d, true
			continue
		}
		nonExternalClaimed = append(nonExternalClaimed, d)
	}
	rest := make([]ImageDecoder, 0, len(all)-len(claimed))
	for _, d := range all {
		if d.Name == ExternalImageDecoder {
			if !hasExternal {
				external, hasExternal = d, true
			}
			continue
		}
		if _, ok := seen[d.Name]; !ok {
			rest = append(rest, d)
		}
	}

	if hasExternal && imageDecoderPriorityOverridden(ExternalImageDecoder) {
		if _, claimed := seen[ExternalImageDecoder]; claimed {
			nonExternalClaimed = append(nonExternalClaimed, external)
			sortImageDecoders(nonExternalClaimed)
		} else {
			rest = append(rest, external)
			sortImageDecoders(rest)
		}
	} else if hasExternal {
		// A process-backed decoder must not steal a format an in-process
		// decoder can read.
		rest = append(rest, external)
		sortImageDecoders(rest)
	}
	return append(nonExternalClaimed, rest...)
}

func ImageDecodersFor(path string) []ImageDecoder {
	return imageDecodersForExtension(path)
}

func IsImageFile(path string) bool {
	return len(ImageDecodersFor(path)) > 0
}

// VideoFileExtensions are the containers the shell decoder pulls a single
// frame from. They are video, not pictures: a preview must not read a whole
// (possibly multi-gigabyte) movie into memory.
var VideoFileExtensions = []string{
	"mp4", "mkv", "avi", "webm", "mov", "wmv", "flv", "m4v",
	"mpg", "mpeg", "3gp", "ts", "mts", "m2ts",
}

// VideoPreviewMaxSize bounds how large a video may be for its preview. The
// shell renders from the real file, so the cap only guards against stalling
// on a gigantic container. Past it, the preview is skipped quietly.
const VideoPreviewMaxSize int64 = 1 << 30

// IsVideoFile reports whether the path names a video container. The viewer
// can still open one (the shell shows a single frame), but the QuickView and
// the gallery skip them.
func IsVideoFile(path string) bool {
	ext := ImageExtension(path)
	for _, e := range VideoFileExtensions {
		if e == ext {
			return true
		}
	}
	return false
}

// PathDecoderFor returns the first decoder that claims the extension and
// renders the file itself (the shell), so the pipeline can skip the read.
func PathDecoderFor(path string) (ImageDecoder, bool) {
	for _, d := range imageDecodersForExtension(path) {
		if d.FromPath {
			return d, true
		}
	}
	return ImageDecoder{}, false
}

// ErrVideoPreviewUnavailable is the quiet refusal for a video whose frame
// cannot be rendered (a virtual file system has no real path for the shell).
var ErrVideoPreviewUnavailable = errors.New("video preview unavailable")

// DecodeImageFromPath runs one path-rendering decoder (the shell) over the
// real file; no bytes are read into memory.
func DecodeImageFromPath(ctx context.Context, path string, d ImageDecoder) (*vtui.ImageSurface, string, error) {
	return decodeImage(d, ctx, path, nil)
}

// Extension-claiming decoders first, the rest as fallbacks; a name nobody
// claims is refused outright, never sniffed.
func DecodeImage(path string, data []byte) (*vtui.ImageSurface, string, error) {
	return DecodeImageContext(context.Background(), path, data)
}

func DecodeImageContext(ctx context.Context, path string, data []byte) (*vtui.ImageSurface, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	decoders := imageDecoderCandidates(path)
	if len(decoders) == 0 {
		return nil, "", fmt.Errorf("no image decoder for %q", path)
	}

	var lastErr error
	for _, d := range decoders {
		surf, name, err := decodeImage(d, ctx, path, data)
		if err == nil {
			return surf, name, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no image decoder for %q", path)
	}
	return nil, "", lastErr
}

// DecodeImageContextSize is DecodeImageContext with an aspect-fit size hint;
// the sized flag tells the caller whether a downscaling decoder was used.
func DecodeImageContextSize(ctx context.Context, path string, data []byte, w, h int) (*vtui.ImageSurface, string, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	decoders := imageDecoderCandidates(path)
	if len(decoders) == 0 {
		return nil, "", false, fmt.Errorf("no image decoder for %q", path)
	}

	var lastErr error
	for _, d := range decoders {
		surf, name, sized, err := decodeImageSize(d, ctx, path, data, w, h)
		if err == nil {
			return surf, name, sized, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no image decoder for %q", path)
	}
	return nil, "", false, lastErr
}

func decoderDisplayName(d ImageDecoder, data []byte) string {
	if d.Label != nil {
		if label := d.Label(data); label != "" {
			return label
		}
	}
	return d.Name
}

var ErrEmptyDecoderResult = errors.New("empty decoder result")

func decodeImage(d ImageDecoder, ctx context.Context, path string, data []byte) (*vtui.ImageSurface, string, error) {
	surf, name, _, err := decodeImageSize(d, ctx, path, data, 0, 0)
	return surf, name, err
}

// decodeImageSize is decodeImage with a size hint: a decoder with a DecodeSize
// path (the WIC scaler) shrinks while reading; the rest decode whole. The
// sized flag reports which path ran.
func decodeImageSize(d ImageDecoder, ctx context.Context, path string, data []byte, w, h int) (*vtui.ImageSurface, string, bool, error) {
	var (
		surf  *vtui.ImageSurface
		err   error
		sized bool
	)
	if w > 0 && h > 0 && d.DecodeSize != nil {
		surf, err = d.DecodeSize(ctx, path, data, w, h)
		sized = true
	} else {
		surf, err = d.decode(ctx, path, data)
	}
	surf, err = finishDecode(d, surf, err)
	return surf, decoderDisplayName(d, data), sized, err
}

// finishDecode rejects a decoder's empty answer and marks opaque surfaces so
// the block renderer can skip per-pixel blending.
func finishDecode(d ImageDecoder, surf *vtui.ImageSurface, err error) (*vtui.ImageSurface, error) {
	if err == nil && (surf == nil || !surf.Valid()) {
		err = fmt.Errorf("%w: decoder %s produced an empty image", ErrEmptyDecoderResult, d.Name)
	}
	if err == nil && surf != nil && !surf.Opaque {
		surf.Opaque = surfaceIsOpaque(surf)
	}
	return surf, err
}

// surfaceIsOpaque scans alpha bytes for decoders that don't set Opaque.
func surfaceIsOpaque(s *vtui.ImageSurface) bool {
	for y := 0; y < s.Height; y++ {
		off := y*s.Stride + 3
		for x := 0; x < s.Width; x++ {
			if s.Pix[off] != 255 {
				return false
			}
			off += 4
		}
	}
	return true
}

func findDecoder(name string) (ImageDecoder, bool) {
	for _, d := range allImageDecoders() {
		if d.Name == name {
			return d, true
		}
	}
	return ImageDecoder{}, false
}

// One named decoder even when unclaimed; fallbacks get their turn.
func decodeImagePreferred(ctx context.Context, path string, data []byte, preferred string) (*vtui.ImageSurface, string, error) {
	d, ok := findDecoder(preferred)
	if !ok {
		return nil, "", fmt.Errorf("no decoder named %q", preferred)
	}
	return decodeImage(d, ctx, path, data)
}

const maxImageFileSize = 128 << 20

const imageMaxPixels = 64 << 20

// validateImageAllocation rejects invalid dimensions before any pixel buffer
// is allocated, so overflow checks don't drift apart per format. maxBytes may
// be zero when only the pixel limit applies.
func validateImageAllocation(width, height, bytesPerPixel int, maxPixels, maxBytes int64) error {
	if width <= 0 || height <= 0 || bytesPerPixel <= 0 {
		return fmt.Errorf("invalid image dimensions %dx%d", width, height)
	}
	w, h := uint64(width), uint64(height)
	if maxPixels > 0 && w > uint64(maxPixels)/h {
		return fmt.Errorf("image dimensions exceed pixel limit: %dx%d", width, height)
	}
	if w > ^uint64(0)/h {
		return fmt.Errorf("image dimensions overflow: %dx%d", width, height)
	}
	pixels := w * h
	bytesPerPixel64 := uint64(bytesPerPixel)
	if maxBytes > 0 && pixels > uint64(maxBytes)/bytesPerPixel64 {
		return fmt.Errorf("image dimensions exceed decoded memory limit: %dx%d", width, height)
	}
	if pixels > ^uint64(0)/bytesPerPixel64 {
		return fmt.Errorf("image dimensions exceed decoded memory limit: %dx%d", width, height)
	}
	return nil
}

// ImageReadFileBytes is the uncached transfer used by ImagePipeline.FileBytes.
func ImageReadFileBytes(ctx context.Context, v vfs.VFS, path string) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("no filesystem provider")
	}
	f, err := v.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size := f.Size()
	if size <= 0 {
		return nil, fmt.Errorf("file is empty")
	}
	if size > maxImageFileSize {
		return nil, fmt.Errorf("image is too large: %d bytes", size)
	}

	data := make([]byte, size)
	n, err := f.ReadAt(ctx, data, 0)
	if n != len(data) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		if n == 0 && err == io.ErrUnexpectedEOF {
			err = fmt.Errorf("nothing could be read")
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

type ImageDecoderChoice struct {
	key   string // what the viewer remembers: "go-std" or "external@im"
	label string // what the interface shows
}

const ImageDecoderChoiceTool = "external@"

func ImageDecoderChoices(path string) []ImageDecoderChoice {
	var out []ImageDecoderChoice
	for _, d := range ImageCycleDecoders(path) {
		if d.Name != ExternalImageDecoder {
			out = append(out, ImageDecoderChoice{key: d.Name, label: decoderDisplayName(d, nil)})
			continue
		}
		for _, tool := range installedExternalImageTools() {
			out = append(out, ImageDecoderChoice{
				key:   ImageDecoderChoiceTool + tool.Label,
				label: tool.Label,
			})
		}
	}
	return out
}

// ImageCycleDecoders is what the F4 cycle offers: the extension claimants in
// priority order (sniffing fallbacks would only land the cycle on a decoder
// that cannot read the picture). A video offers only its path decoder.
func ImageCycleDecoders(path string) []ImageDecoder {
	decoders := imageDecodersForExtension(path)
	if !IsVideoFile(path) {
		return decoders
	}
	return pathDecodersOnly(decoders)
}

// pathDecodersOnly keeps the decoders that render the file itself; the rest
// cannot read a container (a video) from its bytes.
func pathDecodersOnly(decoders []ImageDecoder) []ImageDecoder {
	out := make([]ImageDecoder, 0, len(decoders))
	for _, d := range decoders {
		if d.FromPath {
			out = append(out, d)
		}
	}
	return out
}

func ImageNextDecoder(opts []ImageDecoderChoice, req, current string) string {
	if len(opts) < 2 {
		return ""
	}
	idx := indexOfChoice(opts, req)
	if idx < 0 {
		idx = indexOfChoice(opts, current)
	}
	if idx < 0 {
		idx = len(opts) - 1
	}
	nxt := opts[(idx+1)%len(opts)].key
	if nxt == req || (req == "" && nxt == current) {
		return ""
	}
	return nxt
}

func indexOfChoice(opts []ImageDecoderChoice, keyOrLabel string) int {
	for i, o := range opts {
		if o.key == keyOrLabel || o.label == keyOrLabel {
			return i
		}
	}
	return -1
}

// Still valid? A converter can vanish between pictures.
func ImageChoiceValid(key string) bool {
	if label, ok := strings.CutPrefix(key, ImageDecoderChoiceTool); ok {
		_, found := externalImageToolByLabel(label)
		return found
	}
	return ImageDecoderRegistered(key)
}

func ImageDecoderRegistered(name string) bool {
	for _, d := range allImageDecoders() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// Decode progress: the viewer shows a percentage while the toast is up. The
// sink is an atomic counter carried through the context, so no decode
// signature changes.
type decodeProgressKey struct{}

// WithDecodeProgress attaches a progress sink to a decode context; the WIC
// decoder reports into it while copying pixels.
func WithDecodeProgress(ctx context.Context, pct *atomic.Int32) context.Context {
	if ctx == nil || pct == nil {
		return ctx
	}
	return context.WithValue(ctx, decodeProgressKey{}, pct)
}

func decodeProgressFrom(ctx context.Context) *atomic.Int32 {
	if ctx == nil {
		return nil
	}
	pct, _ := ctx.Value(decodeProgressKey{}).(*atomic.Int32)
	return pct
}

// ApplyImageOrientation turns a decoded picture the way its EXIF orientation
// prescribes. 1 (and unknown) returns the source untouched; 5-8 swap sides.
// The result is a fresh, tightly packed surface.
func ApplyImageOrientation(src *vtui.ImageSurface, orient int) *vtui.ImageSurface {
	if src == nil || !src.Valid() || orient < 2 || orient > 8 {
		return src
	}
	w, h := src.Width, src.Height
	dstW, dstH := w, h
	if orient >= 5 {
		dstW, dstH = h, w
	}
	dst := vtui.NewImageSurface(dstW, dstH)
	if dst == nil {
		return src
	}
	dst.Opaque = src.Opaque
	for y := 0; y < h; y++ {
		srow := y * src.Stride
		for x := 0; x < w; x++ {
			var sx, sy int
			switch orient {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5: // transpose
				sx, sy = y, x
			case 6: // 90 CW
				sx, sy = h-1-y, x
			case 7: // transverse
				sx, sy = h-1-y, w-1-x
			case 8: // 270 CW
				sx, sy = y, w-1-x
			}
			d := sy*dst.Stride + sx*4
			copy(dst.Pix[d:d+4], src.Pix[srow+x*4:srow+x*4+4])
		}
	}
	return dst
}

// LoadImageForBytesSize decodes with an aspect-fit size hint; a decoder that
// can shrink while reading (WIC) honours it, and the sized flag reports that
// the result is not the full picture. A pinned decoder ignores the hint.
func LoadImageForBytesSize(ctx context.Context, path string, data []byte, choice string, w, h int) (*vtui.ImageSurface, string, bool, error) {
	if choice != "" {
		surf, name, err := LoadImageForBytes(ctx, path, data, choice)
		return surf, name, false, err
	}
	return DecodeImageContextSize(ctx, path, data, w, h)
}

func LoadImageForBytes(ctx context.Context, path string, data []byte, choice string) (*vtui.ImageSurface, string, error) {
	if choice == "" {
		return DecodeImageContext(ctx, path, data)
	}
	if label, ok := strings.CutPrefix(choice, ImageDecoderChoiceTool); ok {
		tool, found := externalImageToolByLabel(label)
		if !found {
			return nil, "", fmt.Errorf("no converter %q on the PATH", label)
		}
		surf, err := decodeImageExternallyTool(ctx, tool, data)
		if err != nil {
			return nil, "", err
		}
		return surf, tool.Label, nil
	}
	return decodeImagePreferred(ctx, path, data, choice)
}

// validateStdImageDimensions parses only the header — no pixels are decoded —
// and rejects a decompression bomb: a tiny file whose header claims a huge
// canvas would otherwise make the stdlib decoders allocate gigabytes. A
// DecodeConfig failure is not fatal here; the real decode reports it.
func validateStdImageDimensions(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return validateImageAllocation(cfg.Width, cfg.Height, 4, imageMaxPixels, 0)
}

// decodeStdImageStream decodes through the standard library; it buffers the
// stream so the dimension guard can run over the bytes first.
func decodeStdImageStream(r io.Reader) (*vtui.ImageSurface, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return DecodeImageWithStdlib(data)
}

// Convert opaque standard-library formats without the generic alpha scan.
func surfaceFromDecodedImage(img image.Image) (*vtui.ImageSurface, error) {
	switch m := img.(type) {
	case *image.NRGBA:
		return surfaceFromRGBA(m.Rect.Dx(), m.Rect.Dy(), m.Stride, m.Pix)
	case *image.RGBA:
		return surfaceFromRGBA(m.Rect.Dx(), m.Rect.Dy(), m.Stride, m.Pix)
	case *image.YCbCr, *image.Gray, *image.CMYK:
		return surfaceFromOpaque(img)
	}
	surf := vtui.NewImageSurfaceFromImage(img)
	if surf == nil {
		return nil, fmt.Errorf("unsupported image geometry")
	}
	return surf, nil
}

func surfaceFromRGBA(w, h, stride int, pix []byte) (*vtui.ImageSurface, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("unsupported image geometry")
	}
	surf := vtui.NewImageSurfaceFromPix(w, h, stride, pix)
	if surf == nil || !surf.Valid() {
		return nil, fmt.Errorf("unsupported image geometry")
	}
	return surf, nil
}

// surfaceFromOpaque marks the by-construction opaque result for the fast path.
func surfaceFromOpaque(img image.Image) (*vtui.ImageSurface, error) {
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("unsupported image geometry")
	}
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	surf := vtui.NewImageSurfaceFromPix(b.Dx(), b.Dy(), out.Stride, out.Pix)
	if surf == nil {
		return nil, fmt.Errorf("unsupported image geometry")
	}
	surf.Opaque = true
	return surf, nil
}

// PNG fast path: magic bytes, not the extension, decide.
func DecodeImageWithStdlib(data []byte) (*vtui.ImageSurface, error) {
	// Every other in-process decoder bounds its allocation; the stdlib
	// decoders only check integer overflow, so this header-first check keeps
	// a small file with a huge canvas from OOMing the process.
	if err := validateStdImageDimensions(data); err != nil {
		return nil, err
	}
	if isPNG(data) {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return surfaceFromDecodedImage(img)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return surfaceFromDecodedImage(img)
}

var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

func isPNG(data []byte) bool {
	return len(data) >= len(pngSignature) && bytes.Equal(data[:len(pngSignature)], pngSignature)
}

func init() {
	RegisterImageDecoder(ImageDecoder{
		Name:       "go-std",
		Priority:   0,
		Extensions: []string{"png", "jpg", "jpeg", "jfif", "gif"},
		Decode:     DecodeImageWithStdlib,
	})
}
