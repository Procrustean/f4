package main

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/unxed/f4/imagedecoders"
	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

const (
	imageViewMinZoom = 0.05
	imageViewMaxZoom = 40.0

	// 20% per press: firm step between coarse 25% and a too-fine gradation.
	imageViewZoomFactor = 1.2

	// Fallback cell size when the terminal can't report one; it only affects
	// aspect ratio.
	imageViewFallbackCellW = 8
	imageViewFallbackCellH = 16

	// How many pictures on each side are decoded before they are asked for.
	imageViewPrefetchRadius = 2

	imageViewToastDelay = 2 * time.Second // toast stays a moment

	// The toast slides out over the left edge; the slide, the wall flash and
	// the loading band all redraw on one tick.
	imageToastSlideDur  = 150 * time.Millisecond
	imageToastSlideTick = 30 * time.Millisecond

	// One full pass of the bright band across the loading slab.
	imageLoadingCycle = 900 * time.Millisecond

	// How long the end-of-list toast keeps its inverted "wall" colours.
	imageToastFlashDur = 200 * time.Millisecond

	// Decode may run this long before the viewer admits it is working.
	imageViewDecodeDelay = 500 * time.Millisecond

	// Loading toast palette: pure greys snapped onto the XTerm-256 ramp, so
	// the 256-colour fallback matches truecolor.
	imageLoadingText = 0xE4E4E4

	// Resting and brightest slab greys. The brightest (0x58) stays 14 steps
	// below the text grey (0xE4), so no shade lands under a letter.
	imageSlabDim    = 0x1C1C1C
	imageSlabBright = 0x585858

	// The comet's half-width in cells.
	imageCometHalfWidth = 3
)

var imageViewBackAttr = vtui.SetRGBBoth(0, 0xC0C0C0, blockImageBack)

// overlay: opaque dark slab keeps the info line legible.
var imageOverlayAttr = vtui.SetRGBBoth(0, 0xFFFFFF, 0x000000)

// toast: white on a dark slab; the light glyphs keep it readable on the image.
var imageToastAttr = vtui.SetRGBBoth(0, 0xFFFFFF, 0x333333)

// A blocked step flashes the toast and the OSD in inverted "wall" colours:
// dark glyphs on a lit slab. 0xC0C0C0 is the XTerm-256 light grey.
var imageWallFlashAttr = vtui.SetRGBBoth(0, 0x101010, 0xC0C0C0)

// ImageView shows a single picture full screen.
type ImageView struct {
	vtui.BaseFrame
	topBar *TopBar

	vfs     vfs.VFS
	path    string
	surface *vtui.ImageSurface
	decoder string
	gfxKey  string

	siblings []string
	index    int

	preview    bool
	loading    bool
	err        error
	loadGen    uint64
	actual     bool
	fitPct     int // the fitted percentage, for the scale toast on the way out
	full       bool
	lastScale  float64
	zoom       float64 // the zoom currently on screen
	panX, panY float64

	// How far the picture can still move per axis, as of the last frame. Zero
	// means it fits and there is nothing to pan.
	panMaxX, panMaxY float64

	// Last geometry line written to the log, so an idle picture doesn't fill it.
	lastGeom string

	// Orientation chosen by the reader; surface holds the decoded picture,
	// shown the turned/mirrored copy (nil when seen as decoded).
	rotation     int
	flipH, flipV bool
	shown        *vtui.ImageSurface

	// Last console size, so full-screen toggles can relayout without a resize.
	conW, conH int

	overlay   bool
	fileSize  int64
	sizeKnown bool
	fileTime  time.Time
	timeKnown bool
	gal       *imageGallery
	selected  map[string]bool
	slideStop chan struct{}

	// reqDecoder pins the next decode; empty = the automatic chain.
	reqDecoder string

	// decodeDur: decode time reported by the overlay.
	decodeDur time.Duration

	// zoomFocus: anchor under the window centre; anchorPending = not placed yet.
	zoomFocusX, zoomFocusY float64
	anchorPending          bool
	visW, visH             int

	// tempMsg: transient message, lower-left.
	tempMsg   string
	tempUntil time.Time

	// tempFlashUntil: how long the toast keeps the inverted "wall" colours.
	tempFlashUntil time.Time

	// tempSlideStart: when the toast's exit began (zero = not exiting).
	tempSlideStart time.Time

	// animStop ends the ticker that drives the toast animations; nil when idle.
	animStop chan struct{}

	// loadRow reuses the loading slab's cells across frames, so a long decode
	// doesn't allocate a row per tick.
	loadRow []vtui.CharInfo

	decodeStart  time.Time          // toast once past imageViewDecodeDelay
	decodeCancel context.CancelFunc // stops an in-flight decode on moving on

	// block / blockTiles cache half-block cells (one per picture, one per
	// gallery tile), so resampling runs only on geometry moves.
	block      *blockRender
	blockTiles map[int]*blockRender

	OnClose    func()
	OnSelect   func(path string, selected bool)
	OnNavigate func(path string)
}

// NewImageView loads and decodes the file. Decoding happens here rather than
// lazily so that a failure can still be reported as a normal open error.
func NewImageView(ctx context.Context, v vfs.VFS, path string) (*ImageView, error) {
	// A file that carries a thumbnail opens at once and sharpens when the
	// megapixels arrive; one that does not is waited for.
	res, ok := ImagePipe.PreviewSync(ctx, v, path)
	if !ok {
		res = ImagePipe.LoadSync(ctx, v, path)
		if res.Err != nil {
			return nil, res.Err
		}
	}
	surf, decoder := res.Surface, res.Decoder

	iv := &ImageView{
		vfs:       v,
		path:      path,
		surface:   surf,
		decoder:   decoder,
		preview:   res.Preview,
		zoom:      1,
		full:      AppConfig.ImageFullScreen,
		overlay:   AppConfig.ImageShowOverlay,
		decodeDur: res.DecodeDur,
		block:     &blockRender{},
	}
	iv.gfxKey = fmt.Sprintf("f4.imageview:%p", iv)

	iv.index = -1
	iv.topBar = NewTopBar(
		func() string {
			// Just the name and the resolution, one space apart; the
			// workspace counter "[N]" draws over the empty corner.
			return " " + iv.titleName() + " " + iv.displaySize()
		},
		nil,
	)
	iv.topBar.SetVisible(true)
	iv.SetCanFocus(true)
	iv.SetFocus(true)

	// What is on screen is a stand-in; ask for the real thing.
	if res.Preview {
		iv.startDecode()
	}
	if iv.overlay {
		iv.requestFileSize()
	}
	if res.Preview {
		gen := iv.loadGen
		ImagePipe.Load(v, path, func(full ImageResult) {
			iv.accept(gen, full)
		})
	}
	return iv, nil
}

