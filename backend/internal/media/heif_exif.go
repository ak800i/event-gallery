package media

import (
	"encoding/binary"
	"os"
)

// ISOBMFF stills keep their metadata boxes small; these caps stop a malformed
// or hostile file from turning a header walk into a large allocation.
const (
	maxMetaBoxBytes  = 4 << 20
	maxEXIFItemBytes = 1 << 20
)

// heifEXIFTIFF returns the TIFF-formatted EXIF payload that ISOBMFF stills
// (HEIC/HEIF/AVIF) store as a container item, which is invisible to any
// JPEG/TIFF reader because there is no APP1 segment to find. Returns nil
// whenever the file has no such item or is not shaped as expected; a photo
// without a readable capture time is normal, not an error.
//
// ffprobe is not usable here: iPhone photos are tile grids, and ffmpeg hangs
// their EXIF off the stream group, which no ffprobe section exposes.
func heifEXIFTIFF(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil
	}
	fileSize := stat.Size()

	meta, ok := readTopLevelBox(f, fileSize, "meta", maxMetaBoxBytes)
	if !ok || len(meta) < 4 {
		return nil
	}
	// meta is a FullBox: its children start after version+flags.
	children := meta[4:]

	iinf, ok := findBox(children, "iinf")
	if !ok {
		return nil
	}
	itemID, ok := exifItemID(iinf)
	if !ok {
		return nil
	}
	iloc, ok := findBox(children, "iloc")
	if !ok {
		return nil
	}
	offset, length, ok := itemExtent(iloc, itemID)
	if !ok || length <= 4 || length > maxEXIFItemBytes || offset < 0 || offset+length > fileSize {
		return nil
	}

	payload := make([]byte, length)
	if _, err := f.ReadAt(payload, offset); err != nil {
		return nil
	}
	// An Exif item opens with a 4-byte big-endian offset to the TIFF header.
	skip := int64(binary.BigEndian.Uint32(payload[:4])) + 4
	if skip < 4 || skip >= int64(len(payload)) {
		return nil
	}
	return payload[skip:]
}

// nextBox splits the leading box out of b. ok is false when b does not hold a
// complete, self-consistent box.
func nextBox(b []byte) (boxType string, payload, rest []byte, ok bool) {
	if len(b) < 8 {
		return "", nil, nil, false
	}
	size := int64(binary.BigEndian.Uint32(b[0:4]))
	boxType = string(b[4:8])
	header := int64(8)
	switch size {
	case 1:
		if len(b) < 16 {
			return "", nil, nil, false
		}
		size = int64(binary.BigEndian.Uint64(b[8:16]))
		header = 16
	case 0:
		size = int64(len(b))
	}
	if size < header || size > int64(len(b)) {
		return "", nil, nil, false
	}
	return boxType, b[header:size], b[size:], true
}

func findBox(b []byte, want string) ([]byte, bool) {
	for len(b) > 0 {
		boxType, payload, rest, ok := nextBox(b)
		if !ok {
			return nil, false
		}
		if boxType == want {
			return payload, true
		}
		b = rest
	}
	return nil, false
}

// readTopLevelBox walks the file's outermost boxes by header only, so a
// multi-megabyte mdat is skipped rather than read.
func readTopLevelBox(f *os.File, fileSize int64, want string, maxBytes int64) ([]byte, bool) {
	head := make([]byte, 16)
	for pos := int64(0); pos+8 <= fileSize; {
		if _, err := f.ReadAt(head[:8], pos); err != nil {
			return nil, false
		}
		size := int64(binary.BigEndian.Uint32(head[0:4]))
		boxType := string(head[4:8])
		header := int64(8)
		switch size {
		case 1:
			if _, err := f.ReadAt(head[8:16], pos+8); err != nil {
				return nil, false
			}
			size = int64(binary.BigEndian.Uint64(head[8:16]))
			header = 16
		case 0:
			size = fileSize - pos
		}
		if size < header || pos > fileSize-size {
			return nil, false
		}
		if boxType == want {
			length := size - header
			if length > maxBytes {
				return nil, false
			}
			payload := make([]byte, length)
			if _, err := f.ReadAt(payload, pos+header); err != nil {
				return nil, false
			}
			return payload, true
		}
		pos += size
	}
	return nil, false
}

