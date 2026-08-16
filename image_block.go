package main

import (
	"math"
	"runtime"
	"sync"

	"github.com/unxed/vtui"
)

const blockImageBack = 0x101010
const blockHalfChar = '▀'

// blockSerialRows: below this the parallel passes cost more than they save.
const blockSerialRows = 256

// blockWorkersN bounds the pass parallelism (and the pref buffer multiplier).
var blockWorkersN = func() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		return 1
	}
	if n > 16 {
		return 16
	}
	return n
}()

// blockWorkMargin is the working copy's reach past the visible window, as a
// fraction of the window on each side; blockWorkMaxZoom is the highest zoom
// at which cutting cached colors beats re-filtering per pan step.
const (
	blockWorkMargin  = 0.25
	blockWorkMaxZoom = 4.0
)

// Gamma correction LUTs: blending and averaging happen in linear RGB.
var blockSrgbToLinear [256]uint32
var blockLinearToSrgb [65536]uint8

func init() {
	for i := range 256 {
		c := float64(i) / 255.0
		var lin float64
		if c <= 0.04045 {
			lin = c / 12.92
		} else {
			lin = math.Pow((c+0.055)/1.055, 2.4)
		}
		blockSrgbToLinear[i] = uint32(lin*65535.0 + 0.5)
	}
	for i := range 65536 {
		lin := float64(i) / 65535.0
		var srgb float64
		if lin <= 0.0031308 {
			srgb = lin * 12.92
		} else {
			srgb = 1.055*math.Pow(lin, 1.0/2.4) - 0.055
		}
		val := int(srgb*255.0 + 0.5)
		if val > 255 {
			val = 255
		}
		if val < 0 {
			val = 0
		}
		blockLinearToSrgb[i] = uint8(val)
	}
}

type blockRender struct {
	prefR []uint32
	prefG []uint32
	prefB []uint32
	hBuf  []uint16 // linear RGB (3 channels per source row)
	vBuf  []uint32 // packed sRGB, one half-row per entry
	cells []vtui.CharInfo

	// Memo of the last placement: identical geometry re-stamps cached cells.
	// memoPlain is in the key so a flip never serves cells for the other glyph.
	memoPlace vtui.ImagePlacement
	memoBG    uint32
	memoPlain bool
	memoHit   bool

	// hMemo reuses the horizontal pass across a vertical-only pan: hBuf
	// depends on cropX/cropW/cropH/dstW/bg, never on cropY.
	hMemoSurf  *vtui.ImageSurface
	hMemoCropX int
	hMemoCropY int
	hMemoCropW int
	hMemoCropH int
	hMemoDstW  int
	hMemoBG    uint32
	hMemoHit   bool

	// work is the working copy: visible region plus a margin, resampled once;
	// pans inside it cut cached colors.
	workV    []uint32 // packed sRGB half-row colors, workW x workH
	workSurf *vtui.ImageSurface
	workBG   uint32
	workDstW int // destination density the copy was built for
	workDstH int
	workX    int // work source rect
	workY    int
	workWW   int
	workWH   int
	workW    int // work grid size
	workH    int
}

func (r *blockRender) ensureCapacity(srcMax, hBufSize, vBufSize, cellsSize int) {
	if cap(r.prefR) < srcMax {
		r.prefR = make([]uint32, srcMax)
		r.prefG = make([]uint32, srcMax)
		r.prefB = make([]uint32, srcMax)
	} else {
		r.prefR = r.prefR[:srcMax]
		r.prefG = r.prefG[:srcMax]
		r.prefB = r.prefB[:srcMax]
	}
	if cap(r.hBuf) < hBufSize {
		r.hBuf = make([]uint16, hBufSize)
	} else {
		r.hBuf = r.hBuf[:hBufSize]
	}
	if cap(r.vBuf) < vBufSize {
		r.vBuf = make([]uint32, vBufSize)
	} else {
		r.vBuf = r.vBuf[:vBufSize]
	}
	if cap(r.cells) < cellsSize {
		r.cells = make([]vtui.CharInfo, cellsSize)
	} else {
		r.cells = r.cells[:cellsSize]
	}
}

