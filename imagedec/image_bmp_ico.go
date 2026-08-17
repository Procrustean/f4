package imagedec

// BMP, ICO and CUR. Nothing produces BMP on purpose any more and everything
// produces it by accident, so a file manager had better read it — and the
// icon formats are BMP frames behind a small directory.

import (
	"encoding/binary"
	"fmt"

	"github.com/unxed/vtui"
)

const (
	bmpFileHeaderSize = 14
	bmpInfoHeaderSize = 40

	// ICO and CUR are a directory in front of BMP (or PNG) entries.
	icoDirSize   = 6
	icoEntrySize = 16
	icoBMPType   = 1 // icon
	icoCURType   = 2 // cursor
)

func decodeBMP(data []byte) (*vtui.ImageSurface, error) {
	if len(data) < bmpFileHeaderSize+bmpInfoHeaderSize || data[0] != 'B' || data[1] != 'M' {
		return nil, fmt.Errorf("not a BMP image")
	}
	le := binary.LittleEndian

	pixelOffset := int(le.Uint32(data[10:14]))
	headerSize := int(le.Uint32(data[14:18]))
	if headerSize < bmpInfoHeaderSize {
		return nil, fmt.Errorf("unsupported BMP header of %d bytes", headerSize)
	}

	width := int(int32(le.Uint32(data[18:22])))
	height := int(int32(le.Uint32(data[22:26])))
	bits := int(le.Uint16(data[28:30]))
	compression := le.Uint32(data[30:34])
	paletteLen := int(le.Uint32(data[46:50]))

	// A negative height means rows are stored top-down.
	topDown := height < 0
	if topDown {
		height = -height
	}
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("the image has no size")
	}
	if err := validateImageAllocation(width, height, 4, imageMaxPixels, 0); err != nil {
		return nil, fmt.Errorf("the image is too large: %dx%d: %w", width, height, err)
	}
	if compression != 0 {
		return nil, fmt.Errorf("compressed BMP images are not supported")
	}
	switch bits {
	case 1, 4, 8, 16, 24, 32:
	default:
		return nil, fmt.Errorf("unsupported colour depth of %d bits", bits)
	}

	var palette []qoiPixel
	if bits <= 8 {
		if paletteLen <= 0 || paletteLen > 1<<bits {
			paletteLen = 1 << bits
		}
		at := bmpFileHeaderSize + headerSize
		if at+paletteLen*4 > len(data) {
			return nil, fmt.Errorf("the palette is truncated")
		}
		palette = make([]qoiPixel, paletteLen)
		for i := range palette {
			e := data[at+i*4:]
			palette[i] = qoiPixel{r: e[2], g: e[1], b: e[0], a: 0xFF}
		}
	}

	rowBits := uint64(width) * uint64(bits)
	if rowBits/uint64(width) != uint64(bits) || rowBits > uint64(^uint(0)>>1)-31 {
		return nil, fmt.Errorf("BMP row stride overflows")
	}
	stride := int((rowBits + 31) / 32 * 4)
	if pixelOffset < bmpFileHeaderSize+headerSize || pixelOffset > len(data) || uint64(stride)*uint64(height) > uint64(len(data)-pixelOffset) {
		return nil, fmt.Errorf("the pixel data is truncated")
	}

	pix := make([]byte, width*height*4)
	sawAlpha := false

	// One tight loop per bit depth: the branch and the per-pixel re-slicing
	// stay out of the inner loop, so the common 24/32-bit paths write
	// straight into the buffer.
	dstRowOf := func(y int) int {
		if topDown {
			return y
		}
		return height - 1 - y
	}
	switch bits {
	case 32:
		for y := 0; y < height; y++ {
			src := pixelOffset + stride*y
			dst := dstRowOf(y) * width * 4
			for x := 0; x < width; x++ {
				s := src + x*4
				a := data[s+3]
				if a != 0 {
					sawAlpha = true
				}
				at := dst + x*4
				pix[at], pix[at+1], pix[at+2], pix[at+3] = data[s+2], data[s+1], data[s], a
			}
		}
	case 24:
		for y := 0; y < height; y++ {
			src := pixelOffset + stride*y
			dst := dstRowOf(y) * width * 4
			for x := 0; x < width; x++ {
				s := src + x*3
				at := dst + x*4
				pix[at], pix[at+1], pix[at+2], pix[at+3] = data[s+2], data[s+1], data[s], 0xFF
			}
		}
	case 16:
		for y := 0; y < height; y++ {
			src := pixelOffset + stride*y
			dst := dstRowOf(y) * width * 4
			for x := 0; x < width; x++ {
				v := le.Uint16(data[src+x*2:])
				at := dst + x*4
				pix[at], pix[at+1], pix[at+2], pix[at+3] = bmpScale5((v>>10)&0x1F), bmpScale5((v>>5)&0x1F), bmpScale5(v&0x1F), 0xFF
			}
		}
	case 8:
		for y := 0; y < height; y++ {
			src := pixelOffset + stride*y
			dst := dstRowOf(y) * width * 4
			for x := 0; x < width; x++ {
				c := bmpPaletteAt(palette, int(data[src+x]))
				at := dst + x*4
				pix[at], pix[at+1], pix[at+2], pix[at+3] = c.r, c.g, c.b, c.a
			}
		}
	case 4:
		for y := 0; y < height; y++ {
			src := pixelOffset + stride*y
			dst := dstRowOf(y) * width * 4
			for x := 0; x < width; x++ {
				b := data[src+x/2]
				if x%2 == 0 {
					b >>= 4
				}
				c := bmpPaletteAt(palette, int(b&0x0F))
				at := dst + x*4
				pix[at], pix[at+1], pix[at+2], pix[at+3] = c.r, c.g, c.b, c.a
			}
		}
	default: // 1 bpp
		for y := 0; y < height; y++ {
			src := pixelOffset + stride*y
			dst := dstRowOf(y) * width * 4
			for x := 0; x < width; x++ {
				b := data[src+x/8]
				c := bmpPaletteAt(palette, int((b>>(7-uint(x%8)))&0x01))
				at := dst + x*4
				pix[at], pix[at+1], pix[at+2], pix[at+3] = c.r, c.g, c.b, c.a
			}
		}
	}

	// A 32-bit BMP with an all-zero alpha channel is opaque; fill it in.
	if bits == 32 && !sawAlpha {
		for i := 3; i < len(pix); i += 4 {
			pix[i] = 0xFF
		}
	}

	return surfaceFromRGBA(width, height, width*4, pix)
}

