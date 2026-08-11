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

	// Memo of the last drawn placement: identical geometry re-stamps the
	// cached cells instead of re-running the separable filter.
	memoPlace vtui.ImagePlacement
	memoBG    uint32
	memoCW    int
	memoCH    int
	memoHit   bool
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
// pixels are blended and averaged in linear RGB, then the source is fitted
// into the grid ("contain"), centred and padded with bg. Never crops.
func (r *blockRender) draw(scr *vtui.ScreenBuf, p vtui.ImagePlacement, bg uint32) {
	if scr == nil || !p.Surface.Valid() || p.Cols <= 0 || p.Rows <= 0 {
		return
	}

	cw, ch := scr.Graphics().CellSize()
	if cw <= 0 || ch <= 0 {
		cw, ch = 10, 20 // Fallback 1:2
	}

	// Static geometry: skip both filter passes, re-stamp the cached cells.
	// The caller clears the box before draw, so cells must be re-written.
	// This hot path must stay closure-free: any closure capture would put
	// the placement on the heap on every repeat draw.
	if r.memoHit && r.memoPlace == p && r.memoBG == bg && r.memoCW == cw && r.memoCH == ch {
		for cy := range p.Rows {
			scr.Write(p.Col, p.Row+cy, r.cells[cy*p.Cols:(cy+1)*p.Cols])
		}
		return
	}

	r.drawCold(scr, p, bg, cw, ch)
	r.memoPlace, r.memoBG, r.memoCW, r.memoCH, r.memoHit = p, bg, cw, ch, true
}

func (r *blockRender) drawCold(scr *vtui.ScreenBuf, p vtui.ImagePlacement, bg uint32, cw, ch int) {
	srcX, srcY, srcW, srcH := p.Source()
	if srcW <= 0 || srcH <= 0 {
		srcX, srcY, srcW, srcH = 0, 0, p.Surface.Width, p.Surface.Height
	}

	cols, rows := p.Cols, p.Rows
	var dstW, dstH int

	// Contain math in int64: compare srcW/srcH with (cols*cw)/(rows*ch),
	// then fit the source into the grid without cropping.
	if int64(srcW)*int64(rows)*int64(ch) > int64(srcH)*int64(cols)*int64(cw) {
		dstW = cols
		dstH = int((int64(dstW) * int64(cw) * int64(srcH) * 2) / (int64(ch) * int64(srcW)))
	} else {
		dstH = rows * 2
		dstW = int((int64(dstH) * int64(ch) * int64(srcW)) / (int64(cw) * int64(srcH) * 2))
	}

	// Clamp and keep dstH even so every half-row has a pair for the ▀ glyph.
	if dstW < 1 {
		dstW = 1
	}
	if dstW > cols {
		dstW = cols
	}
	if dstH < 2 {
		dstH = 2
	}
	if dstH > rows*2 {
		dstH = rows * 2
	}
	if dstH%2 != 0 {
		dstH--
	}

	offsetX := (cols - dstW) / 2
	offsetYCells := (rows - dstH/2) / 2

	// The whole requested source rect is used; nothing is cropped.
	cropX, cropY, cropW, cropH := srcX, srcY, srcW, srcH

	stride := p.Surface.Stride
	maxDim := cropW
	if cropH > maxDim {
		maxDim = cropH
	}
	r.ensureCapacity((maxDim+1)*blockWorkersN, dstW*cropH*3, dstW*dstH, cols*rows)

	prefR, prefG, prefB := r.prefR, r.prefG, r.prefB
	hBuf, vBuf, cells := r.hBuf, r.vBuf, r.cells
	prefStride := maxDim + 1 // one prefix bank per worker

	bgR := byte((bg >> 16) & 0xFF)
	bgG := byte((bg >> 8) & 0xFF)
	bgB := byte(bg & 0xFF)
	bgLinR := blockSrgbToLinear[bgR]
	bgLinG := blockSrgbToLinear[bgG]
	bgLinB := blockSrgbToLinear[bgB]

	// Horizontal pass: alpha-blend and average in linear RGB. Rows are
	// independent, so split cropH across workers, one prefix bank each.
	rowWork := func(y int, pr, pg, pb []uint32) {
		rowStart := (cropY+y)*stride + cropX*4
		var rSum, gSum, bSum uint32 = 0, 0, 0
		pr[0], pg[0], pb[0] = 0, 0, 0
		pix := p.Surface.Pix

		if p.Surface.Opaque {
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

	workers := blockWorkersN
	if cropH < blockSerialRows {
		workers = 1
	}
	if workers == 1 {
		pr, pg, pb := prefR[:prefStride], prefG[:prefStride], prefB[:prefStride]
		for y := range cropH {
			rowWork(y, pr, pg, pb)
		}
	} else {
		var wg sync.WaitGroup
		rowsPer := (cropH + workers - 1) / workers
		for w := 0; w < workers; w++ {
			y0 := w * rowsPer
			y1 := y0 + rowsPer
			if y1 > cropH {
				y1 = cropH
			}
			if y0 >= y1 {
				continue
			}
			off := w * prefStride
			wg.Add(1)
			go func(y0, y1 int, pr, pg, pb []uint32) {
				defer wg.Done()
				for y := y0; y < y1; y++ {
					rowWork(y, pr, pg, pb)
				}
			}(y0, y1, prefR[off:off+prefStride], prefG[off:off+prefStride], prefB[off:off+prefStride])
		}
		wg.Wait()
	}

	// Vertical pass: average and convert back to sRGB. Columns are
	// independent; serial was measured ≈ parallel (the pass is small
	// vs the horizontal one) with fewer allocs.
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

	// Padding: clear the whole box with bg, then stamp the image in.
	bgAttr := vtui.SetRGBBoth(0, bg, bg)
	for i := range cols * rows {
		cells[i] = vtui.CharInfo{Char: ' ', Attributes: bgAttr}
	}

	for cy := range dstH / 2 {
		topRow := cy * 2 * dstW
		botRow := (cy*2 + 1) * dstW
		cellRowStart := (cy+offsetYCells)*cols + offsetX
		for cx := range dstW {
			fg := vBuf[topRow+cx]
			bgc := vBuf[botRow+cx]
			ch := uint64(blockHalfChar)
			if fg == bgc {
				ch = ' '
			}
			cells[cellRowStart+cx] = vtui.CharInfo{Char: ch, Attributes: vtui.SetRGBBoth(0, fg, bgc)}
		}
	}

	for cy := range rows {
		scr.Write(p.Col, p.Row+cy, cells[cy*cols:(cy+1)*cols])
	}
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