// barHeight is how many rows the title bar takes from the picture.
func (iv *ImageView) barHeight() int {
	if iv.full {
		return 0
	}
	return 1
}

// SetSiblings tells the viewer which pictures stand next to this one, in the
// order the panel shows them.
func (iv *ImageView) SetSiblings(paths []string, index int) {
	iv.siblings = paths
	iv.index = index
	iv.prefetch()
}

// prefetch decodes the neighbours in advance: the nearest whole, the ring
// beyond only their embedded thumbnail (a header read, not a full decode).
func (iv *ImageView) prefetch() {
	if iv.index < 0 || iv.index >= len(iv.siblings) {
		return
	}
	near := ImageNeighbourhood(iv.siblings, iv.index, 1)
	ImagePipe.Prefetch(iv.vfs, near)
	if ring := ImageNeighbourhood(iv.siblings, iv.index, imageViewPrefetchRadius); len(ring) > len(near) {
		ImagePipe.PreviewPrefetch(iv.vfs, ring[len(near):])
	}
}

// Step walks the siblings, stopping at the ends; the toast announces an edge,
// flashing when it refused to move.
func (iv *ImageView) Step(delta int) {
	total := len(iv.siblings)
	if total == 0 || iv.index < 0 {
		return
	}
	idx := iv.index + delta
	if idx < 0 {
		idx = 0
	}
	if idx >= total {
		idx = total - 1
	}
	blocked := idx == iv.index
	iv.GoTo(idx)
	// Another shove at the same wall blinks the number already in the corner.
	if blocked && iv.tempMsg == fmt.Sprintf("[%d/%d]", idx+1, total) {
		iv.reFlash()
	} else if !iv.loading && (blocked || idx == 0 || idx == total-1) {
		iv.edgeToast(idx, blocked)
	}
}

// edgeToast announces the position on an edge, flashing when it refused to
// move; a decode or message in progress keeps it quiet.
func (iv *ImageView) edgeToast(idx int, blocked bool) {
	if iv.loading || iv.tempMsg != "" {
		return
	}
	msg := fmt.Sprintf("[%d/%d]", idx+1, len(iv.siblings))
	if blocked {
		iv.flashToast(msg)
	} else {
		iv.toast(msg)
	}
}

// GoTo shows the sibling at the given position.
func (iv *ImageView) GoTo(idx int) {
	if idx < 0 || idx >= len(iv.siblings) || iv.siblings[idx] == iv.path {
		return
	}
	iv.index = idx
	iv.open(iv.siblings[idx])
	if iv.OnNavigate != nil {
		iv.OnNavigate(iv.path)
	}
}

// Reload decodes the file again, for a picture that has changed since it was
// put on screen.
func (iv *ImageView) Reload() {
	ImagePipe.Invalidate(iv.vfs, iv.path)
	iv.open(iv.path)
	iv.toast("re-decoded")
}

// open puts another picture on screen; a decoded one appears at once, else
// the previous picture stays until the new one arrives.
func (iv *ImageView) open(path string) {
	if path != iv.path {
		// The toast answers the picture it was said over; drop it at once.
		iv.toastClear()
	}
	iv.cancelDecode()
	iv.path = path
	iv.zoom = 1
	iv.panX, iv.panY = 0, 0
	iv.rotation, iv.flipH, iv.flipV = 0, false, false
	iv.fileSize, iv.sizeKnown = 0, false
	iv.fileTime, iv.timeKnown = time.Time{}, false
	iv.shown = nil
	iv.err = nil
	iv.decodeDur = 0
	iv.loadGen++
	gen := iv.loadGen
	iv.startDecode()
	iv.prefetch()
	if iv.overlay {
		iv.requestFileSize()
	}
	if iv.reqDecoder != "" && !imagedecoders.ImageChoiceValid(iv.reqDecoder) {
		iv.reqDecoder = ""
	}

	if iv.reqDecoder == "" {
		if res, ok := ImagePipe.Cached(iv.vfs, path); ok {
			iv.accept(gen, res)
			return
		}
		iv.openAutomatic(gen, path)
		return
	}
	iv.openPinned(gen, path, iv.reqDecoder)
}

// startDecode marks the moment the decoder went to work, so the loading
// toast can appear once imageViewDecodeDelay has passed.
func (iv *ImageView) startDecode() {
	iv.loading = true
	iv.decodeStart = time.Now()
}

func (iv *ImageView) openAutomatic(gen uint64, path string) {
	v := iv.vfs
	iv.decodeCancel = vtui.RunAsync(func(ctx *vtui.TaskContext) {
		if res, ok := ImagePipe.PreviewSync(ctx.Context, v, path); ok {
			ctx.RunOnUI(func() { iv.accept(gen, res) })
		}
		res := ImagePipe.LoadSync(ctx.Context, v, path)
		ctx.RunOnUI(func() { iv.accept(gen, res) })
	}).Cancel
}

