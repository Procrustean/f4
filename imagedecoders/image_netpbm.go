package imagedecoders

// Netpbm (P1..P7, PF, Pf) decoder with streaming and raw/ASCII support.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"strconv"
	"unsafe"

	"github.com/unxed/vtui"
)

const maxNetpbmDimension = 16384
const maxNetpbmHeaderLine = 1024
const maxNetpbmHeaderToken = 64
const netpbmBlockSize = 4096

var endHdr = []byte("ENDHDR")
var errNetpbmToken = errors.New("netpbm: numeric token out of range")
var errNetpbmTokenLong = errors.New("netpbm: token too long")
var errNetpbmHeaderLong = errors.New("netpbm: header parse error: line too long")

// pbmLookup: raster byte -> 8 RGBA pixels (MSB first, 1=black).
var pbmLookup [256][32]byte

func init() {
	for i := range pbmLookup {
		row := &pbmLookup[i]
		for j := 0; j < 8; j++ {
			c := byte(255)
			if (i>>(7-j))&1 == 1 {
				c = 0
			}
			off := j * 4
			row[off], row[off+1], row[off+2], row[off+3] = c, c, c, 255
		}
	}
}

func validTupleTypeDepth(tupleType string, depth int) bool {
	switch tupleType {
	case "BLACKANDWHITE", "GRAYSCALE":
		return depth == 1
	case "BLACKANDWHITE_ALPHA", "GRAYSCALE_ALPHA":
		return depth == 2
	case "RGB":
		return depth == 3
	case "RGB_ALPHA":
		return depth == 4
	default:
		return true // unknown/absent: trust DEPTH alone
	}
}

func scaleTo8(v uint32, maxVal uint32) uint8 {
	if maxVal == 0 {
		return 0
	}
	if v >= maxVal {
		return 255
	}
	if maxVal == 255 {
		return uint8(v)
	}
	if maxVal == 65535 {
		return uint8((v + 128) / 257)
	}
	return uint8((v*255 + maxVal/2) / maxVal)
}

func floatTo8(val float32, absScale float64) uint8 {
	fVal := float64(val)
	if math.IsNaN(fVal) || math.IsInf(fVal, 0) || absScale <= 0 || math.IsNaN(absScale) || math.IsInf(absScale, 0) {
		return 0
	}
	v := fVal / absScale
	if v <= 0 {
		return 0
	}
	if v >= 1.0 {
		return 255
	}
	return uint8(v*255.0 + 0.5)
}