// bmpScale5 stretches a five bit channel over the whole byte.
func bmpScale5(v uint16) byte {
	return byte((int(v)*255 + 15) / 31)
}

func bmpPaletteAt(palette []qoiPixel, i int) qoiPixel {
	if i < 0 || i >= len(palette) {
		return qoiPixel{a: 0xFF}
	}
	return palette[i]
}

// decodeBMPEntry reads a BMP, or an ICO/CUR container when the magic says so.
func decodeBMPEntry(data []byte) (*vtui.ImageSurface, error) {
	if isICO(data) {
		return decodeICO(data)
	}
	return decodeBMP(data)
}

// isICO tells the ICO/CUR directory (00 00 01 00 / 00 00 02 00) from a BMP.
func isICO(data []byte) bool {
	return len(data) >= 4 && data[0] == 0 && data[1] == 0 &&
		(data[2] == icoBMPType || data[2] == icoCURType) && data[3] == 0
}

// decodeICO decodes the best frame of an ICO/CUR file. Modern icons embed a
// PNG; the classic ones a BMP whose height counts both the picture and its
// AND mask, which becomes the alpha channel.
func decodeICO(data []byte) (*vtui.ImageSurface, error) {
	if len(data) < icoDirSize {
		return nil, fmt.Errorf("not an ICO image")
	}
	le := binary.LittleEndian
	if data[0] != 0 || data[1] != 0 {
		return nil, fmt.Errorf("not an ICO image")
	}
	typ := le.Uint16(data[2:4])
	if typ != icoBMPType && typ != icoCURType {
		return nil, fmt.Errorf("not an ICO image")
	}
	count := int(le.Uint16(data[4:6]))
	if count <= 0 || count > 256 {
		return nil, fmt.Errorf("invalid ICO entry count %d", count)
	}
	dirEnd := icoDirSize + count*icoEntrySize
	if dirEnd > len(data) {
		return nil, fmt.Errorf("the ICO directory is truncated")
	}

	// The largest entry makes the best picture; ties go to the deeper one.
	// A zero width or height byte means 256.
	best := -1
	bestArea, bestBits := 0, 0
	for i := 0; i < count; i++ {
		e := icoDirSize + i*icoEntrySize
		w := int(data[e])
		if w == 0 {
			w = 256
		}
		h := int(data[e+1])
		if h == 0 {
			h = 256
		}
		bits := int(le.Uint16(data[e+6 : e+8]))
		area := w * h
		if best < 0 || area > bestArea || (area == bestArea && bits > bestBits) {
			best, bestArea, bestBits = i, area, bits
		}
	}

	e := icoDirSize + best*icoEntrySize
	size := int(le.Uint32(data[e+8 : e+12]))
	off := int(le.Uint32(data[e+12 : e+16]))
	if off < dirEnd || size <= 0 || off+size > len(data) {
		return nil, fmt.Errorf("the ICO image data is truncated")
	}
	img := data[off : off+size]
	if isPNG(img) {
		return DecodeImageWithStdlib(img)
	}
	return decodeICOBMP(img)
}