// Pin: named decoder only; a non-reader falls back, dropping the pin.
func (iv *ImageView) openPinned(gen uint64, path, dec string) {
	v := iv.vfs
	iv.decodeCancel = vtui.RunAsync(func(ctx *vtui.TaskContext) {
		start := time.Now()
		pinned := false
		var surf *vtui.ImageSurface
		var decoder string
		var err error
		// A path-rendering decoder (the shell) reads the real file itself;
		// a video must not be pulled into memory just to hand over its path.
		// The pin sticks only when it names this decoder.
		if d, ok := imagedecoders.PathDecoderFor(path); ok {
			surf, decoder, err = imagedecoders.DecodeImageFromPath(ctx.Context, path, d)
			switch {
			case err == nil && (dec == "" || dec == d.Name):
				pinned = true
			case err == nil:
				surf, decoder, err = nil, "", nil // honour a different pin below
			case imagedecoders.IsVideoFile(path):
				err = imagedecoders.ErrVideoPreviewUnavailable // no real path
			default:
				surf, decoder, err = nil, "", nil // fall through to the bytes
			}
		}
		if surf == nil && err == nil {
			// Read the bytes and let the pinned decoder (automatic chain as
			// fallback) have them.
			data, derr := ImagePipe.FileBytes(ctx.Context, v, path)
			if derr != nil {
				err = derr
			} else {
				surf, decoder, err = imagedecoders.LoadImageForBytes(ctx.Context, path, data, dec)
				pinned = err == nil
				if err != nil {
					start = time.Now()
					surf, decoder, err = imagedecoders.LoadImageForBytes(ctx.Context, path, data, "")
				}
			}
		}
		res := ImageResult{Path: path, Surface: surf, Decoder: decoder, Err: err, DecodeDur: time.Since(start)}
		ctx.RunOnUI(func() {
			if gen == iv.loadGen && !pinned && err == nil {
				iv.reqDecoder = ""
			}
			iv.accept(gen, res)
		})
	}).Cancel
}

func (iv *ImageView) CycleDecoder() {
	next := imagedecoders.ImageNextDecoder(imagedecoders.ImageDecoderChoices(iv.path), iv.reqDecoder, iv.decoder)
	if next == "" {
		return
	}
	iv.reqDecoder = next
	iv.open(iv.path)
}

func (iv *ImageView) cancelDecode() {
	if iv.decodeCancel != nil {
		iv.decodeCancel()
		iv.decodeCancel = nil
	}
}

// accept takes a result unless the reader has moved on since asking for it.
func (iv *ImageView) accept(gen uint64, res ImageResult) {
	if gen != iv.loadGen {
		return
	}
	if res.Err != nil {
		iv.loading = false
		iv.err = res.Err
		iv.toast("error: " + res.Err.Error())
		vtui.DebugLog("IMAGE: %s: %v", res.Path, res.Err)
		return
	}
	iv.SetImage(res)
	iv.loading = res.Preview
	if !res.Preview && res.Decoder != "" && res.DecodeDur > imageViewDecodeDelay && !iv.tempActive() {
		iv.toast(decoderWithTime(res.Decoder, res.DecodeDur))
	}
}

// baseScale is what a zoom of one means: the picture fitted into the window,
// or its own pixels when the actual size is asked for.
func (iv *ImageView) baseScale(boxW, boxH int) float64 {
	img := iv.display()
	if !img.Valid() || img.Width <= 0 || iv.actual {
		return 1
	}
	fitW, _ := vtui.FitInside(img.Width, img.Height, boxW, boxH)
	if fitW <= 0 {
		return 1
	}
	return float64(fitW) / float64(img.Width)
}

// ToggleActualSize switches between window-fit and literal pixels; the toast
// says the new scale.
func (iv *ImageView) ToggleActualSize() {
	iv.actual = !iv.actual
	iv.zoom = 1
	iv.panX, iv.panY = 0, 0
	if iv.actual {
		iv.fitPct = iv.scalePercent()
		iv.toast("scale: 100%")
	} else {
		iv.toast(fmt.Sprintf("scale: %d%%", iv.fitPct))
	}
}

// display is the turned/mirrored copy when orientation changed, else surface.
func (iv *ImageView) display() *vtui.ImageSurface {
	if iv.shown.Valid() {
		return iv.shown
	}
	return iv.surface
}

// rebuild bakes the orientation into pixels; a backend can only ship a
// rectangle, so a turn cannot be expressed in the placement.
func (iv *ImageView) rebuild() {
	if iv.rotation == 0 && !iv.flipH && !iv.flipV {
		iv.shown = nil
		return
	}
	iv.shown = TransformSurface(iv.surface, iv.rotation, iv.flipH, iv.flipV)
}

// Rotate turns the picture clockwise by a multiple of ninety degrees.
func (iv *ImageView) Rotate(delta int) {
	// A mirror reverses the turn direction, so with one axis mirrored the
	// stored angle moves the other way.
	if iv.flipH != iv.flipV {
		delta = -delta
	}
	iv.rotation = ((iv.rotation+delta)%360 + 360) % 360
	iv.panX, iv.panY = 0, 0
	iv.rebuild()
	iv.toast("rotated")
}

// Flip mirrors the picture as it is seen, so it is applied after the turn.
func (iv *ImageView) Flip(horizontal, vertical bool) {
	if horizontal {
		iv.flipH = !iv.flipH
	}
	if vertical {
		iv.flipV = !iv.flipV
	}
	iv.panX, iv.panY = 0, 0
	iv.rebuild()
	iv.toast("mirrored")
}

// Keep the anchor centred, rescaled for the new size.
func (iv *ImageView) SetImage(res ImageResult) {
	if res.Surface == nil || !res.Surface.Valid() {
		return
	}
	prev := iv.display()
	iv.surface = res.Surface
	iv.decoder = res.Decoder
	iv.preview = res.Preview
	iv.decodeDur = res.DecodeDur
	iv.rebuild()
	if prev.Valid() && prev.Width > 0 && prev.Height > 0 {
		if !iv.anchorPending {
			iv.zoomFocusX = iv.panX + float64(iv.visW)/2
			iv.zoomFocusY = iv.panY + float64(iv.visH)/2
		}
		sc := float64(iv.display().Width) / float64(prev.Width)
		iv.zoomFocusX *= sc
		iv.zoomFocusY *= sc
		iv.anchorPending = true
	}
}