// exifItemID finds the item carrying EXIF in an iinf box. Only infe versions
// 2 and 3 declare an item_type, and versions 0/1 predate HEIF entirely.
func exifItemID(iinf []byte) (uint32, bool) {
	if len(iinf) < 4 {
		return 0, false
	}
	version := iinf[0]
	b := iinf[4:]

	var count uint32
	if version == 0 {
		if len(b) < 2 {
			return 0, false
		}
		count, b = uint32(binary.BigEndian.Uint16(b[:2])), b[2:]
	} else {
		if len(b) < 4 {
			return 0, false
		}
		count, b = binary.BigEndian.Uint32(b[:4]), b[4:]
	}

	for i := uint32(0); i < count; i++ {
		boxType, payload, rest, ok := nextBox(b)
		if !ok {
			return 0, false
		}
		b = rest
		if boxType != "infe" || len(payload) < 4 {
			continue
		}
		entry := payload[4:]
		var id uint32
		switch {
		case payload[0] == 2:
			if len(entry) < 8 {
				continue
			}
			id, entry = uint32(binary.BigEndian.Uint16(entry[0:2])), entry[4:]
		case payload[0] >= 3:
			if len(entry) < 10 {
				continue
			}
			id, entry = binary.BigEndian.Uint32(entry[0:4]), entry[6:]
		default:
			continue
		}
		if string(entry[0:4]) == "Exif" {
			return id, true
		}
	}
	return 0, false
}

// itemExtent resolves an item's first extent to an absolute file offset.
func itemExtent(iloc []byte, wantID uint32) (offset, length int64, ok bool) {
	if len(iloc) < 6 {
		return 0, 0, false
	}
	version := iloc[0]
	b := iloc[4:]

	offsetSize, lengthSize := int(b[0]>>4), int(b[0]&0x0f)
	baseOffsetSize := int(b[1] >> 4)
	indexSize := 0
	if version == 1 || version == 2 {
		indexSize = int(b[1] & 0x0f)
	}
	b = b[2:]

	var count uint32
	if version < 2 {
		if len(b) < 2 {
			return 0, 0, false
		}
		count, b = uint32(binary.BigEndian.Uint16(b[:2])), b[2:]
	} else {
		if len(b) < 4 {
			return 0, 0, false
		}
		count, b = binary.BigEndian.Uint32(b[:4]), b[4:]
	}

	for i := uint32(0); i < count; i++ {
		var id uint32
		if version < 2 {
			if len(b) < 2 {
				return 0, 0, false
			}
			id, b = uint32(binary.BigEndian.Uint16(b[:2])), b[2:]
		} else {
			if len(b) < 4 {
				return 0, 0, false
			}
			id, b = binary.BigEndian.Uint32(b[:4]), b[4:]
		}

		construction := uint16(0)
		if version == 1 || version == 2 {
			if len(b) < 2 {
				return 0, 0, false
			}
			construction, b = binary.BigEndian.Uint16(b[:2])&0x0f, b[2:]
		}
		if len(b) < 2 {
			return 0, 0, false
		}
		b = b[2:] // data_reference_index

		baseOffset, rest, ok := readUint(b, baseOffsetSize)
		if !ok {
			return 0, 0, false
		}
		b = rest
		if len(b) < 2 {
			return 0, 0, false
		}
		extentCount := binary.BigEndian.Uint16(b[:2])
		b = b[2:]

		for e := uint16(0); e < extentCount; e++ {
			if indexSize > 0 {
				if len(b) < indexSize {
					return 0, 0, false
				}
				b = b[indexSize:]
			}
			extentOffset, rest, ok := readUint(b, offsetSize)
			if !ok {
				return 0, 0, false
			}
			b = rest
			extentLength, rest, ok := readUint(b, lengthSize)
			if !ok {
				return 0, 0, false
			}
			b = rest

			// Construction method 0 addresses the file directly; the others
			// index into idat/another item, which this reader does not follow.
			if id == wantID && construction == 0 && e == 0 {
				if baseOffset > 1<<62 || extentOffset > 1<<62 || extentLength > 1<<62 {
					return 0, 0, false
				}
				return int64(baseOffset + extentOffset), int64(extentLength), true
			}
		}
	}
	return 0, 0, false
}

// readUint reads a big-endian integer of the given byte width. Width 0 is
// legal in iloc and means the field is absent with an implied value of zero.
func readUint(b []byte, width int) (uint64, []byte, bool) {
	if width == 0 {
		return 0, b, true
	}
	if width > 8 || len(b) < width {
		return 0, nil, false
	}
	var v uint64
	for _, c := range b[:width] {
		v = v<<8 | uint64(c)
	}
	return v, b[width:], true
}
