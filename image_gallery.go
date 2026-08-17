package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mattn/go-runewidth"
	"github.com/unxed/f4/imagedec"
	"github.com/unxed/f4/vfs"
	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

// The F12 thumbnail grid. A tile is measured in cells and gives its bottom
// row to the file name; sized to fit an ordinary terminal as a grid while an
// Exif thumbnail stays recognisable.
const (
	imageTileCols = 18
	imageTileRows = 9

	// galleryThumbBudget bounds how many tile decodes one frame starts, so a
	// screenful doesn't flood the pipeline; the nearest tiles go first.
	galleryThumbBudget = 12
)

var (
	imageTileNameAttr   = vtui.SetRGBBoth(0, 0xC0C0C0, 0x101010)
	imageTileCursorAttr = vtui.SetRGBBoth(0, 0x101010, 0xC0C0C0)
	imageTilePickedAttr = vtui.SetRGBBoth(0, 0xFFFF00, 0x101010)
)

// imageGallery is the grid's state: cursor, top row, and arrived thumbnails.
// It knows nothing about files — the viewer owns the list.
type imageGallery struct {
	cursor int
	top    int
	cols   int
	rows   int
	tw     int // tile size in pixels
	th     int
	thumbs map[string]*vtui.ImageSurface
	asked  map[string]bool
	// in-flight tile decodes, so leaving the grid cancels them.
	cancels map[string]func()
}

// layout works out how many tiles fit into the window.
func (g *imageGallery) layout(cols, rows int) {
	g.cols = cols / imageTileCols
	g.rows = rows / imageTileRows
	if g.cols < 1 {
		g.cols = 1
	}
	if g.rows < 1 {
		g.rows = 1
	}
}

// step is how far one press of the up or down arrow moves.
func (g *imageGallery) step() int {
	if g.cols < 1 {
		return 1
	}
	return g.cols
}

// page is how many rows of tiles one press of PgUp or PgDn moves.
func (g *imageGallery) page() int {
	if g.rows < 1 {
		return 1
	}
	return g.rows
}

// move walks the grid, stopping at both ends rather than wrapping.
func (g *imageGallery) move(delta, total int) {
	if total <= 0 {
		return
	}
	g.cursor += delta
	if g.cursor < 0 {
		g.cursor = 0
	}
	if g.cursor >= total {
		g.cursor = total - 1
	}
}

// scrollTo brings the row the cursor sits on into view.
func (g *imageGallery) scrollTo(idx, total int) {
	cols, rows := g.step(), g.page()
	row := idx / cols
	if row < g.top {
		g.top = row
	}
	if row >= g.top+rows {
		g.top = row - rows + 1
	}
	if last := (total + cols - 1) / cols; g.top > last-rows {
		g.top = last - rows
	}
	if g.top < 0 {
		g.top = 0
	}
}

// ToggleGallery switches between one picture and the grid, opening on the
// picture that was on screen.
func (iv *ImageView) ToggleGallery() {
	// A grid and a slide show can't both own the current picture.
	iv.stopSlideShow()
	iv.stopAnimIfIdle()
	if iv.gal != nil {
		iv.stopGallery()
		return
	}
	cursor := iv.index
	if cursor < 0 {
		cursor = 0
	}
	iv.gal = &imageGallery{
		cursor:  cursor,
		cols:    1,
		rows:    1,
		thumbs:  make(map[string]*vtui.ImageSurface),
		asked:   make(map[string]bool),
		cancels: make(map[string]func()),
	}
	if iv.blockTiles == nil {
		iv.blockTiles = make(map[int]*blockRender)
	}
	iv.identifyAll()
}

// galleryPath is the picture under the grid cursor.
func (iv *ImageView) galleryPath() string {
	if iv.gal == nil || iv.gal.cursor < 0 || iv.gal.cursor >= len(iv.siblings) {
		return ""
	}
	return iv.siblings[iv.gal.cursor]
}