func (iv *ImageView) SetPosition(x1, y1, x2, y2 int) {
	iv.ScreenObject.SetPosition(x1, y1, x2, y2)
	if iv.topBar != nil {
		iv.topBar.SetPosition(x1, y1, x2, y1)
	}
}

// ResizeConsole lays the viewer out over the console. In the whole screen
// mode the row that normally belongs to the key bar is taken by the picture.
func (iv *ImageView) ResizeConsole(w, h int) {
	iv.conW, iv.conH = w, h
	bottom := h - 2
	if iv.full {
		bottom = h - 1
	}
	iv.SetPosition(0, 0, w-1, bottom)
}

// SetZoom: 1 means fit; one press, one redraw (terminal too slow to animate).
func (iv *ImageView) SetZoom(z float64) {
	if z < imageViewMinZoom {
		z = imageViewMinZoom
	}
	if z > imageViewMaxZoom {
		z = imageViewMaxZoom
	}
	if !iv.anchorPending {
		iv.zoomFocusX = iv.panX + float64(iv.visW)/2
		iv.zoomFocusY = iv.panY + float64(iv.visH)/2
		iv.anchorPending = true
	}
	iv.zoom = z
	iv.toast(fmt.Sprintf("scale: %d%%", int(iv.zoom*100+0.5)))
}

// Pan moves the visible region by a step of one twentieth of the image.
func (iv *ImageView) Pan(dx, dy int) {
	img := iv.display()
	if !img.Valid() {
		return
	}
	stepX := float64(img.Width) / 20
	stepY := float64(img.Height) / 20
	if stepX < 1 {
		stepX = 1
	}
	if stepY < 1 {
		stepY = 1
	}
	iv.panX += float64(dx) * stepX
	iv.panY += float64(dy) * stepY
	if iv.panX < 0 {
		iv.panX = 0
	}
	if iv.panY < 0 {
		iv.panY = 0
	}
}

func (iv *ImageView) clampPan(visW, visH int) {
	img := iv.display()
	maxX := float64(img.Width - visW)
	maxY := float64(img.Height - visH)
	if maxX < 0 {
		maxX = 0
	}
	if maxY < 0 {
		maxY = 0
	}
	if iv.panX > maxX {
		iv.panX = maxX
	}
	if iv.panY > maxY {
		iv.panY = maxY
	}
	if iv.panX < 0 {
		iv.panX = 0
	}
	if iv.panY < 0 {
		iv.panY = 0
	}
}

// cellSize returns the graphics cell size, with the fallback for backends
// that report none.
func cellSize(scr *vtui.ScreenBuf) (int, int) {
	cw, ch := scr.Graphics().CellSize()
	if cw <= 0 || ch <= 0 {
		return imageViewFallbackCellW, imageViewFallbackCellH
	}
	return cw, ch
}

// placementFor computes where the picture goes: centred when it fits, cropped
// and panned once zoomed past the window.
func (iv *ImageView) placementFor(scr *vtui.ScreenBuf) (vtui.ImagePlacement, bool) {
	if scr == nil {
		return vtui.ImagePlacement{}, false
	}
	cw, ch := cellSize(scr)
	return iv.placementForSize(scr, cw, ch)
}

// placementForSize is placementFor with an explicit cell size, so the block
// renderer (1x2 pixels per cell) and the graphics backend share one path.
func (iv *ImageView) placementForSize(scr *vtui.ScreenBuf, cw, ch int) (vtui.ImagePlacement, bool) {
	img := iv.display()
	if scr == nil || !img.Valid() {
		return vtui.ImagePlacement{}, false
	}

	x1, y1, x2, y2 := iv.GetPosition()
	top := y1 + iv.barHeight()
	cols := x2 - x1 + 1
	rows := y2 - top + 1
	if cols <= 0 || rows <= 0 {
		return vtui.ImagePlacement{}, false
	}

	boxW := cols * cw
	boxH := rows * ch
	scale := iv.baseScale(boxW, boxH) * iv.zoom
	if scale <= 0 {
		return vtui.ImagePlacement{}, false
	}
	iv.lastScale = scale

	dispW := max(1, int(float64(img.Width)*scale+0.5))
	dispH := max(1, int(float64(img.Height)*scale+0.5))

	p := vtui.ImagePlacement{Surface: img}
	if iv.overlay {
		// Under the glyphs but over the cell background: keeps the info panel
		// readable without hiding the picture behind a box.
		p.ZIndex = -1
	}

	if dispW <= boxW && dispH <= boxH {
		iv.panX, iv.panY = 0, 0
		iv.panMaxX, iv.panMaxY = 0, 0
		iv.visW, iv.visH = img.Width, img.Height
		iv.anchorPending = false
		p.Cols, p.Rows = cellsFor(dispW, cw, cols), cellsFor(dispH, ch, rows)
		p.Col = x1 + (cols-p.Cols)/2
		p.Row = top + (rows-p.Rows)/2
		return p, true
	}

	visW := max(1, min(img.Width, int(float64(boxW)/scale)))
	visH := max(1, min(img.Height, int(float64(boxH)/scale)))
	iv.panMaxX = max(0, float64(img.Width-visW))
	iv.panMaxY = max(0, float64(img.Height-visH))
	iv.visW, iv.visH = visW, visH
	if iv.anchorPending {
		iv.panX = iv.zoomFocusX - float64(visW)/2
		iv.panY = iv.zoomFocusY - float64(visH)/2
		iv.anchorPending = false
	}
	iv.clampPan(visW, visH)

	shownW := int(float64(visW)*scale + 0.5)
	shownH := int(float64(visH)*scale + 0.5)
	if shownW > boxW {
		shownW = boxW
	}
	if shownH > boxH {
		shownH = boxH
	}

	p.Cols, p.Rows = cellsFor(shownW, cw, cols), cellsFor(shownH, ch, rows)
	p.Col = x1 + (cols-p.Cols)/2
	p.Row = top + (rows-p.Rows)/2
	p.SrcX, p.SrcY = int(iv.panX), int(iv.panY)
	p.SrcW, p.SrcH = visW, visH
	return p, true
}

