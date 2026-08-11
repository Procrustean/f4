package main

// Kept for comparison: the pre-replacement (legacy) sRGB-cover box filter,
// benchmarked against the gamma-correct contain renderer in image_block.go.

import (
	"testing"

	"github.com/unxed/vtui"
)

// legacyBlockRender is the pre-replacement renderer: averages in sRGB,
// cover-crops the source to the box aspect, blends with +127 rounding.
type legacyBlockRender struct {
	prefR []uint32
	prefG []uint32
	prefB []uint32
	hBuf  []byte
	vBuf  []uint32
	cells []vtui.CharInfo
}

func (r *legacyBlockRender) ensureCapacity(srcMax, hBufSize, vBufSize, cellsSize int) {
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
		r.hBuf = make([]byte, hBufSize)
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

func legacyCropRect(p vtui.ImagePlacement, cw, ch int) (cropW, cropH, cropX, cropY int) {
	cols := p.Cols
	rows := p.Rows
	if cols <= 0 {
		cols = 1
	}
	if rows <= 0 {
		rows = 1
	}
	boxAspect := float64(cols*cw) / float64(rows*ch)
	cropX, cropY = p.SrcX, p.SrcY
	if p.SrcW > 0 {
		cropW = p.SrcW
	} else {
		cropW = p.Surface.Width
	}
	if p.SrcH > 0 {
		cropH = p.SrcH
	} else {
		cropH = p.Surface.Height
	}
	srcAspect := float64(cropW) / float64(cropH)
	if srcAspect > boxAspect {
		newW := int(float64(cropH)*boxAspect + 0.5)
		if newW < 1 {
			newW = 1
		}
		cropX += (cropW - newW) / 2
		cropW = newW
	} else {
		newH := int(float64(cropW)/boxAspect + 0.5)
		if newH < 1 {
			newH = 1
		}
		cropY += (cropH - newH) / 2
		cropH = newH
	}
	return
}

func (r *legacyBlockRender) draw(scr *vtui.ScreenBuf, p vtui.ImagePlacement, bg uint32) {
	if scr == nil || !p.Surface.Valid() || p.Cols <= 0 || p.Rows <= 0 {
		return
	}
	cw, ch := scr.Graphics().CellSize()
	if cw <= 0 || ch <= 0 {
		cw, ch = 10, 20
	}
	cropW, cropH, cropX, cropY := legacyCropRect(p, cw, ch)

	stride := p.Surface.Stride
	maxDim := cropW
	if cropH > maxDim {
		maxDim = cropH
	}
	dstW, dstH := p.Cols, p.Rows*2
	r.ensureCapacity(maxDim+1, dstW*cropH*3, dstW*dstH, p.Cols*p.Rows)

	prefR, prefG, prefB := r.prefR, r.prefG, r.prefB
	hBuf, vBuf, cells := r.hBuf, r.vBuf, r.cells

	bgR := byte((bg >> 16) & 0xFF)
	bgG := byte((bg >> 8) & 0xFF)
	bgB := byte(bg & 0xFF)

	for y := 0; y < cropH; y++ {
		rowStart := (cropY+y)*stride + cropX*4
		var rSum, gSum, bSum uint32
		prefR[0], prefG[0], prefB[0] = 0, 0, 0
		for x := 0; x < cropW; x++ {
			px := p.Surface.Pix[rowStart+x*4:]
			al := uint32(px[3])
			var pr, pg, pb uint32
			if al >= 255 {
				pr, pg, pb = uint32(px[0]), uint32(px[1]), uint32(px[2])
			} else if al == 0 {
				pr, pg, pb = uint32(bgR), uint32(bgG), uint32(bgB)
			} else {
				invAl := 255 - al
				pr = (uint32(px[0])*al + uint32(bgR)*invAl + 127) / 255
				pg = (uint32(px[1])*al + uint32(bgG)*invAl + 127) / 255
				pb = (uint32(px[2])*al + uint32(bgB)*invAl + 127) / 255
			}
			rSum += pr
			gSum += pg
			bSum += pb
			prefR[x+1], prefG[x+1], prefB[x+1] = rSum, gSum, bSum
		}
		hRowStart := y * dstW * 3
		for cx := 0; cx < dstW; cx++ {
			x1 := (cx * cropW) / dstW
			x2 := ((cx + 1) * cropW) / dstW
			if x2 == x1 {
				x2 = x1 + 1
			}
			count := uint32(x2 - x1)
			hBuf[hRowStart+cx*3] = uint8((prefR[x2] - prefR[x1]) / count)
			hBuf[hRowStart+cx*3+1] = uint8((prefG[x2] - prefG[x1]) / count)
			hBuf[hRowStart+cx*3+2] = uint8((prefB[x2] - prefB[x1]) / count)
		}
	}
	for cx := 0; cx < dstW; cx++ {
		var rSum, gSum, bSum uint32
		prefR[0], prefG[0], prefB[0] = 0, 0, 0
		for y := 0; y < cropH; y++ {
			idx := y*dstW*3 + cx*3
			rSum += uint32(hBuf[idx])
			gSum += uint32(hBuf[idx+1])
			bSum += uint32(hBuf[idx+2])
			prefR[y+1], prefG[y+1], prefB[y+1] = rSum, gSum, bSum
		}
		for cy := 0; cy < dstH; cy++ {
			y1 := (cy * cropH) / dstH
			y2 := ((cy + 1) * cropH) / dstH
			if y2 == y1 {
				y2 = y1 + 1
			}
			count := uint32(y2 - y1)
			r := (prefR[y2] - prefR[y1]) / count
			g := (prefG[y2] - prefG[y1]) / count
			b := (prefB[y2] - prefB[y1]) / count
			vBuf[cy*dstW+cx] = (r << 16) | (g << 8) | b
		}
	}
	for cy := 0; cy < dstH/2; cy++ {
		topRow := cy * 2 * dstW
		botRow := (cy*2 + 1) * dstW
		rowStart := cy * p.Cols
		for cx := 0; cx < dstW; cx++ {
			fg := vBuf[topRow+cx]
			bgc := vBuf[botRow+cx]
			ch := uint64(blockHalfChar)
			if fg == bgc {
				ch = ' '
			}
			cells[rowStart+cx] = vtui.CharInfo{Char: ch, Attributes: vtui.SetRGBBoth(0, fg, bgc)}
		}
	}
	for cy := 0; cy < p.Rows; cy++ {
		scr.Write(p.Col, p.Row+cy, cells[cy*p.Cols:(cy+1)*p.Cols])
	}
}

func BenchmarkLegacyBoxFilter_4KtoTerminal(b *testing.B) {
	surf := newBenchSurface(3840, 2160)
	p := vtui.ImagePlacement{Surface: surf, Cols: 80, Rows: 25, SrcW: 3840, SrcH: 2160}
	r := &legacyBlockRender{}
	scr := newBenchScreen()
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		r.draw(scr, p, blockImageBack)
	}
}