// SetSelection replaces the set of picked pictures without telling anybody:
// it is how the panel hands over what it had picked before the viewer opened.
func (iv *ImageView) SetSelection(picked map[string]bool) {
	iv.selected = make(map[string]bool, len(picked))
	for path := range picked {
		iv.selected[path] = true
	}
}

// SetSelected picks or unpicks one picture and forwards the change.
func (iv *ImageView) SetSelected(path string, on bool) {
	if path == "" {
		return
	}
	if iv.selected == nil {
		iv.selected = make(map[string]bool)
	}
	if on {
		iv.selected[path] = true
	} else {
		delete(iv.selected, path)
	}
	if iv.OnSelect != nil {
		iv.OnSelect(path, on)
	}
}

// requestVisibleThumbs asks for the visible thumbnails, closest to the cursor
// first, at most galleryThumbBudget per frame; already-asked tiles are
// skipped, so frames walk the screen in rings around the cursor.
func (iv *ImageView) requestVisibleThumbs() {
	g := iv.gal
	if g == nil {
		return
	}
	total := len(iv.siblings)
	if total == 0 {
		return
	}
	first := g.top * g.cols
	last := first + g.cols*g.rows
	if last > total {
		last = total
	}

	type slot struct {
		idx  int
		dist int
	}
	visible := make([]slot, 0, last-first)
	for idx := first; idx < last; idx++ {
		if g.asked[iv.siblings[idx]] {
			continue
		}
		visible = append(visible, slot{idx, galleryRing(g, idx)})
	}
	sort.SliceStable(visible, func(i, j int) bool {
		return visible[i].dist < visible[j].dist
	})
	if len(visible) > galleryThumbBudget {
		visible = visible[:galleryThumbBudget]
	}
	for _, s := range visible {
		iv.requestThumb(iv.siblings[s.idx])
	}
}

// galleryRing is a slot's distance from the cursor; ring order fills a fresh
// grid around it.
func galleryRing(g *imageGallery, idx int) int {
	cols := g.cols
	if cols < 1 {
		cols = 1
	}
	dx, dy := idx%cols-g.cursor%cols, idx/cols-g.cursor/cols
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	if dx > dy {
		return dx
	}
	return dy
}

// galleryCanDecode reports whether a tile is worth decoding in the grid.
// Videos and shell-only formats (SVG, EMF/WMF) are skipped: the Windows
// shell thumbnailer can block for a long time on them, and a folder of mixed
// files saturates the pipeline workers until the grid stops answering. Such
// tiles show only their caption.
func galleryCanDecode(path string) bool {
	if imagedec.IsVideoFile(path) {
		return false
	}
	for _, d := range imagedec.ImageDecodersFor(path) {
		if d.Name != "shell" {
			return true
		}
	}
	return false
}

// requestThumb decodes one thumbnail off the drawing path.
func (iv *ImageView) requestThumb(path string) {
	g := iv.gal
	if g == nil || g.asked[path] {
		return
	}
	g.asked[path] = true
	if !galleryCanDecode(path) {
		return
	}

	v := iv.vfs
	task := vtui.RunAsync(func(ctx *vtui.TaskContext) {
		res, ok := ImagePipe.PreviewSync(ctx.Context, v, path)
		if !ok {
			// No thumbnail: converters shrink RAW/AVIF, no full decode per tile.
			res = loadGalleryTile(ctx.Context, v, path, g.tw, g.th)
			if res.Err != nil {
				return
			}
		}
		surface := res.Surface
		ctx.RunOnUI(func() {
			if iv.gal == g && surface != nil && surface.Valid() {
				g.thumbs[path] = surface
			}
			delete(g.cancels, path)
		})
	})
	g.cancels[path] = task.Cancel
}

