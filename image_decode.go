package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

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

	// DecodeCtx: process-leaving decoders can be cancelled mid-decode.
	DecodeCtx func(ctx context.Context, data []byte) (*vtui.ImageSurface, error)

	// Label names the external tool in the interface.
	Label func(data []byte) string
}

func (d ImageDecoder) decode(ctx context.Context, data []byte) (*vtui.ImageSurface, error) {
	if d.DecodeCtx != nil {
		return d.DecodeCtx(ctx, data)
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
		out[i].Priority = imageDecoderPriorityOf(out[i].Name, out[i].Priority)
	}
	return out
}

func RegisterImageDecoder(d ImageDecoder) {
	if d.Name == "" || (d.Decode == nil && d.DecodeCtx == nil) {
		return
	}
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

func imageDecoderPriorityOf(name string, registered int) int {
	imageDecoderPrioMu.RLock()
	defer imageDecoderPrioMu.RUnlock()
	if value, ok := imageDecoderPrio[name]; ok {
		return value
	}
	return registered
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

func imageExtension(path string) string {
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

func ImageDecodersFor(path string) []ImageDecoder {
	ext := imageExtension(path)
	if ext == "" {
		return nil
	}
	var out []ImageDecoder
	for _, d := range allImageDecoders() {
		for _, e := range d.Extensions {
			if strings.ToLower(e) == ext {
				out = append(out, d)
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority > out[j].Priority
	})
	return out
}

func IsImageFile(path string) bool {
	return len(ImageDecodersFor(path)) > 0
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
	decoders := ImageDecodersFor(path)
	if len(decoders) == 0 {
		return nil, "", fmt.Errorf("no image decoder for %q", path)
	}

	claimed := make(map[string]bool, len(decoders))
	for _, d := range decoders {
		claimed[d.Name] = true
	}
	rest := make([]ImageDecoder, 0, len(claimed))
	for _, d := range allImageDecoders() {
		if !claimed[d.Name] {
			rest = append(rest, d)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		return rest[i].Priority > rest[j].Priority
	})
	decoders = append(decoders, rest...)

	var lastErr error
	for _, d := range decoders {
		surf, name, err := decodeImage(d, ctx, data)
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

func decoderDisplayName(d ImageDecoder, data []byte) string {
	if d.Label != nil {
		if label := d.Label(data); label != "" {
			return label
		}
	}
	return d.Name
}

func decodeImage(d ImageDecoder, ctx context.Context, data []byte) (*vtui.ImageSurface, string, error) {
	surf, err := d.decode(ctx, data)
	if err == nil && !surf.Valid() {
		err = fmt.Errorf("decoder %s produced an empty image", d.Name)
	}
	return surf, decoderDisplayName(d, data), err
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
	return decodeImage(d, ctx, data)
}

const maxImageFileSize = 128 << 20

const imageMaxPixels = 64 << 20

func imageFileBytes(ctx context.Context, v vfs.VFS, path string) ([]byte, error) {
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
	if n <= 0 {
		if err == nil {
			err = fmt.Errorf("nothing could be read")
		}
		return nil, err
	}
	return data[:n], nil
}

func LoadImage(ctx context.Context, v vfs.VFS, path string) (*vtui.ImageSurface, string, error) {
	return loadImageForChoice(ctx, v, path, "")
}

type imageDecoderChoice struct {
	key   string // what the viewer remembers: "go-std" or "external@im"
	label string // what the interface shows
}

const imageDecoderChoiceTool = "external@"

func imageDecoderChoices(path string) []imageDecoderChoice {
	var out []imageDecoderChoice
	for _, d := range imageCycleDecoders(path) {
		if d.Name != externalImageDecoder {
			out = append(out, imageDecoderChoice{key: d.Name, label: d.Name})
			continue
		}
		for _, tool := range installedExternalImageTools() {
			out = append(out, imageDecoderChoice{
				key:   imageDecoderChoiceTool + tool.Label,
				label: tool.Label,
			})
		}
	}
	return out
}

func imageCycleDecoders(path string) []ImageDecoder {
	claimed := ImageDecodersFor(path)
	if len(claimed) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(claimed))
	for _, d := range claimed {
		seen[d.Name] = true
	}
	rest := make([]ImageDecoder, 0, 4)
	for _, d := range allImageDecoders() {
		if !seen[d.Name] {
			rest = append(rest, d)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		return rest[i].Priority > rest[j].Priority
	})
	return append(claimed, rest...)
}

func imageNextDecoder(opts []imageDecoderChoice, req, current string) string {
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

func indexOfChoice(opts []imageDecoderChoice, keyOrLabel string) int {
	for i, o := range opts {
		if o.key == keyOrLabel || o.label == keyOrLabel {
			return i
		}
	}
	return -1
}

// Still valid? A converter can vanish between pictures.
func imageChoiceValid(key string) bool {
	if label, ok := strings.CutPrefix(key, imageDecoderChoiceTool); ok {
		_, found := externalImageToolByLabel(label)
		return found
	}
	return imageDecoderRegistered(key)
}

func imageDecoderRegistered(name string) bool {
	for _, d := range allImageDecoders() {
		if d.Name == name {
			return true
		}
	}
	return false
}

func loadImageForChoice(ctx context.Context, v vfs.VFS, path, choice string) (*vtui.ImageSurface, string, error) {
	data, err := imageFileBytes(ctx, v, path)
	if err != nil {
		return nil, "", err
	}
	return loadImageForBytes(ctx, path, data, choice)
}

func loadImageForBytes(ctx context.Context, path string, data []byte, choice string) (*vtui.ImageSurface, string, error) {
	if choice == "" {
		return DecodeImageContext(ctx, path, data)
	}
	if label, ok := strings.CutPrefix(choice, imageDecoderChoiceTool); ok {
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

// Built-ins decode whole; converters shrink while reading.
func loadGalleryTile(ctx context.Context, v vfs.VFS, path string, w, h int) ImageResult {
	if w < 1 {
		w = (imageTileCols - 2) * imageViewFallbackCellW
	}
	if h < 1 {
		h = (imageTileRows - 2) * imageViewFallbackCellH
	}
	decoders := ImageDecodersFor(path)
	if len(decoders) == 0 || decoders[0].Name != externalImageDecoder {
		return ImagePipe.LoadTileSync(ctx, v, path)
	}
	surf, decoder, err := loadImageScaled(ctx, v, path, w, h)
	if err != nil {
		return ImagePipe.LoadTileSync(ctx, v, path)
	}
	return ImageResult{Path: path, Surface: surf, Decoder: decoder}
}

func decodeStdImageStream(r io.Reader) (*vtui.ImageSurface, error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, err
	}
	return surfaceFromDecodedImage(img)
}

// RGBA/NRGBA straight copy, skipping unpremultiply; others go the generic path.
func surfaceFromDecodedImage(img image.Image) (*vtui.ImageSurface, error) {
	var dx, dy, stride int
	var pix []byte
	switch m := img.(type) {
	case *image.NRGBA:
		dx, dy, stride, pix = m.Rect.Dx(), m.Rect.Dy(), m.Stride, m.Pix
	case *image.RGBA:
		dx, dy, stride, pix = m.Rect.Dx(), m.Rect.Dy(), m.Stride, m.Pix
	}
	if dx > 0 && dy > 0 {
		if surf := vtui.NewImageSurfaceFromPix(dx, dy, stride, pix); surf != nil && surf.Valid() {
			return surf, nil
		}
	}
	surf := vtui.NewImageSurfaceFromImage(img)
	if surf == nil {
		return nil, fmt.Errorf("unsupported image geometry")
	}
	return surf, nil
}

// PNG fast path: magic bytes, not the extension, decide.
func decodeImageWithStdlib(data []byte) (*vtui.ImageSurface, error) {
	if isPNG(data) {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return surfaceFromDecodedImage(img)
	}
	return decodeStdImageStream(bytes.NewReader(data))
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
		Decode:     decodeImageWithStdlib,
	})
}
