package main

import (
	"unsafe"

	"github.com/unxed/vtui"
)

// Rotation and mirroring of decoded pictures. This is deliberately plain Go
// over the RGBA bytes of an ImageSurface: the graphics backends only know how
// to ship a rectangle of pixels, so anything the viewer wants to change about
// the orientation has to be baked into the pixels before they are sent.
//
// Every function here returns a fresh, tightly packed surface and never
// touches the source, so the viewer can keep the decoded picture around and
// rebuild the shown one whenever the orientation changes.
//
// A quarter turn is a transposition, the classic cache-hostile layout switch:
// it is done in one cache-blocked pass over uint32 pixels, which also lets a
// turn followed by a mirroring land in a single allocation instead of two.
// Plain half turns and mirrorings stay per-pixel, where they are already
// close to a memcpy.

// CopySurface returns a tightly packed copy of a surface.
func CopySurface(src *vtui.ImageSurface) *vtui.ImageSurface {
	if !src.Valid() {
		return nil
	}
	dst := vtui.NewImageSurface(src.Width, src.Height)
	line := src.Width * 4
	for y := 0; y < src.Height; y++ {
		s := y * src.Stride
		d := y * dst.Stride
		copy(dst.Pix[d:d+line], src.Pix[s:s+line])
	}
	dst.Opaque = src.Opaque
	return dst
}

// RotateSurface turns a picture clockwise by the given angle, which is taken
// modulo a full turn and has to be a multiple of ninety degrees; any other
// angle would need resampling and is treated as no rotation at all. A quarter
// turn swaps the width and the height.
func RotateSurface(src *vtui.ImageSurface, degrees int) *vtui.ImageSurface {
	if !src.Valid() {
		return nil
	}
	deg := ((degrees % 360) + 360) % 360
	if deg == 0 || deg%90 != 0 {
		return CopySurface(src)
	}

	w, h := src.Width, src.Height
	dstW, dstH := w, h
	if deg == 90 || deg == 270 {
		dstW, dstH = h, w
	}
	dst := vtui.NewImageSurface(dstW, dstH)
	dst.Opaque = src.Opaque

	if deg == 180 {
		// A half turn reverses every row in reverse order: per-pixel, but
		// row-wise sequential, already near memcpy speed.
		for y := 0; y < h; y++ {
			srow := (h - 1 - y) * src.Stride
			drow := y * dst.Stride
			for x := 0; x < w; x++ {
				s := srow + (w-1-x)*4
				d := drow + x*4
				copy(dst.Pix[d:d+4], src.Pix[s:s+4])
			}
		}
		return dst
	}

	rotateFlipPixels(dst, src, deg, false, false)
	return dst
}

// FlipSurface mirrors a picture across the vertical axis, the horizontal one,
// or both.
func FlipSurface(src *vtui.ImageSurface, horizontal, vertical bool) *vtui.ImageSurface {
	if !src.Valid() {
		return nil
	}
	if !horizontal && !vertical {
		return CopySurface(src)
	}

	dst := vtui.NewImageSurface(src.Width, src.Height)
	dst.Opaque = src.Opaque
	for y := 0; y < src.Height; y++ {
		dy := y
		if vertical {
			dy = src.Height - 1 - y
		}
		for x := 0; x < src.Width; x++ {
			dx := x
			if horizontal {
				dx = src.Width - 1 - x
			}
			s := y*src.Stride + x*4
			d := dy*dst.Stride + dx*4
			copy(dst.Pix[d:d+4], src.Pix[s:s+4])
		}
	}
	return dst
}

// TransformSurface applies a rotation and then a mirroring. The order matters:
// the flip is meant as "mirror what I see", so it works on the already turned
// picture rather than on the decoded one. A quarter turn with a mirror is the
// viewer's rebuild path, and it is done in one pass and one allocation.
func TransformSurface(src *vtui.ImageSurface, degrees int, horizontal, vertical bool) *vtui.ImageSurface {
	if !src.Valid() {
		return nil
	}
	deg := ((degrees % 360) + 360) % 360
	if deg%90 != 0 {
		deg = 0
	}
	if deg == 0 {
		return FlipSurface(src, horizontal, vertical)
	}
	if deg == 180 {
		// A half turn and a mirroring commute and are both row-wise fast;
		// the two-pass form stays on their fast path.
		out := RotateSurface(src, 180)
		if horizontal || vertical {
			out = FlipSurface(out, horizontal, vertical)
		}
		return out
	}

	dst := vtui.NewImageSurface(src.Height, src.Width)
	dst.Opaque = src.Opaque
	rotateFlipPixels(dst, src, deg, horizontal, vertical)
	return dst
}

// rotateFlipPixels writes the turned and mirrored source into dst in one
// cache-blocked pass over uint32 pixels. dst is tightly packed; src may be
// padded, so its stride is honoured.
func rotateFlipPixels(dst, src *vtui.ImageSurface, deg int, flipH, flipV bool) {
	w, h := src.Width, src.Height
	srcStride := src.Stride / 4
	src32 := unsafe.Slice((*uint32)(unsafe.Pointer(&src.Pix[0])), len(src.Pix)/4)
	dst32 := unsafe.Slice((*uint32)(unsafe.Pointer(&dst.Pix[0])), dst.Width*dst.Height)

	// A clockwise quarter turn is the transposition sx = y, sy = h-1-x, and
	// a mirroring folds an axis. A 270 turn is the 90 with both axes
	// mirrored, so its flags are folded in first. The four coefficients are
	// precomputed once instead of switched per pixel:
	//   sx = sxY*y,  sy = syX*x + syC
	if deg == 270 {
		flipH, flipV = !flipH, !flipV
	}
	sxY, sxC := 1, 0
	syX, syC := -1, h-1
	if flipV {
		sxY, sxC = -1, w-1
	}
	if flipH {
		syX, syC = 1, 0
	}

	// Cache blocking: a block of destination rows reads a block of source
	// rows, so the transposed access stays in cache.
	const block = 32
	for y0 := 0; y0 < dst.Height; y0 += block {
		y1 := y0 + block
		if y1 > dst.Height {
			y1 = dst.Height
		}
		for x0 := 0; x0 < dst.Width; x0 += block {
			x1 := x0 + block
			if x1 > dst.Width {
				x1 = dst.Width
			}
			for y := y0; y < y1; y++ {
				sx := sxY*y + sxC
				d := y * dst.Width
				for x := x0; x < x1; x++ {
					sy := syX*x + syC
					dst32[d+x] = src32[sy*srcStride+sx]
				}
			}
		}
	}
}
