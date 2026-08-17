package imagedec

// Header-only identification: a picture's dimensions and EXIF orientation,
// read from the first bytes without decoding pixels. Cheap enough to run
// over a whole directory ahead of the decode prefetch.

import (
	"encoding/binary"
	"strconv"
)

// ImageHead is what a header probe finds: the picture's dimensions and the
// EXIF orientation tag (0 = none/unknown).
type ImageHead struct {
	Width, Height int
	Orientation   int
}

// ProbeImageHead identifies a picture from the head of its file. The head
// must reach the format's size fields; a short or malformed head answers
// ok=false, never a wrong size.
func ProbeImageHead(head []byte) (ImageHead, bool) {
	switch {
	case isPNG(head):
		return probePNG(head)
	case len(head) >= 6 && (string(head[:6]) == "GIF87a" || string(head[:6]) == "GIF89a"):
		return probeGIF(head)
	case len(head) >= 2 && head[0] == 0xFF && head[1] == 0xD8:
		return probeJPEG(head)
	case len(head) >= 2 && head[0] == 'B' && head[1] == 'M':
		return probeBMP(head)
	case len(head) >= 4 && string(head[:4]) == "qoif":
		return probeQOI(head)
	case len(head) >= 4 && (string(head[:4]) == "II*\x00" || string(head[:4]) == "MM\x00*"):
		return probeTIFF(head)
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return probeWebP(head)
	case len(head) >= 2 && head[0] == 'P' && (head[1] >= '1' && head[1] <= '7' || head[1] == 'F' || head[1] == 'f'):
		return probeNetpbm(head)
	}
	return ImageHead{}, false
}

func probePNG(head []byte) (ImageHead, bool) {
	// The first chunk after the signature must be IHDR; anything else is a
	// malformed file whose bytes are not dimensions.
	if len(head) < 24 || string(head[12:16]) != "IHDR" {
		return ImageHead{}, false
	}
	return ImageHead{Width: int(binary.BigEndian.Uint32(head[16:20])), Height: int(binary.BigEndian.Uint32(head[20:24]))}, true
}

func probeGIF(head []byte) (ImageHead, bool) {
	if len(head) < 10 {
		return ImageHead{}, false
	}
	return ImageHead{Width: int(binary.LittleEndian.Uint16(head[6:8])), Height: int(binary.LittleEndian.Uint16(head[8:10]))}, true
}

// probeJPEG walks the segments to the frame header; the Exif APP1 supplies
// the orientation.
func probeJPEG(head []byte) (ImageHead, bool) {
	info := ImageHead{}
	for i := 2; i+4 <= len(head); {
		if head[i] != 0xFF {
			break
		}
		marker := head[i+1]
		if marker == 0xFF {
			i++
			continue
		}
		if marker >= 0xD0 && marker <= 0xD9 {
			i += 2
			continue
		}
		length := int(binary.BigEndian.Uint16(head[i+2 : i+4]))
		if length < 2 || i+2+length > len(head) {
			break
		}
		payload := head[i+4 : i+2+length]
		if marker == 0xE1 && len(payload) > 6 && string(payload[:6]) == "Exif\x00\x00" {
			info.Orientation = tiffOrientation(payload[6:])
		}
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			if len(payload) >= 5 {
				info.Width = int(binary.BigEndian.Uint16(payload[3:5]))
				info.Height = int(binary.BigEndian.Uint16(payload[1:3]))
			}
			break
		}
		if marker == 0xDA {
			break // SOS: the scan begins, no size fields follow
		}
		i += 2 + length
	}
	if info.Width <= 0 || info.Height <= 0 {
		return ImageHead{}, false
	}
	return info, true
}

func probeBMP(head []byte) (ImageHead, bool) {
	if len(head) < 26 {
		return ImageHead{}, false
	}
	if binary.LittleEndian.Uint32(head[14:18]) < 40 {
		return ImageHead{}, false
	}
	w := int(int32(binary.LittleEndian.Uint32(head[18:22])))
	h := int(int32(binary.LittleEndian.Uint32(head[22:26])))
	if w <= 0 || h == 0 {
		return ImageHead{}, false
	}
	if h < 0 {
		h = -h
	}
	return ImageHead{Width: w, Height: h}, true
}

func probeQOI(head []byte) (ImageHead, bool) {
	if len(head) < 12 {
		return ImageHead{}, false
	}
	w := int(binary.BigEndian.Uint32(head[4:8]))
	h := int(binary.BigEndian.Uint32(head[8:12]))
	if w <= 0 || h <= 0 {
		return ImageHead{}, false
	}
	return ImageHead{Width: w, Height: h}, true
}

func probeTIFF(head []byte) (ImageHead, bool) {
	return tiffHead(head)
}

// tiffHead reads width/height from a TIFF block (or an Exif payload, which
// is a TIFF by another name).
func tiffHead(tiff []byte) (ImageHead, bool) {
	info, order, ifd, count, ok := tiffIFD(tiff)
	if !ok {
		return ImageHead{}, false
	}
	for i := 0; i < count; i++ {
		e := tiff[ifd+2+i*12:]
		tag, typ := order.Uint16(e[0:2]), order.Uint16(e[2:4])
		switch tag {
		case 256:
			info.Width = tiffScalar(typ, order.Uint32(e[8:12]))
		case 257:
			info.Height = tiffScalar(typ, order.Uint32(e[8:12]))
		}
	}
	if info.Width <= 0 || info.Height <= 0 {
		return ImageHead{}, false
	}
	info.Orientation = tiffOrientation(tiff)
	return info, true
}