// decodeNetpbm decodes a Netpbm stream; the bool says whether the format is
// opaque by construction (no alpha).
func decodeNetpbm(r io.Reader) (image.Image, bool, error) {
	var br *bufio.Reader
	if b, ok := r.(*bufio.Reader); ok {
		br = b
	} else {
		br = bufio.NewReader(r)
	}

	magicLine, err := br.ReadSlice('\n')
	if err == bufio.ErrBufferFull || len(magicLine) > maxNetpbmHeaderLine {
		return nil, false, fmt.Errorf("netpbm: header read error: line too long")
	}
	if err != nil {
		return nil, false, fmt.Errorf("netpbm: header read error: %w", err)
	}
	magic := string(bytes.TrimSpace(magicLine))

	var width, height, depth, maxVal int
	var pfmScale float64
	var tokBuf [maxNetpbmHeaderToken + 1]byte

	switch magic {
	case "P1", "P4": // PBM
		wTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		if width, err = parseUint(wTok); err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}

		hTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		if height, err = parseUint(hTok); err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		maxVal = 1
		depth = 1

	case "P2", "P3", "P5", "P6": // PGM / PPM
		wTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		if width, err = parseUint(wTok); err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}

		hTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		if height, err = parseUint(hTok); err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}

		mTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		if maxVal, err = parseUint(mTok); err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}

		if magic == "P3" || magic == "P6" {
			depth = 3
		} else {
			depth = 1
		}

	case "PF", "Pf": // PFM Float
		wTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		if width, err = parseUint(wTok); err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}

		hTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		if height, err = parseUint(hTok); err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}

		sTok, err := readNetpbmToken(br, tokBuf[:0])
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
		}
		pfmScale, err = strconv.ParseFloat(string(sTok), 64)
		if err != nil {
			return nil, false, fmt.Errorf("netpbm: PFM scale parse error: %w", err)
		}
		maxVal = 255
		if magic == "PF" {
			depth = 3
		} else {
			depth = 1
		}

	case "P7": // PAM
		var tupleType string
		for {
			line, err := br.ReadSlice('\n')
			if err == bufio.ErrBufferFull || len(line) > maxNetpbmHeaderLine {
				return nil, false, errNetpbmHeaderLong
			}
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
			}
			line = bytes.TrimSpace(line)
			if len(line) == 0 || line[0] == '#' {
				continue
			}
			if bytes.Equal(line, endHdr) {
				break
			}
			key, val, ok := parseHeaderKeyVal(line)
			if !ok {
				continue
			}
			var v int
			switch string(key) {
			case "WIDTH":
				v, err = parseUint(val)
				width = v
			case "HEIGHT":
				v, err = parseUint(val)
				height = v
			case "DEPTH":
				v, err = parseUint(val)
				depth = v
			case "MAXVAL":
				v, err = parseUint(val)
				maxVal = v
			case "TUPLTYPE":
				tupleType = string(bytes.TrimSpace(val))
			}
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: header parse error: %w", err)
			}
		}
		if tupleType != "" && !validTupleTypeDepth(tupleType, depth) {
			return nil, false, fmt.Errorf("netpbm: PAM TUPLTYPE %q incompatible with DEPTH %d", tupleType, depth)
		}

	default:
		return nil, false, fmt.Errorf("netpbm: unsupported magic %q", magic)
	}

	if width > maxNetpbmDimension || height > maxNetpbmDimension {
		return nil, false, fmt.Errorf("netpbm: invalid dimensions %dx%d", width, height)
	}
	// Only depths with an alpha channel can be translucent.
	opaque := depth == 1 || depth == 3
	if err := validateImageAllocation(width, height, 4, int64(maxNetpbmDimension)*maxNetpbmDimension, 2<<30); err != nil {
		return nil, false, fmt.Errorf("netpbm: image limits exceeded %dx%d: %w", width, height, err)
	}
	if maxVal <= 0 || maxVal > 65535 {
		return nil, false, fmt.Errorf("netpbm: invalid MAXVAL %d", maxVal)
	}
	if depth < 1 || depth > 4 {
		return nil, false, fmt.Errorf("netpbm: unsupported depth %d", depth)
	}

	// Maxval's whitespace is consumed; the raster starts here (0x23 is data,
	// not a comment).

	totalPixels := width * height
	// []uint32 backing keeps 4-byte alignment (1-aligned []byte would SIGBUS on ARM64).
	pix32 := make([]uint32, totalPixels)
	pix8 := unsafe.Slice((*byte)(unsafe.Pointer(&pix32[0])), totalPixels*4)
	img := &image.NRGBA{Pix: pix8, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}
	dst := img.Pix

	switch magic {
	case "P1": // PBM ASCII (bits may be written with or without separators)
		for i := range totalPixels {
			for {
				b, err := br.ReadByte()
				if err != nil {
					return nil, false, fmt.Errorf("netpbm: P1 read error: %w", err)
				}
				if b == '#' {
					if err := skipNetpbmLine(br); err != nil {
						return nil, false, fmt.Errorf("netpbm: P1 read error: %w", err)
					}
					continue
				}
				if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
					continue
				}
				d := i * 4
				if b == '1' { // black
					dst[d], dst[d+1], dst[d+2], dst[d+3] = 0, 0, 0, 255
				} else if b == '0' { // white
					dst[d], dst[d+1], dst[d+2], dst[d+3] = 255, 255, 255, 255
				} else {
					return nil, false, fmt.Errorf("netpbm: P1 invalid character %q", b)
				}
				break
			}
		}
		return img, opaque, nil

	case "P2": // PGM ASCII
		m := uint32(maxVal)
		for i := range totalPixels {
			tok, err := readNetpbmToken(br, tokBuf[:0])
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P2 read error: %w", err)
			}
			v, err := parseUint(tok)
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P2 pixel parse error: %w", err)
			}
			g := scaleTo8(uint32(v), m)
			d := i * 4
			dst[d], dst[d+1], dst[d+2], dst[d+3] = g, g, g, 255
		}
		return img, opaque, nil

	case "P3": // PPM ASCII
		m := uint32(maxVal)
		for i := range totalPixels {
			rTok, err := readNetpbmToken(br, tokBuf[:0])
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P3 read error: %w", err)
			}
			rv, err := parseUint(rTok)
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P3 pixel parse error: %w", err)
			}

			gTok, err := readNetpbmToken(br, tokBuf[:0])
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P3 read error: %w", err)
			}
			gv, err := parseUint(gTok)
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P3 pixel parse error: %w", err)
			}

			bTok, err := readNetpbmToken(br, tokBuf[:0])
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P3 read error: %w", err)
			}
			bv, err := parseUint(bTok)
			if err != nil {
				return nil, false, fmt.Errorf("netpbm: P3 pixel parse error: %w", err)
			}

			d := i * 4
			dst[d] = scaleTo8(uint32(rv), m)
			dst[d+1] = scaleTo8(uint32(gv), m)
			dst[d+2] = scaleTo8(uint32(bv), m)
			dst[d+3] = 255
		}
		return img, opaque, nil

	case "P4": // PBM Raw (1 bit/px, MSB-first, row-padded)
		rowBytes := (width + 7) / 8
		rowBuf := make([]byte, rowBytes)
		for y := range height {
			if _, err := io.ReadFull(br, rowBuf); err != nil {
				return nil, false, fmt.Errorf("netpbm: P4 read error: %w", err)
			}
			rowOff := y * width * 4
			x := 0
			for ; x+8 <= width; x += 8 {
				copy(dst[rowOff+x*4:], pbmLookup[rowBuf[x/8]][:])
			}
			for ; x < width; x++ {
				b := rowBuf[x/8]
				bit := (b >> uint(7-(x%8))) & 1
				d := rowOff + x*4
				if bit == 1 {
					dst[d], dst[d+1], dst[d+2], dst[d+3] = 0, 0, 0, 255 // Black
				} else {
					dst[d], dst[d+1], dst[d+2], dst[d+3] = 255, 255, 255, 255 // White
				}
			}
		}
		return img, opaque, nil

	case "PF", "Pf": // PFM Float32 (Bottom-to-top scanlines)
		isLittle := pfmScale < 0
		absScale := math.Abs(pfmScale)
		if absScale == 0 || math.IsNaN(absScale) || math.IsInf(absScale, 0) {
			absScale = 1.0
		}

		// Row buffers allocated once; []float32 backing keeps the unsafe view aligned.
		var floats []float32
		var rowBuf []byte
		if isLittle {
			floats = make([]float32, width*depth)
			rowBuf = unsafe.Slice((*byte)(unsafe.Pointer(&floats[0])), len(floats)*4)
		} else {
			rowBuf = make([]byte, width*depth*4)
		}

		for y := height - 1; y >= 0; y-- {
			if _, err := io.ReadFull(br, rowBuf); err != nil {
				return nil, false, fmt.Errorf("netpbm: PFM pixel read error: %w", err)
			}
			rowOff := y * width * 4
			if isLittle {
				if depth == 1 {
					for x := range width {
						g := floatTo8(floats[x], absScale)
						d := rowOff + x*4
						dst[d], dst[d+1], dst[d+2], dst[d+3] = g, g, g, 255
					}
					continue
				}
				for x := range width {
					d := rowOff + x*4
					dst[d] = floatTo8(floats[x*depth], absScale)
					dst[d+1] = floatTo8(floats[x*depth+1], absScale)
					dst[d+2] = floatTo8(floats[x*depth+2], absScale)
					dst[d+3] = 255
				}
				continue
			}
			if depth == 1 {
				for x := range width {
					g := floatTo8(math.Float32frombits(binary.BigEndian.Uint32(rowBuf[x*4:])), absScale)
					d := rowOff + x*4
					dst[d], dst[d+1], dst[d+2], dst[d+3] = g, g, g, 255
				}
				continue
			}
			for x := range width {
				p := x * 12
				d := rowOff + x*4
				dst[d] = floatTo8(math.Float32frombits(binary.BigEndian.Uint32(rowBuf[p:])), absScale)
				dst[d+1] = floatTo8(math.Float32frombits(binary.BigEndian.Uint32(rowBuf[p+4:])), absScale)
				dst[d+2] = floatTo8(math.Float32frombits(binary.BigEndian.Uint32(rowBuf[p+8:])), absScale)
				dst[d+3] = 255
			}
		}
		return img, opaque, nil

	case "P5", "P6", "P7":
		if maxVal == 255 && depth >= 1 && depth <= 4 {
			if depth == 4 {
				if _, err := io.ReadFull(br, dst); err != nil {
					return nil, false, fmt.Errorf("netpbm: RGBA read error: %w", err)
				}
				return img, opaque, nil
			}
			if _, err := io.ReadFull(br, dst[:totalPixels*depth]); err != nil {
				return nil, false, fmt.Errorf("netpbm: pixel read error: %w", err)
			}
			// Backward expansion so stores never clobber unread sources.
			// Groups of 4/4/8 pixels (depths 3/2/1) expand from one 64-bit
			// load each; stores stay below the next group's unread input.
			dst32 := unsafe.Slice((*uint32)(unsafe.Pointer(&dst[0])), totalPixels)
			switch depth {
			case 3:
				i := totalPixels
				for ; i >= 8; i -= 4 {
					s := (i - 4) * 3
					q0 := *(*uint64)(unsafe.Pointer(&dst[s]))
					q1 := *(*uint64)(unsafe.Pointer(&dst[s+6]))
					j := i - 4
					v := uint32(q0)
					dst32[j] = v&0xFF | (v>>8&0xFF)<<8 | (v>>16&0xFF)<<16 | 0xFF000000
					v = uint32(q0 >> 24)
					dst32[j+1] = v&0xFF | (v>>8&0xFF)<<8 | (v>>16&0xFF)<<16 | 0xFF000000
					v = uint32(q1)
					dst32[j+2] = v&0xFF | (v>>8&0xFF)<<8 | (v>>16&0xFF)<<16 | 0xFF000000
					v = uint32(q1 >> 24)
					dst32[j+3] = v&0xFF | (v>>8&0xFF)<<8 | (v>>16&0xFF)<<16 | 0xFF000000
				}
				for ; i > 0; i-- { // tail: fewer than 4 pixels
					s := (i - 1) * 3
					dst32[i-1] = uint32(dst[s]) | (uint32(dst[s+1]) << 8) | (uint32(dst[s+2]) << 16) | 0xFF000000
				}
			case 2:
				i := totalPixels
				for ; i >= 4; i -= 4 {
					j := i - 4
					q := *(*uint64)(unsafe.Pointer(&dst[j*2]))
					g := uint32(q & 0xFF)
					dst32[j] = g | g<<8 | g<<16 | (uint32(q>>8)&0xFF)<<24
					g = uint32(q >> 16 & 0xFF)
					dst32[j+1] = g | g<<8 | g<<16 | (uint32(q>>24)&0xFF)<<24
					g = uint32(q >> 32 & 0xFF)
					dst32[j+2] = g | g<<8 | g<<16 | (uint32(q>>40)&0xFF)<<24
					g = uint32(q >> 48 & 0xFF)
					dst32[j+3] = g | g<<8 | g<<16 | (uint32(q>>56)&0xFF)<<24
				}
				for ; i > 0; i-- {
					s := (i - 1) * 2
					g := uint32(dst[s])
					dst32[i-1] = g | (g << 8) | (g << 16) | (uint32(dst[s+1]) << 24)
				}
			case 1:
				i := totalPixels
				for ; i >= 8; i -= 8 {
					j := i - 8
					q := *(*uint64)(unsafe.Pointer(&dst[j]))
					for k := range 8 {
						g := uint32(q & 0xFF)
						dst32[j+k] = g | g<<8 | g<<16 | 0xFF000000
						q >>= 8
					}
				}
				for ; i > 0; i-- {
					g := uint32(dst[i-1])
					dst32[i-1] = g | (g << 8) | (g << 16) | 0xFF000000
				}
			}
			return img, opaque, nil
		}

		bytesPerSample := 1
		if maxVal > 255 {
			bytesPerSample = 2
		}
		m := uint32(maxVal)

		scratch := make([]byte, netpbmBlockSize*4*2)

		// MAXVAL 65535 (what encoders emit): the const /257 scale lets whole
		// blocks be scaled once, then copied out per pixel.
		if bytesPerSample == 2 && maxVal == 65535 {
			scaled := make([]byte, netpbmBlockSize*4)
			for base := 0; base < totalPixels; base += netpbmBlockSize {
				n := totalPixels - base
				if n > netpbmBlockSize {
					n = netpbmBlockSize
				}
				if _, err := io.ReadFull(br, scratch[:n*depth*2]); err != nil {
					return nil, false, fmt.Errorf("netpbm: raw pixel read error: %w", err)
				}
				lim := n * depth * 2
				ob := 0
				for o := 0; o+16 <= lim; o += 16 {
					q0 := binary.BigEndian.Uint64(scratch[o:])
					q1 := binary.BigEndian.Uint64(scratch[o+8:])
					scaled[ob] = uint8((uint32(q0>>48) + 128) / 257)
					scaled[ob+1] = uint8((uint32(q0>>32&0xFFFF) + 128) / 257)
					scaled[ob+2] = uint8((uint32(q0>>16&0xFFFF) + 128) / 257)
					scaled[ob+3] = uint8((uint32(q0&0xFFFF) + 128) / 257)
					scaled[ob+4] = uint8((uint32(q1>>48) + 128) / 257)
					scaled[ob+5] = uint8((uint32(q1>>32&0xFFFF) + 128) / 257)
					scaled[ob+6] = uint8((uint32(q1>>16&0xFFFF) + 128) / 257)
					scaled[ob+7] = uint8((uint32(q1&0xFFFF) + 128) / 257)
					ob += 8
				}
				for ; ob < n*depth; ob++ {
					o := ob * 2
					scaled[ob] = scaleTo8(uint32(binary.BigEndian.Uint16(scratch[o:])), 65535)
				}
				for j := range n {
					s := j * depth
					d := (base + j) * 4
					switch depth {
					case 1:
						g := scaled[s]
						dst[d], dst[d+1], dst[d+2], dst[d+3] = g, g, g, 255
					case 2:
						g, a := scaled[s], scaled[s+1]
						dst[d], dst[d+1], dst[d+2], dst[d+3] = g, g, g, a
					case 3:
						dst[d], dst[d+1], dst[d+2], dst[d+3] = scaled[s], scaled[s+1], scaled[s+2], 255
					case 4:
						dst[d], dst[d+1], dst[d+2], dst[d+3] = scaled[s], scaled[s+1], scaled[s+2], scaled[s+3]
					}
				}
			}
			return img, opaque, nil
		}

		for base := 0; base < totalPixels; base += netpbmBlockSize {
			n := totalPixels - base
			if n > netpbmBlockSize {
				n = netpbmBlockSize
			}
			if _, err := io.ReadFull(br, scratch[:n*depth*bytesPerSample]); err != nil {
				return nil, false, fmt.Errorf("netpbm: raw pixel read error: %w", err)
			}

			for j := range n {
				d := (base + j) * 4
				off := j * depth * bytesPerSample

				// Straight alpha: RGB bytes kept even where A==0.
				var samples [4]uint32
				for c := range depth {
					if bytesPerSample == 1 {
						samples[c] = uint32(scratch[off+c])
					} else {
						samples[c] = uint32(binary.BigEndian.Uint16(scratch[off+c*2:]))
					}
				}

				switch depth {
				case 1:
					g := scaleTo8(samples[0], m)
					dst[d], dst[d+1], dst[d+2], dst[d+3] = g, g, g, 255
				case 2:
					g := scaleTo8(samples[0], m)
					a := scaleTo8(samples[1], m)
					dst[d], dst[d+1], dst[d+2], dst[d+3] = g, g, g, a
				case 3:
					dst[d] = scaleTo8(samples[0], m)
					dst[d+1] = scaleTo8(samples[1], m)
					dst[d+2] = scaleTo8(samples[2], m)
					dst[d+3] = 255
				case 4:
					dst[d] = scaleTo8(samples[0], m)
					dst[d+1] = scaleTo8(samples[1], m)
					dst[d+2] = scaleTo8(samples[2], m)
					dst[d+3] = scaleTo8(samples[3], m)
				}
			}
		}
		return img, opaque, nil
	}

	return nil, false, fmt.Errorf("netpbm: unsupported format %s", magic)
}