// arrow is what the arrow keys do: pan an axis that can move, else walk the
// directory. w/a/s/d always pan.
func (iv *ImageView) arrow(dx, dy int) {
	if dx != 0 && iv.panMaxX > 0 {
		iv.Pan(dx, 0)
		return
	}
	if dy != 0 && iv.panMaxY > 0 {
		iv.Pan(0, dy)
		return
	}
	if dx < 0 || dy < 0 {
		iv.Step(-1)
		return
	}
	iv.Step(1)
}

// titleName prefixes a * to the picked picture, so selection is visible
// without leaving the viewer even without colours.
func (iv *ImageView) titleName() string {
	if iv.selected[iv.path] {
		return "*" + iv.baseName()
	}
	return iv.baseName()
}

// logGeometry records the layout once per change, for debugging a stray
// background strip.
func (iv *ImageView) logGeometry(scr *vtui.ScreenBuf, p vtui.ImagePlacement) {
	img := iv.display()
	if scr == nil || !img.Valid() {
		return
	}
	x1, y1, x2, y2 := iv.GetPosition()
	cw, ch := scr.Graphics().CellSize()
	line := fmt.Sprintf(
		"console=%dx%d frame=%d,%d..%d,%d bar=%d cell=%dx%d img=%dx%d scale=%.4f place=%d,%d %dx%d src=%d,%d %dx%d z=%d layer=%d",
		iv.conW, iv.conH, x1, y1, x2, y2, iv.barHeight(), cw, ch,
		img.Width, img.Height, iv.lastScale,
		p.Col, p.Row, p.Cols, p.Rows, p.SrcX, p.SrcY, p.SrcW, p.SrcH, p.ZIndex,
		scr.Graphics().Len())
	if line == iv.lastGeom {
		return
	}
	iv.lastGeom = line
	vtui.DebugLog("IMAGE_GEOM: %s", line)
}

func cellsFor(pixels, cellSize, limit int) int {
	n := (pixels + cellSize - 1) / cellSize
	if n > limit {
		n = limit
	}
	if n < 1 {
		n = 1
	}
	return n
}

// fitPlacement fits and centres a surface in a cw x ch cell box. The block
// renderer passes 1x2 so its cells and the backend's agree on what fits.
func fitPlacement(surface *vtui.ImageSurface, cw, ch, col, row, boxCols, boxRows int) (vtui.ImagePlacement, bool) {
	if !surface.Valid() || boxCols <= 0 || boxRows <= 0 {
		return vtui.ImagePlacement{}, false
	}
	fw, fh := vtui.FitInside(surface.Width, surface.Height, boxCols*cw, boxRows*ch)
	if fw <= 0 || fh <= 0 {
		return vtui.ImagePlacement{}, false
	}
	p := vtui.ImagePlacement{Surface: surface}
	p.Cols, p.Rows = cellsFor(fw, cw, boxCols), cellsFor(fh, ch, boxRows)
	p.Col = col + (boxCols-p.Cols)/2
	p.Row = row + (boxRows-p.Rows)/2
	return p, true
}

// SetFullScreen gives the title/key-bar rows to the picture. The key bar is
// drawn centrally, so hiding it has to be asked for there.
func (iv *ImageView) SetFullScreen(on bool) {
	if iv.full == on {
		return
	}
	iv.full = on
	AppConfig.ImageFullScreen = on
	RequestSaveConfig()
	vtui.FrameManager.HideBars = on
	if iv.conW > 0 && iv.conH > 0 {
		iv.ResizeConsole(iv.conW, iv.conH)
	}
}

// ToggleOverlay shows or hides the panel that describes the picture.
func (iv *ImageView) ToggleOverlay() {
	iv.overlay = !iv.overlay
	AppConfig.ImageShowOverlay = iv.overlay
	RequestSaveConfig()
	if iv.overlay {
		iv.requestFileSize()
	}
}

// CycleRenderer round-robins the picture renderer: the graphics protocol
// and the half-block cells. A backend without a graphics protocol leaves
// the cycle one stop long, so the half-block cells stay put and the
// setting is not touched. Otherwise the choice is saved and both stations
// alternate, no matter which one the picture uses at the moment.
func (iv *ImageView) CycleRenderer() {
	scr := vtui.FrameManager.Screen()
	if scr == nil || !scr.SupportsGraphics() {
		iv.toast("renderer: half-block")
		return
	}
	mode := 1
	label := "graphics"
	if !imageBlockMode(scr) {
		// Currently the graphics protocol: the next stop is the cells.
		mode = 2
		label = "half-block"
	}
	if AppConfig.ImageBlockRenderer != mode {
		AppConfig.ImageBlockRenderer = mode
		RequestSaveConfig()
	}
	iv.toast("renderer: " + label)
}

// requestFileSize asks the file system how big the file is. Stat can be a
// network round trip, so it runs off the drawing path, once, for the overlay.
func (iv *ImageView) requestFileSize() {
	if iv.sizeKnown || iv.vfs == nil {
		return
	}
	v, path, gen := iv.vfs, iv.path, iv.loadGen
	vtui.RunAsync(func(ctx *vtui.TaskContext) {
		item, err := v.Stat(ctx.Context, path)
		if err != nil {
			return
		}
		ctx.RunOnUI(func() {
			if gen == iv.loadGen {
				iv.fileSize, iv.sizeKnown = item.Size, true
				if !item.MTime.IsZero() {
					iv.fileTime, iv.timeKnown = item.MTime, true
				}
			}
		})
	})
}

// imageOrientationLabel names a turn and mirroring, or nothing.
func imageOrientationLabel(rotation int, flipH, flipV bool) string {
	var parts []string
	if rotation != 0 {
		parts = append(parts, fmt.Sprintf("%d°", rotation))
	}
	if flipH {
		parts = append(parts, "mirror H")
	}
	if flipV {
		parts = append(parts, "mirror V")
	}
	return strings.Join(parts, ", ")
}