// draw renders the placement with a gamma-correct separable box filter:
// pixels are blended and averaged in linear RGB, then the placement's source
// crop is resampled into its cell rect (one half-block pixel per column, two
// per row). The rect is the canonical image rect worked out by the caller.
func (r *blockRender) draw(scr *vtui.ScreenBuf, p vtui.ImagePlacement, bg uint32) {
	if scr == nil || !p.Surface.Valid() || p.Cols <= 0 || p.Rows <= 0 {
		return
	}

	// Plain cells are a drawing property: resolved here so every consumer
	// gets it for free and the memo key cannot drift.
	plain := blockPlainMode(scr)

	// Static geometry: re-stamp the cached cells (the box is cleared each
	// draw, so they must be re-written) instead of re-filtering. This hot
	// path must stay closure-free: a closure capture would put the placement
	// on the heap on every repeat draw.
	if r.memoHit && r.memoPlace == p && r.memoBG == bg && r.memoPlain == plain {
		for cy := range p.Rows {
			scr.Write(p.Col, p.Row+cy, r.cells[cy*p.Cols:(cy+1)*p.Cols])
		}
		return
	}

	r.drawCold(scr, p, bg, plain)
	r.memoPlace, r.memoBG, r.memoPlain, r.memoHit = p, bg, plain, true
}

func (r *blockRender) drawCold(scr *vtui.ScreenBuf, p vtui.ImagePlacement, bg uint32, plain bool) {
	srcX, srcY, srcW, srcH := p.Source()
	if srcW <= 0 || srcH <= 0 {
		srcX, srcY, srcW, srcH = 0, 0, p.Surface.Width, p.Surface.Height
	}

	// The placement rect is the canonical image rect (one half-block pixel
	// per cell column, two per cell row): resample the crop into it exactly,
	// like the graphics backend stretches the same crop into the same rect.
	// The caller works out the rect (fitPlacement / placementForSize), so no
	// re-fit or padding is done here.
	cols, rows := p.Cols, p.Rows
	dstW, dstH := cols, rows*2

	cropX, cropY, cropW, cropH := srcX, srcY, srcW, srcH

	maxDim := cropW
	if cropH > maxDim {
		maxDim = cropH
	}
	r.ensureCapacity((maxDim+1)*blockWorkersN, dstW*cropH*3, dstW*dstH, cols*rows)

	prefR, prefG, prefB := r.prefR, r.prefG, r.prefB
	hBuf, vBuf, cells := r.hBuf, r.vBuf, r.cells
	prefStride := maxDim + 1
	bgAttr := vtui.SetRGBBoth(0, bg, bg)

	// Working copy: a zoomed placement resamples once and pans cut cached
	// colors (the half-block analogue of the native backends' working copy).
	if cropW < p.Surface.Width || cropH < p.Surface.Height {
		if r.workDraw(p.Surface, bg, cropX, cropY, cropW, cropH, dstW, dstH,
			cells, cols, rows, 0, 0, bgAttr, plain) {
			for cy := range rows {
				scr.Write(p.Col, p.Row+cy, cells[cy*cols:(cy+1)*cols])
			}
			return
		}
	}

	// A vertical-only pan keeps each row's horizontal output: shift the
	// cached hBuf rows and refill only the new ones. Anything else re-runs
	// the pass (keyed on the surface, not its hash — hashing 4K costs more
	// than the pass saves).
	hFresh := !r.hMemoHit || r.hMemoSurf != p.Surface ||
		r.hMemoCropX != cropX || r.hMemoCropW != cropW ||
		r.hMemoCropH != cropH || r.hMemoDstW != dstW || r.hMemoBG != bg
	dy := r.hMemoCropY - cropY
	if hFresh || dy >= cropH || dy <= -cropH {
		r.horizPass(p.Surface, bg, cropX, cropY, cropW, dstW, 0, cropH, hBuf, prefR, prefG, prefB, prefStride)
		r.hMemoSurf, r.hMemoCropX, r.hMemoCropW, r.hMemoCropH = p.Surface, cropX, cropW, cropH
		r.hMemoDstW, r.hMemoBG = dstW, bg
	} else if dy > 0 {
		// The view moved up: cached rows shift down, the top dy are new.
		rowStride := dstW * 3
		copy(hBuf[dy*rowStride:cropH*rowStride], hBuf[:(cropH-dy)*rowStride])
		r.horizPass(p.Surface, bg, cropX, cropY, cropW, dstW, 0, dy, hBuf, prefR, prefG, prefB, prefStride)
	} else if dy < 0 {
		// The view moved down: cached rows shift up, the bottom rows are new.
		n := -dy
		rowStride := dstW * 3
		copy(hBuf[:(cropH-n)*rowStride], hBuf[n*rowStride:cropH*rowStride])
		r.horizPass(p.Surface, bg, cropX, cropY, cropW, dstW, cropH-n, cropH, hBuf, prefR, prefG, prefB, prefStride)
	}
	r.hMemoCropY = cropY
	r.hMemoHit = true

	r.vertPass(cropH, dstW, dstH, hBuf, vBuf, prefR, prefG, prefB, prefStride)
	stampCells(cells, vBuf, dstW, cols, rows, 0, 0, dstW, dstH, bgAttr, plain)

	for cy := range rows {
		scr.Write(p.Col, p.Row+cy, cells[cy*cols:(cy+1)*cols])
	}
}