// tiffOrientation reads the EXIF orientation tag alone; camera IFD0s carry
// it without the size fields, so it is read separately from the dimensions.
func tiffOrientation(tiff []byte) int {
	_, order, ifd, count, ok := tiffIFD(tiff)
	if !ok {
		return 0
	}
	for i := 0; i < count; i++ {
		e := tiff[ifd+2+i*12:]
		if order.Uint16(e[0:2]) == 274 && order.Uint16(e[2:4]) == 3 {
			return int(order.Uint32(e[8:12]) & 0xFFFF)
		}
	}
	return 0
}

// tiffIFD resolves a TIFF block's byte order and first IFD; the caller
// iterates its entries.
func tiffIFD(tiff []byte) (ImageHead, binary.ByteOrder, int, int, bool) {
	if len(tiff) < 8 {
		return ImageHead{}, nil, 0, 0, false
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return ImageHead{}, nil, 0, 0, false
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return ImageHead{}, nil, 0, 0, false
	}
	ifd := int(order.Uint32(tiff[4:8]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return ImageHead{}, nil, 0, 0, false
	}
	count := int(order.Uint16(tiff[ifd : ifd+2]))
	if count > 4096 || ifd+2+count*12 > len(tiff) {
		return ImageHead{}, nil, 0, 0, false
	}
	return ImageHead{}, order, ifd, count, true
}

// tiffScalar reads a SHORT or LONG entry's inline value.
func tiffScalar(typ uint16, val uint32) int {
	switch typ {
	case 3:
		return int(val & 0xFFFF)
	case 4:
		return int(val)
	}
	return 0
}

// probeWebP walks the RIFF chunks: VP8X carries the canvas size, VP8/VP8L
// the frame's.
func probeWebP(head []byte) (ImageHead, bool) {
	for off := 12; off+8 <= len(head); {
		chunk := string(head[off : off+4])
		size := int(binary.LittleEndian.Uint32(head[off+4 : off+8]))
		data := head[off+8:]
		if size > len(data) {
			return ImageHead{}, false
		}
		switch chunk {
		case "VP8X":
			if size < 10 {
				return ImageHead{}, false
			}
			w := 1 + (int(data[4]) | int(data[5])<<8 | int(data[6])<<16)
			h := 1 + (int(data[7]) | int(data[8])<<8 | int(data[9])<<16)
			if w <= 0 || h <= 0 {
				return ImageHead{}, false
			}
			return ImageHead{Width: w, Height: h}, true
		case "VP8 ":
			if size < 10 {
				return ImageHead{}, false
			}
			w := 1 + (int(data[6])|int(data[7])<<8)&0x3FFF
			h := 1 + (int(data[8])|int(data[9])<<8)&0x3FFF
			if w <= 0 || h <= 0 {
				return ImageHead{}, false
			}
			return ImageHead{Width: w, Height: h}, true
		case "VP8L":
			if size < 5 {
				return ImageHead{}, false
			}
			v := binary.LittleEndian.Uint32(data[1:5])
			w := 1 + int(v&0x3FFF)
			h := 1 + int(v>>14&0x3FFF)
			if w <= 0 || h <= 0 {
				return ImageHead{}, false
			}
			return ImageHead{Width: w, Height: h}, true
		}
		off += 8 + size + size&1
	}
	return ImageHead{}, false
}

// probeNetpbm reads the ASCII header: magic, then the two size tokens.
func probeNetpbm(head []byte) (ImageHead, bool) {
	pos := 0
	next := func() ([]byte, bool) {
		for pos < len(head) {
			b := head[pos]
			if b == '#' {
				for pos < len(head) && head[pos] != '\n' {
					pos++
				}
				continue
			}
			if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
				pos++
				continue
			}
			s := pos
			for pos < len(head) && head[pos] != ' ' && head[pos] != '\t' && head[pos] != '\r' && head[pos] != '\n' {
				pos++
			}
			return head[s:pos], true
		}
		return nil, false
	}
	magic, ok := next()
	if !ok {
		return ImageHead{}, false
	}
	if string(magic) == "P7" {
		return probePAM(next)
	}
	wTok, ok := next()
	if !ok {
		return ImageHead{}, false
	}
	hTok, ok := next()
	if !ok {
		return ImageHead{}, false
	}
	w, werr := strconv.Atoi(string(wTok))
	h, herr := strconv.Atoi(string(hTok))
	if werr != nil || herr != nil || w <= 0 || h <= 0 {
		return ImageHead{}, false
	}
	return ImageHead{Width: w, Height: h}, true
}

// probePAM reads WIDTH/HEIGHT from a P7 header's key-value lines.
func probePAM(next func() ([]byte, bool)) (ImageHead, bool) {
	info := ImageHead{}
	for {
		key, ok := next()
		if !ok {
			return ImageHead{}, false
		}
		if string(key) == "ENDHDR" {
			break
		}
		val, ok := next()
		if !ok {
			return ImageHead{}, false
		}
		n, err := strconv.Atoi(string(val))
		if err != nil {
			continue
		}
		switch string(key) {
		case "WIDTH":
			info.Width = n
		case "HEIGHT":
			info.Height = n
		}
		if info.Width > 0 && info.Height > 0 {
			return info, true
		}
	}
	return ImageHead{}, false
}