// decodeICOBMP decodes one ICO entry's BMP payload (no file header; height
// counts picture + AND mask). It is repackaged as a plain BMP and the mask
// becomes the alpha.
func decodeICOBMP(data []byte) (*vtui.ImageSurface, error) {
	le := binary.LittleEndian
	if len(data) < bmpInfoHeaderSize {
		return nil, fmt.Errorf("the ICO entry holds no BMP")
	}
	headerSize := int(le.Uint32(data[0:4]))
	if headerSize < bmpInfoHeaderSize {
		return nil, fmt.Errorf("unsupported ICO BMP header of %d bytes", headerSize)
	}
	width := int(int32(le.Uint32(data[4:8])))
	height := int(int32(le.Uint32(data[8:12])))
	bits := int(le.Uint16(data[14:16]))
	if width <= 0 || height <= 0 || bits == 0 {
		return nil, fmt.Errorf("the image has no size")
	}
	// The stored height may count the AND mask; check the larger value.
	if err := validateImageAllocation(width, height, 4, imageMaxPixels, 0); err != nil {
		return nil, fmt.Errorf("the image is too large: %dx%d: %w", width, height, err)
	}

	paletteLen := 0
	if bits <= 8 {
		paletteLen = int(le.Uint32(data[32:36])) // biClrUsed
		if paletteLen <= 0 || paletteLen > 1<<bits {
			paletteLen = 1 << bits
		}
	}
	pixOff := headerSize + paletteLen*4
	if pixOff > len(data) {
		return nil, fmt.Errorf("the palette is truncated")
	}

	// The classic ICO doubles the height: half the rows are XOR, half the AND
	// mask. biSizeImage, when set, settles which; else the doubled layout
	// wins when its mask fits.
	stride := (width*bits + 31) / 32 * 4
	andStride := (width + 31) / 32 * 4
	avail := len(data) - pixOff
	picH := height / 2
	if picH < 1 {
		picH = height
	}
	if biSize := int(le.Uint32(data[20:24])); biSize > 0 {
		switch {
		case biSize == stride*height:
			picH = height
		case height > 1 && biSize == stride*(height/2):
			picH = height / 2
		}
	} else if stride*picH+andStride*picH > avail {
		picH = height
	}
	if picH < 1 || stride*picH > avail {
		return nil, fmt.Errorf("the pixel data is truncated")
	}
	xorLen := stride * picH

	// Repackage as a plain BMP with the height corrected.
	pixelOffset := bmpFileHeaderSize + headerSize + paletteLen*4
	out := make([]byte, 0, pixelOffset+xorLen)
	out = append(out, 'B', 'M')
	out = le.AppendUint32(out, uint32(pixelOffset+xorLen))
	out = le.AppendUint32(out, 0)
	out = le.AppendUint32(out, uint32(pixelOffset))
	out = append(out, data[:pixelOffset-bmpFileHeaderSize]...)
	out = append(out, data[pixOff:pixOff+xorLen]...)
	le.PutUint32(out[22:26], uint32(int32(picH)))

	surf, err := decodeBMP(out)
	if err != nil {
		return nil, err
	}

	// The AND mask is one bit per pixel (set = transparent), bottom-up. A
	// 32-bit entry that already carries alpha usually has no mask worth it.
	andOff := pixOff + xorLen
	if andOff+andStride*picH > len(data) {
		return surf, nil
	}
	transparent := false
	for sy := 0; sy < picH; sy++ {
		row := andOff + (picH-1-sy)*andStride
		for x := 0; x < width; x++ {
			if data[row+x/8]>>uint(7-x%8)&1 == 1 {
				surf.Pix[sy*surf.Stride+x*4+3] = 0
				transparent = true
			}
		}
	}
	if transparent {
		surf.Opaque = false
	}
	return surf, nil
}

func init() {
	RegisterImageDecoder(ImageDecoder{
		Name:       "go-bmp-ico",
		Priority:   10,
		Extensions: []string{"bmp", "dib", "ico", "cur"},
		Decode:     decodeBMPEntry,
	})
}