func decodeNetpbmSurface(data []byte) (*vtui.ImageSurface, error) {
	if len(data) == 0 {
		return nil, errors.New("netpbm: empty file")
	}
	return decodeNetpbmSurfaceStream(bytes.NewReader(data))
}

func decodeNetpbmSurfaceStream(r io.Reader) (*vtui.ImageSurface, error) {
	img, opaque, err := decodeNetpbm(r)
	if err != nil {
		return nil, err
	}
	nrgba, ok := img.(*image.NRGBA)
	if !ok {
		return nil, fmt.Errorf("netpbm: unexpected decode type %T", img)
	}
	b := img.Bounds()
	surf, err := surfaceFromRGBA(b.Dx(), b.Dy(), b.Dx()*4, nrgba.Pix)
	if err != nil {
		return nil, fmt.Errorf("netpbm: %w", err)
	}
	surf.Opaque = opaque // formats without alpha skip the alpha scan
	return surf, nil
}

func readNetpbmToken(br *bufio.Reader, buf []byte) ([]byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == '#' {
			if err := skipNetpbmLine(br); err != nil {
				return nil, err
			}
			continue
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}

		tok := buf[:0]
		tok = append(tok, b)
		for {
			nb, err := br.ReadByte()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return tok, nil
				}
				return nil, err
			}
			if nb == ' ' || nb == '\t' || nb == '\r' || nb == '\n' {
				if nb == '\r' {
					if peek, err := br.Peek(1); err == nil && peek[0] == '\n' {
						_, _ = br.ReadByte()
					}
				}
				return tok, nil
			}
			if nb == '#' {
				_ = br.UnreadByte()
				return tok, nil
			}
			tok = append(tok, nb)
			if len(tok) > maxNetpbmHeaderToken {
				return nil, errNetpbmTokenLong
			}
		}
	}
}

func parseHeaderKeyVal(line []byte) (key, val []byte, ok bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return nil, nil, false
	}
	keyStart := i
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		i++
	}
	key = line[keyStart:i]
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) {
		return key, nil, true
	}
	val = line[i:]
	return key, val, true
}

func parseUint(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, errNetpbmToken
	}
	const limit = 1 << 30
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, errNetpbmToken
		}
		d := int(c - '0')
		if n > (limit-d)/10 {
			return 0, errNetpbmToken
		}
		n = n*10 + d
	}
	return n, nil
}

func skipNetpbmLine(br *bufio.Reader) error {
	line, err := br.ReadSlice('\n')
	// maxNetpbmHeaderLine bounds the body (excl. '\n'); +1 keeps that boundary.
	if err == bufio.ErrBufferFull || len(line) > maxNetpbmHeaderLine+1 {
		return errNetpbmHeaderLong
	}
	// EOF without '\n' passes up; callers report it as a header parse error.
	return err
}

func init() {
	RegisterImageDecoder(ImageDecoder{
		Name:       "go-netpbm",
		Priority:   10,
		Extensions: []string{"pam", "ppm", "pgm", "pnm", "pbm", "pfm"},
		Decode:     decodeNetpbmSurface,
	})
}