// baseName is what the viewer calls the file it shows.
func (iv *ImageView) baseName() string {
	if iv.vfs != nil {
		return iv.vfs.Base(iv.path)
	}
	return filepath.Base(iv.path)
}

// displaySize is the picture dimensions, "1442 x 2160".
func (iv *ImageView) displaySize() string {
	img := iv.display()
	return fmt.Sprintf("%d x %d", img.Width, img.Height)
}

// scalePercent is how much of the picture fits the window, rounded.
func (iv *ImageView) scalePercent() int {
	scale := iv.lastScale
	if scale <= 0 {
		scale = iv.zoom
	}
	return int(scale*100 + 0.5)
}

// positionLabel is "4/683" when the viewer shares its folder with other files.
func (iv *ImageView) positionLabel() string {
	if iv.index >= 0 && len(iv.siblings) > 1 {
		return fmt.Sprintf("%d/%d", iv.index+1, len(iv.siblings))
	}
	return ""
}

// decoderLabel is "go-std 15 ms" in the OSD and titles.
func (iv *ImageView) decoderLabel() string {
	return decoderWithTime(iv.decoder, iv.decodeDur)
}

func decoderWithTime(decoder string, dur time.Duration) string {
	if dur <= 0 {
		return decoder
	}
	return decoder + " " + formatImageDuration(dur)
}

// stateLabel adds the picture's status; ", slideshow" always goes last.
func (iv *ImageView) stateLabel() string {
	state := iv.decoderLabel()
	switch {
	case iv.err != nil:
		state = "error: " + iv.err.Error()
	case iv.loading:
		state += ", decoding"
	case iv.preview:
		state += ", preview"
	}
	if iv.slideStop != nil {
		state += ", slideshow"
	}
	return state
}

// overlayLines is what the info panel has to say about the picture.
func (iv *ImageView) overlayLines() []string {
	size := "unknown size"
	if iv.sizeKnown {
		size = formatSize(iv.fileSize)
	}

	lines := []string{
		iv.baseName(),
		iv.displaySize(),
		size,
	}
	if iv.timeKnown {
		lines = append(lines, iv.fileTime.Format("2006-01-02 15:04"))
	}
	lines = append(lines, iv.decoderLabel())
	if label := imageOrientationLabel(iv.rotation, iv.flipH, iv.flipV); label != "" {
		lines = append(lines, label)
	}
	return lines
}

func formatImageDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func (iv *ImageView) toast(msg string) {
	iv.tempMsg = msg
	iv.tempUntil = time.Now().Add(imageViewToastDelay)
	iv.tempSlideStart = time.Time{}
	iv.tempFlashUntil = time.Time{}
}

// toastClear drops the toast at once, exit animation included.
func (iv *ImageView) toastClear() {
	iv.tempMsg = ""
	iv.tempSlideStart = time.Time{}
	iv.tempFlashUntil = time.Time{}
	iv.stopAnimIfIdle()
}

// flashToast is toast plus a short "wall" flash when a step hits the edge.
func (iv *ImageView) flashToast(msg string) {
	iv.toast(msg)
	iv.tempFlashUntil = time.Now().Add(imageToastFlashDur)
	iv.ensureAnim()
}

// reFlash restarts the wall flash without touching the message.
func (iv *ImageView) reFlash() {
	iv.tempFlashUntil = time.Now().Add(imageToastFlashDur)
	iv.ensureAnim()
}

func (iv *ImageView) tempActive() bool {
	return iv.tempMsg != "" && time.Now().Before(iv.tempUntil)
}

// flashing reports whether the toast keeps its inverted "wall" colours.
func (iv *ImageView) flashing() bool {
	return time.Now().Before(iv.tempFlashUntil)
}

// tempSliding reports whether the toast is in its exit animation.
func (iv *ImageView) tempSliding() bool {
	return !iv.tempSlideStart.IsZero() && time.Since(iv.tempSlideStart) < imageToastSlideDur
}

// ensureAnim starts the redraw ticker that drives the toast animations.
// Idempotent.
func (iv *ImageView) ensureAnim() {
	if iv.animStop != nil {
		return
	}
	stop := make(chan struct{})
	iv.animStop = stop
	go func() {
		ticker := time.NewTicker(imageToastSlideTick)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				vtui.FrameManager.PostTask(func() {
					if iv.animStop == stop {
						vtui.FrameManager.Redraw()
					}
				})
			}
		}
	}()
}

// stopAnimIfIdle ends the redraw ticker, if one is running.
func (iv *ImageView) stopAnimIfIdle() {
	if iv.animStop == nil {
		return
	}
	close(iv.animStop)
	iv.animStop = nil
}

func paintPadded(scr *vtui.ScreenBuf, x, y, limit int, text string, attr uint64) {
	width := runewidth.StringWidth(text)
	if width > limit {
		width = limit
	}
	text = runewidth.Truncate(text, width, "…")
	if w := runewidth.StringWidth(text); w < width {
		text += strings.Repeat(" ", width-w)
	}
	scr.Write(x, y, vtui.StringToCharInfo(text, attr))
}

func (iv *ImageView) drawToastText(scr *vtui.ScreenBuf) {
	x1, _, x2, y2 := iv.GetPosition()
	limit := x2 - x1 - 1
	if limit < 1 {
		limit = 1
	}
	text := " " + iv.tempMsg + " "
	width := runewidth.StringWidth(text)
	if width > limit {
		text = runewidth.Truncate(text, limit, "…")
		width = runewidth.StringWidth(text)
	}
	attr := imageToastAttr
	if iv.flashing() {
		attr = imageWallFlashAttr
	}
	scr.Write(x1+iv.tempSlideOffset(width), y2, vtui.StringToCharInfo(text, attr))
}

// tempSlideOffset is how far the toast has slid left, eased (smoothstep).
func (iv *ImageView) tempSlideOffset(width int) int {
	if !iv.tempSliding() {
		return 0
	}
	p := float64(time.Since(iv.tempSlideStart)) / float64(imageToastSlideDur)
	p = p * p * (3 - 2*p) // smoothstep
	return -int(p * float64(width))
}