// horizPass fills hBuf rows [y0, y1) with the horizontal box averages of the
// source rect (cropW columns downsampled to dstW, alpha-blended against bg in
// linear RGB). Rows are independent, so the range splits across workers.
func (r *blockRender) horizPass(surf *vtui.ImageSurface, bg uint32, cropX, cropY, cropW, dstW, y0, y1 int, hBuf []uint16, prefR, prefG, prefB []uint32, prefStride int) {
	stride := surf.Stride
	pix := surf.Pix
	bgLinR := blockSrgbToLinear[byte((bg>>16)&0xFF)]
	bgLinG := blockSrgbToLinear[byte((bg>>8)&0xFF)]
	bgLinB := blockSrgbToLinear[byte(bg&0xFF)]

	rowWork := func(y int, pr, pg, pb []uint32) {
		rowStart := (cropY+y)*stride + cropX*4
		var rSum, gSum, bSum uint32 = 0, 0, 0
		pr[0], pg[0], pb[0] = 0, 0, 0

		if surf.Opaque {
			// Common case (no alpha): straight LUT, no per-pixel branches.
			for x := range cropW {
				o := rowStart + x*4
				rSum += blockSrgbToLinear[pix[o]]
				gSum += blockSrgbToLinear[pix[o+1]]
				bSum += blockSrgbToLinear[pix[o+2]]
				pr[x+1], pg[x+1], pb[x+1] = rSum, gSum, bSum
			}
		} else {
			for x := range cropW {
				o := rowStart + x*4
				a := uint32(pix[o+3])
				var linR, linG, linB uint32
				if a >= 255 {
					linR = blockSrgbToLinear[pix[o]]
					linG = blockSrgbToLinear[pix[o+1]]
					linB = blockSrgbToLinear[pix[o+2]]
				} else if a == 0 {
					linR, linG, linB = bgLinR, bgLinG, bgLinB
				} else {
					invA := 255 - a
					linR = (blockSrgbToLinear[pix[o]]*a + bgLinR*invA) / 255
					linG = (blockSrgbToLinear[pix[o+1]]*a + bgLinG*invA) / 255
					linB = (blockSrgbToLinear[pix[o+2]]*a + bgLinB*invA) / 255
				}
				rSum += linR
				gSum += linG
				bSum += linB
				pr[x+1], pg[x+1], pb[x+1] = rSum, gSum, bSum
			}
		}

		hRowStart := y * dstW * 3
		for cx := range dstW {
			x1 := (cx * cropW) / dstW
			x2 := ((cx + 1) * cropW) / dstW
			if x2 == x1 {
				x2 = x1 + 1
			}
			count := uint32(x2 - x1)
			hBuf[hRowStart+cx*3] = uint16((pr[x2] - pr[x1]) / count)
			hBuf[hRowStart+cx*3+1] = uint16((pg[x2] - pg[x1]) / count)
			hBuf[hRowStart+cx*3+2] = uint16((pb[x2] - pb[x1]) / count)
		}
	}

	n := y1 - y0
	if n <= 0 {
		return
	}
	workers := blockWorkersN
	if n < blockSerialRows {
		workers = 1
	}
	if workers == 1 {
		pr, pg, pb := prefR[:prefStride], prefG[:prefStride], prefB[:prefStride]
		for y := y0; y < y1; y++ {
			rowWork(y, pr, pg, pb)
		}
		return
	}
	var wg sync.WaitGroup
	rowsPer := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		wy0 := y0 + w*rowsPer
		wy1 := wy0 + rowsPer
		if wy1 > y1 {
			wy1 = y1
		}
		if wy0 >= wy1 {
			continue
		}
		off := w * prefStride
		wg.Add(1)
		go func(wy0, wy1 int, pr, pg, pb []uint32) {
			defer wg.Done()
			for y := wy0; y < wy1; y++ {
				rowWork(y, pr, pg, pb)
			}
		}(wy0, wy1, prefR[off:off+prefStride], prefG[off:off+prefStride], prefB[off:off+prefStride])
	}
	wg.Wait()
}