// showGallery paints the grid over the area the picture would have taken.
func (iv *ImageView) showGallery(scr *vtui.ScreenBuf) {
	g := iv.gal
	if g == nil || scr == nil {
		return
	}
	x1, y1, x2, y2 := iv.GetPosition()
	top := y1

	g.layout(x2-x1+1, y2-top+1)
	total := len(iv.siblings)
	if total == 0 {
		return
	}
	g.move(0, total)
	g.scrollTo(g.cursor, total)

	// A frame's worth of decodes, nearest the cursor first.
	iv.requestVisibleThumbs()

	cw, ch := cellSize(scr)
	g.tw, g.th = (imageTileCols-2)*cw, (imageTileRows-2)*ch

	first := g.top * g.cols
	for slot := 0; slot < g.cols*g.rows; slot++ {
		idx := first + slot
		if idx >= total {
			break
		}
		iv.showTile(scr, slot, idx,
			x1+(slot%g.cols)*imageTileCols,
			top+(slot/g.cols)*imageTileRows)
	}
}

// showTile paints one thumbnail and the caption under it.
func (iv *ImageView) showTile(scr *vtui.ScreenBuf, slot, idx, col, row int) {
	path := iv.siblings[idx]
	name := filepath.Base(path)
	if iv.vfs != nil {
		name = iv.vfs.Base(path)
	}

	attr := imageTileNameAttr
	switch {
	case idx == iv.gal.cursor:
		attr = imageTileCursorAttr
	case iv.selected[path]:
		attr = imageTilePickedAttr
	}

	caption := runewidth.Truncate(" "+name, imageTileCols, "…")
	if w := runewidth.StringWidth(caption); w < imageTileCols {
		caption += strings.Repeat(" ", imageTileCols-w)
	}
	scr.Write(col, row+imageTileRows-1, vtui.StringToCharInfo(caption, attr))

	// The header pass knows the size before the thumbnail decodes; a rotated
	// picture (5-8) shows swapped sides, like its decoded surface.
	if h, ok := ImagePipe.Identified(iv.vfs, path); ok {
		w, ht := h.Width, h.Height
		if h.Orientation >= 5 && h.Orientation <= 8 {
			w, ht = ht, w
		}
		dims := runewidth.Truncate(" "+fmt.Sprintf("%dx%d", w, ht), imageTileCols, "…")
		scr.Write(col, row+imageTileRows-2, vtui.StringToCharInfo(dims, imageTileNameAttr))
	}

	surface := iv.gal.thumbs[path]
	if surface == nil || !surface.Valid() {
		// Not requested yet: the per-frame budget decides what is asked.
		return
	}

	boxCols, boxRows := imageTileCols-2, imageTileRows-2

	if imageBlockMode(scr) {
		if p, ok := fitPlacement(surface, col+1, row, boxCols, boxRows); ok {
			c := iv.blockTiles[slot]
			if c == nil {
				c = &blockRender{}
				iv.blockTiles[slot] = c
			}
			c.draw(scr, p, blockImageBack)
		}
		return
	}
	if !scr.SupportsGraphics() {
		return
	}

	if p, ok := fitPlacement(surface, col+1, row, boxCols, boxRows); ok {
		scr.Graphics().DrawImage(fmt.Sprintf("%s#%d", iv.gfxKey, idx), p)
	}
}