// loadingToastShade is one cell of the loading slab. A soft bright comet
// crosses the row once per cycle, ramping from nothing and back so no
// restart is visible. phase is the pass position, [0,1).
func loadingToastShade(x, width int, phase float64) uint32 {
	if width <= 0 {
		return imageSlabDim
	}
	span := float64(width + imageCometHalfWidth)
	// Pass envelope: zero at both ends, one in the middle — the comet fades
	// in from nothing and out into nothing.
	t := phase * span
	env := math.Sin(math.Pi * t / span)
	// Local comet profile: a raised cosine centred on the comet, so the
	// slab brightens into one smooth patch.
	d := float64(x) - t
	prof := 0.0
	if d > -float64(imageCometHalfWidth) && d < float64(imageCometHalfWidth) {
		prof = 0.5 + 0.5*math.Cos(math.Pi*d/float64(imageCometHalfWidth))
	}
	m := env * prof
	base := uint32(imageSlabDim >> 16)
	c := base + uint32(m*float64(imageSlabBright>>16-base))
	return c<<16 | c<<8 | c
}

// loadingToastOn reports whether the loading toast is drawn: a decode at
// work past imageViewDecodeDelay, with no other message in the corner.
func (iv *ImageView) loadingToastOn() bool {
	return iv.loading && !iv.tempActive() && time.Since(iv.decodeStart) > imageViewDecodeDelay
}

// drawLoadingToast shows the decode is still at work: seconds spent over a
// slab a bright band keeps crossing, so a long decode reads as progress.
func (iv *ImageView) drawLoadingToast(scr *vtui.ScreenBuf) {
	x1, _, x2, y2 := iv.GetPosition()
	limit := x2 - x1 - 1
	if limit < 1 {
		limit = 1
	}
	// One clock read drives both the counter and the band phase.
	dur := time.Since(iv.decodeStart)
	text := " decoding " + formatImageDuration(dur) + " "
	text = runewidth.Truncate(text, limit, "…")
	// Phase counts from decodeStart (pinned when loading began), never from
	// the frame clock, so the band cannot sit frozen at its first shape.
	phase := float64(dur%imageLoadingCycle) / float64(imageLoadingCycle)
	iv.loadRow = buildLoadingRow(iv.loadRow[:0], text, phase)
	scr.Write(x1, y2, iv.loadRow)
}

// buildLoadingRow renders one loading-toast frame into row, reusing its
// backing array so a steady decode allocates nothing per frame.
func buildLoadingRow(row []vtui.CharInfo, text string, phase float64) []vtui.CharInfo {
	base := vtui.SetRGBBoth(0, imageLoadingText, imageSlabDim)
	for _, r := range text {
		row = append(row, vtui.CharInfo{Char: uint64(r), Attributes: base})
	}
	hw := float64(imageCometHalfWidth)
	t := phase * float64(len(row)+imageCometHalfWidth)
	for i := range row {
		if d := float64(i) - t; d > -hw && d < hw {
			row[i].Attributes = vtui.SetRGBBoth(0, imageLoadingText, loadingToastShade(i, len(row), phase))
		}
	}
	return row
}

// drawOverlay writes the info panel over the left edge of the picture.
func (iv *ImageView) drawOverlay(scr *vtui.ScreenBuf) {
	lines := iv.overlayLines()
	if scr == nil || len(lines) == 0 {
		return
	}
	x1, y1, x2, y2 := iv.GetPosition()
	top := y1 + iv.barHeight()

	width := 0
	for _, s := range lines {
		if w := runewidth.StringWidth(s); w > width {
			width = w
		}
	}
	width += 2
	if limit := (x2 - x1 + 1) / 2; width > limit {
		width = limit
	}
	rows := len(lines)
	if rows > y2-top {
		rows = y2 - top
	}
	if width <= 0 || rows <= 0 {
		return
	}

	// One slab under all the lines reads as a pane; it inverts with the toast
	// while the wall flash lasts.
	attr := imageOverlayAttr
	if iv.flashing() {
		attr = imageWallFlashAttr
	}
	scr.FillRect(x1, top+1, x1+width-1, top+rows, ' ', attr)
	for i := 0; i < rows; i++ {
		paintPadded(scr, x1, top+1+i, width, " "+lines[i], attr)
	}
}

func (iv *ImageView) Show(scr *vtui.ScreenBuf) {
	iv.ScreenObject.Show(scr)
	if iv.topBar != nil {
		iv.topBar.SetVisible(!iv.full)
		if !iv.full {
			iv.topBar.Show(scr)
		}
	}

	x1, y1, x2, y2 := iv.GetPosition()
	top := y1 + iv.barHeight()
	scr.FillRect(x1, top, x2, y2, ' ', imageViewBackAttr)
	if iv.gal != nil {
		iv.showGallery(scr)
		return
	}

	if imageBlockMode(scr) {
		if p, ok := iv.placementForSize(scr, 1, 2); ok {
			iv.block.draw(scr, p, blockImageBack)
			iv.logGeometry(scr, p)
		}
	} else if !scr.SupportsGraphics() {
		msg := "This backend cannot display images."
		x := x1 + (x2-x1+1-len(msg))/2
		if x < x1 {
			x = x1
		}
		scr.Write(x, (top+y2)/2, vtui.StringToCharInfo(msg, imageViewBackAttr))
		return
	} else {
		p, ok := iv.placementFor(scr)
		if !ok {
			return
		}
		scr.Graphics().DrawImage(iv.gfxKey, p)
		iv.logGeometry(scr, p)
	}
	if iv.overlay {
		iv.drawOverlay(scr)
	}
	iv.toastUpdate()
	if iv.tempMsg != "" {
		iv.drawToastText(scr)
	}
	if iv.loadingToastOn() {
		iv.ensureAnim()
		iv.drawLoadingToast(scr)
	} else if iv.flashing() || iv.tempSliding() {
		iv.ensureAnim() // the wall flash or the exit slide still needs its ticks
	} else {
		iv.stopAnimIfIdle()
	}
}