// vertPass averages the horizontal output into the packed sRGB half-row colors
// (dstW x dstH). Serial was measured equal to parallel with fewer allocs.
func (r *blockRender) vertPass(cropH, dstW, dstH int, hBuf []uint16, vBuf []uint32, prefR, prefG, prefB []uint32, prefStride int) {
	colWork := func(cx int, pr, pg, pb []uint32) {
		var rSum, gSum, bSum uint32 = 0, 0, 0
		pr[0], pg[0], pb[0] = 0, 0, 0

		for y := range cropH {
			idx := y*dstW*3 + cx*3
			rSum += uint32(hBuf[idx])
			gSum += uint32(hBuf[idx+1])
			bSum += uint32(hBuf[idx+2])
			pr[y+1], pg[y+1], pb[y+1] = rSum, gSum, bSum
		}

		for cy := range dstH {
			y1 := (cy * cropH) / dstH
			y2 := ((cy + 1) * cropH) / dstH
			if y2 == y1 {
				y2 = y1 + 1
			}
			count := uint32(y2 - y1)
			r := blockLinearToSrgb[(pr[y2]-pr[y1])/count]
			g := blockLinearToSrgb[(pg[y2]-pg[y1])/count]
			b := blockLinearToSrgb[(pb[y2]-pb[y1])/count]
			vBuf[cy*dstW+cx] = (uint32(r) << 16) | (uint32(g) << 8) | uint32(b)
		}
	}

	pr, pg, pb := prefR[:prefStride], prefG[:prefStride], prefB[:prefStride]
	for cx := range dstW {
		colWork(cx, pr, pg, pb)
	}
}

// stampCells pads the whole box with bg and stamps the image's half-row
// colors (stride-separated, dstW x dstH) into the grid. In plain mode every
// cell is a space with one colour — the linear average of its two halves —
// so no block glyph is needed.
func stampCells(cells []vtui.CharInfo, colors []uint32, stride, cols, rows, offsetX, offsetYCells, dstW, dstH int, bgAttr uint64, plain bool) {
	for i := range cols * rows {
		cells[i] = vtui.CharInfo{Char: ' ', Attributes: bgAttr}
	}

	for cy := range dstH / 2 {
		topRow := cy * 2 * stride
		botRow := (cy*2 + 1) * stride
		cellRowStart := (cy+offsetYCells)*cols + offsetX
		for cx := range dstW {
			fg := colors[topRow+cx]
			bgc := colors[botRow+cx]
			if plain {
				blended := blockBlend(fg, bgc)
				cells[cellRowStart+cx] = vtui.CharInfo{Char: ' ', Attributes: vtui.SetRGBBoth(0, blended, blended)}
				continue
			}
			ch := uint64(blockHalfChar)
			if fg == bgc {
				ch = ' '
			}
			cells[cellRowStart+cx] = vtui.CharInfo{Char: ch, Attributes: vtui.SetRGBBoth(0, fg, bgc)}
		}
	}
}

// blockBlend merges the two half-row colors into the plain cell's single
// colour, averaged in linear RGB like the filters, so brightness stays
// correct without a glyph.
func blockBlend(a, b uint32) uint32 {
	linR := (blockSrgbToLinear[byte((a>>16)&0xFF)] + blockSrgbToLinear[byte((b>>16)&0xFF)]) / 2
	linG := (blockSrgbToLinear[byte((a>>8)&0xFF)] + blockSrgbToLinear[byte((b>>8)&0xFF)]) / 2
	linB := (blockSrgbToLinear[byte(a&0xFF)] + blockSrgbToLinear[byte(b&0xFF)]) / 2
	return uint32(blockLinearToSrgb[linR])<<16 | uint32(blockLinearToSrgb[linG])<<8 | uint32(blockLinearToSrgb[linB])
}