// galleryKey handles the grid. Anything it does not know falls through to the
// ordinary viewer keys.
func (iv *ImageView) galleryKey(e *vtinput.InputEvent) bool {
	g := iv.gal
	if g == nil {
		return false
	}
	if (e.ControlKeyState & (vtinput.LeftCtrlPressed | vtinput.RightCtrlPressed |
		vtinput.LeftAltPressed | vtinput.RightAltPressed)) != 0 {
		return false
	}
	total := len(iv.siblings)

	switch e.Char {
	case 'a', 'A':
		g.move(-1, total)
		return true
	case 'd', 'D', ' ':
		g.move(1, total)
		return true
	case 'w', 'W':
		g.move(-g.step(), total)
		return true
	case 's', 'S':
		g.move(g.step(), total)
		return true
	}

	switch e.VirtualKeyCode {
	case vtinput.VK_F12, vtinput.VK_ESCAPE:
		iv.ToggleGallery()
		return true
	case vtinput.VK_RETURN:
		idx := g.cursor
		iv.ToggleGallery()
		iv.GoTo(idx)
		return true
	case vtinput.VK_LEFT:
		g.move(-1, total)
		return true
	case vtinput.VK_RIGHT:
		g.move(1, total)
		return true
	case vtinput.VK_UP:
		g.move(-g.step(), total)
		return true
	case vtinput.VK_DOWN:
		g.move(g.step(), total)
		return true
	case vtinput.VK_PRIOR:
		g.move(-g.step()*g.page(), total)
		return true
	case vtinput.VK_NEXT:
		g.move(g.step()*g.page(), total)
		return true
	case vtinput.VK_HOME:
		g.move(-total, total)
		return true
	case vtinput.VK_END:
		g.move(total, total)
		return true
	case vtinput.VK_INSERT:
		path := iv.galleryPath()
		iv.SetSelected(path, !iv.selected[path])
		g.move(1, total)
		return true
	case vtinput.VK_DELETE:
		iv.SetSelected(iv.galleryPath(), false)
		g.move(1, total)
		return true
	}
	return false
}

// loadGalleryTile decodes one thumbnail off the drawing path: a decoder that
// can shrink while reading (WIC) or a converter serves the tile without a
// full decode; everything else decodes whole.
func loadGalleryTile(ctx context.Context, v vfs.VFS, path string, w, h int) ImageResult {
	if w < 1 {
		w = (imageTileCols - 2) * imageViewFallbackCellW
	}
	if h < 1 {
		h = (imageTileRows - 2) * imageViewFallbackCellH
	}
	decoders := imagedec.ImageDecodersFor(path)
	// A video renders only from a real path; never hand its bytes to a size
	// decoder or converter, which would read the whole container. The shell
	// frame fetch can also block for a long time, so a tile is never worth
	// it: the grid shows the caption and moves on.
	if imagedec.IsVideoFile(path) {
		return ImageResult{Path: path, Err: imagedec.ErrVideoPreviewUnavailable}
	}
	if len(decoders) == 0 {
		return ImagePipe.LoadTileSync(ctx, v, path)
	}
	if d := decoders[0]; d.DecodeSize != nil {
		if data, err := ImagePipe.FileBytes(ctx, v, path); err == nil {
			if surf, err := d.DecodeSize(ctx, path, data, w, h); err == nil && surf != nil && surf.Valid() {
				return ImageResult{Path: path, Surface: surf, Decoder: d.Name}
			}
		}
	}
	if decoders[0].Name == imagedec.ExternalImageDecoder {
		surf, decoder, err := loadImageScaled(ctx, v, path, w, h)
		if err != nil {
			return ImagePipe.LoadTileSync(ctx, v, path)
		}
		return ImageResult{Path: path, Surface: surf, Decoder: decoder}
	}
	res := ImagePipe.LoadTileSync(ctx, v, path)
	// A full decode for a tile happens once: downscale it for the grid.
	if res.Surface.Valid() && (res.Surface.Width > w || res.Surface.Height > h) {
		if tile := vtui.ScaleSurface(res.Surface, w, h); tile != nil {
			return ImageResult{Path: path, Surface: tile, Decoder: res.Decoder}
		}
	}
	return res
}

// loadImageScaled lets a converter shrink RAW/AVIF while reading: never a
// full decode for a tile.
func loadImageScaled(ctx context.Context, v vfs.VFS, path string, w, h int) (*vtui.ImageSurface, string, error) {
	data, err := ImagePipe.FileBytes(ctx, v, path)
	if err != nil {
		return nil, "", err
	}
	tool, ok := imagedec.ExternalImageToolFor(data)
	if !ok {
		return nil, "", fmt.Errorf("no external image converter on the PATH")
	}
	surf, err := imagedec.DecodeImageExternallyScaled(ctx, tool, data, w, h)
	if err != nil {
		return nil, "", err
	}
	return surf, tool.Label, nil
}