// toastUpdate advances the toast's life once per frame; Show draws what's left.
func (iv *ImageView) toastUpdate() {
	if iv.tempMsg == "" {
		iv.stopAnimIfIdle()
		return
	}
	if iv.tempActive() || iv.tempSliding() {
		return // still within its window, or still sliding out
	}
	if iv.tempSlideStart.IsZero() {
		// Window over: begin the exit; the ticker keeps the slide moving.
		iv.tempSlideStart = time.Now()
		iv.ensureAnim()
		return
	}
	iv.tempMsg = ""
	iv.stopAnimIfIdle()
}

func (iv *ImageView) ProcessKey(e *vtinput.InputEvent) bool {
	if e == nil || !e.KeyDown {
		return false
	}
	if iv.gal != nil && iv.galleryKey(e) {
		return true
	}

	ctrl := (e.ControlKeyState & (vtinput.LeftCtrlPressed | vtinput.RightCtrlPressed)) != 0
	if ctrl {
		switch e.VirtualKeyCode {
		case vtinput.VK_R:
			iv.Reload()
			return true
		case vtinput.VK_F:
			iv.SetFullScreen(!iv.full)
			return true
		case vtinput.VK_I:
			iv.ToggleOverlay()
			return true
		case vtinput.VK_S:
			iv.ToggleSlideShow()
			return true
		}
		return false
	}

	alt := (e.ControlKeyState & (vtinput.LeftAltPressed | vtinput.RightAltPressed)) != 0
	if alt {
		switch e.Char {
		case '>', '.':
			iv.Flip(true, false)
			return true
		case '<', ',':
			iv.Flip(false, true)
			return true
		}
		return false
	}

	// Shift+F4 round-robins the renderer (swallowed in the gallery so it
	// doesn't fall through to the F4 decoder cycle).
	shift := (e.ControlKeyState & vtinput.ShiftPressed) != 0
	if shift && e.VirtualKeyCode == vtinput.VK_F4 {
		if iv.gal == nil {
			iv.CycleRenderer()
		}
		return true
	}

	switch e.Char {
	case '+', '=', 'e', 'E':
		iv.SetZoom(iv.zoom * imageViewZoomFactor)
		return true
	case '-', '_', 'q', 'Q':
		iv.SetZoom(iv.zoom / imageViewZoomFactor)
		return true
	case '*', '0':
		iv.ToggleActualSize()
		return true
	case '>', '.':
		iv.Rotate(90)
		return true
	case '<', ',':
		iv.Rotate(-90)
		return true
	case ' ':
		iv.Step(1)
		return true
	case 'f', 'F':
		iv.SetFullScreen(!iv.full)
		return true
	case 'i', 'I':
		iv.ToggleOverlay()
		return true
	case 'a', 'A':
		iv.Pan(-1, 0)
		return true
	case 'd', 'D':
		iv.Pan(1, 0)
		return true
	case 'w', 'W':
		iv.Pan(0, -1)
		return true
	case 's', 'S':
		iv.Pan(0, 1)
		return true
	}

	switch e.VirtualKeyCode {
	case vtinput.VK_ESCAPE, vtinput.VK_F10:
		iv.Close()
		return true
	case vtinput.VK_NEXT:
		iv.Step(1)
		return true
	case vtinput.VK_PRIOR, vtinput.VK_BACK:
		iv.Step(-1)
		return true
	case vtinput.VK_HOME:
		if len(iv.siblings) > 0 {
			iv.GoTo(0)
			iv.edgeToast(0, false)
		}
		return true
	case vtinput.VK_END:
		if len(iv.siblings) > 0 {
			iv.GoTo(len(iv.siblings) - 1)
			iv.edgeToast(len(iv.siblings)-1, false)
		}
		return true
	case vtinput.VK_TAB:
		iv.ToggleActualSize()
		return true
	case vtinput.VK_F12:
		iv.ToggleGallery()
		return true
	case vtinput.VK_F4:
		iv.CycleDecoder()
		return true
	case vtinput.VK_INSERT:
		iv.SetSelected(iv.path, !iv.selected[iv.path])
		return true
	case vtinput.VK_DELETE:
		iv.SetSelected(iv.path, false)
		return true
	case vtinput.VK_LEFT:
		iv.arrow(-1, 0)
		return true
	case vtinput.VK_RIGHT:
		iv.arrow(1, 0)
		return true
	case vtinput.VK_UP:
		iv.arrow(0, -1)
		return true
	case vtinput.VK_DOWN:
		iv.arrow(0, 1)
		return true
	}
	return false
}

func (iv *ImageView) HandleCommand(cmd int, args any) bool {
	if cmd == vtui.CmClose {
		iv.Close()
		return true
	}
	return iv.BaseFrame.HandleCommand(cmd, args)
}

func (iv *ImageView) Close() {
	// Full screen is a manager state, so leaving the viewer hands the bars back.
	iv.full = false
	iv.cancelDecode()
	iv.stopSlideShow()
	iv.stopAnimIfIdle()
	vtui.FrameManager.HideBars = false
	iv.BaseFrame.Close()
	// Leave the panel cursor on the picture the viewer stopped at.
	if iv.OnNavigate != nil {
		iv.OnNavigate(iv.path)
	}
	if iv.OnClose != nil {
		iv.OnClose()
	}
}

func (iv *ImageView) GetKeyLabels() *vtui.KeySet {
	return &vtui.KeySet{
		Normal: vtui.KeyBarLabels{
			"", "", "", "", "", "", "", "", "", "Quit",
		},
	}
}

func (iv *ImageView) GetType() vtui.FrameType { return vtui.TypeUser + 7 }

// GetTitle is the full window title (name, size, scale, position, state).
func (iv *ImageView) GetTitle() string {
	parts := []string{iv.titleName(), iv.displaySize(), fmt.Sprintf("%d%%", iv.scalePercent())}
	if rel := iv.positionLabel(); rel != "" {
		parts = append(parts, rel)
	}
	parts = append(parts, iv.stateLabel())
	return strings.Join(parts, "   ")
}