// workDraw serves a zoomed placement from the working copy: pans inside the
// margin cut cached colors (snapped to the copy's grid) instead of
// re-filtering. Returns false to fall through to the ordinary filter.
func (r *blockRender) workDraw(surf *vtui.ImageSurface, bg uint32, cropX, cropY, cropW, cropH, dstW, dstH int, cells []vtui.CharInfo, cols, rows, offsetX, offsetYCells int, bgAttr uint64, plain bool) bool {
	// Beyond this zoom a pan outruns the margin; hMemo stays the fallback.
	if zoom := float64(surf.Width) / float64(cropW); zoom > blockWorkMaxZoom {
		return false
	}
	mx := int(float64(cropW)*blockWorkMargin + 0.5)
	my := int(float64(cropH)*blockWorkMargin + 0.5)
	if mx < 1 || my < 1 {
		return false
	}
	wx := cropX - mx
	if wx < 0 {
		wx = 0
	}
	wy := cropY - my
	if wy < 0 {
		wy = 0
	}
	wx1 := cropX + cropW + mx
	if wx1 > surf.Width {
		wx1 = surf.Width
	}
	wy1 := cropY + cropH + my
	if wy1 > surf.Height {
		wy1 = surf.Height
	}
	if wx1-wx <= cropW && wy1-wy <= cropH {
		return false
	}

	if r.workSurf != surf || r.workBG != bg || r.workDstW != dstW || r.workDstH != dstH ||
		wx < r.workX || cropX+cropW > r.workX+r.workWW ||
		wy < r.workY || cropY+cropH > r.workY+r.workWH {
		r.buildWork(surf, bg, wx, wy, wx1-wx, wy1-wy, cropW, cropH, dstW, dstH)
	}

	offX := int(float64(cropX-r.workX)*float64(r.workW)/float64(r.workWW) + 0.5)
	offY := int(float64(cropY-r.workY)*float64(r.workH)/float64(r.workWH) + 0.5)
	if offX+dstW > r.workW {
		offX = r.workW - dstW
	}
	if offY+dstH > r.workH {
		offY = r.workH - dstH
	}
	if offX < 0 || offY < 0 {
		return false
	}
	stampCells(cells, r.workV[offY*r.workW+offX:], r.workW, cols, rows, offsetX, offsetYCells, dstW, dstH, bgAttr, plain)
	return true
}

// buildWork resamples the region around the visible rect once, at the visible
// density plus a margin, into the cached work colors.
func (r *blockRender) buildWork(surf *vtui.ImageSurface, bg uint32, wx, wy, ww, wh, cropW, cropH, dstW, dstH int) {
	workW := max(1, int(float64(ww)*float64(dstW)/float64(cropW)+0.5))
	workH := max(1, int(float64(wh)*float64(dstH)/float64(cropH)+0.5))
	maxDim := max(ww, wh)
	r.ensureCapacity((maxDim+1)*blockWorkersN, workW*wh*3, workW*workH, 0)
	if cap(r.workV) < workW*workH {
		r.workV = make([]uint32, workW*workH)
	} else {
		r.workV = r.workV[:workW*workH]
	}
	prefStride := maxDim + 1
	r.horizPass(surf, bg, wx, wy, ww, workW, 0, wh, r.hBuf, r.prefR, r.prefG, r.prefB, prefStride)
	r.vertPass(wh, workW, workH, r.hBuf, r.workV, r.prefR, r.prefG, r.prefB, prefStride)
	r.workSurf, r.workBG, r.workDstW, r.workDstH = surf, bg, dstW, dstH
	r.workX, r.workY, r.workWW, r.workWH = wx, wy, ww, wh
	r.workW, r.workH = workW, workH
	// hBuf/vBuf now describe the work rect, not the visible one: record it
	// so the visible path cannot reuse the wrong horizontal pass.
	r.hMemoSurf, r.hMemoCropX, r.hMemoCropW, r.hMemoCropH = surf, wx, ww, wh
	r.hMemoDstW, r.hMemoBG = workW, bg
	r.hMemoCropY = wy
	r.hMemoHit = true
}

// imageBlockMode reports whether half-block rendering must be used:
// renderer 0 forces cells off, 2 forces them on, 1 (default) falls back to
// cells only when the terminal cannot draw images.
func imageBlockMode(scr *vtui.ScreenBuf) bool {
	switch AppConfig.ImageBlockRenderer {
	case 0:
		return false
	case 2:
		return true
	}
	return scr != nil && !scr.SupportsGraphics()
}

// blockPlainMode reports whether the block renderer must use plain cells
// (spaces, one colour) instead of the half-block glyph: a 16-colour console's
// font cannot be trusted to carry the half-block at all.
func blockPlainMode(scr *vtui.ScreenBuf) bool {
	return scr != nil && scr.ColorProfile == vtui.ColorProfile16
}
