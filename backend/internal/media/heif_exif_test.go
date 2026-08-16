package media

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func be32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func box(boxType string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	out := append(be32(uint32(8+len(body))), []byte(boxType)...)
	return append(out, body...)
}

func fullBox(boxType string, version byte, parts ...[]byte) []byte {
	return box(boxType, append([][]byte{{version, 0, 0, 0}}, parts...)...)
}

// tiffWithDateTime builds a minimal little-endian TIFF whose IFD0 carries a
// single DateTime tag, which is what goexif reads back.
func tiffWithDateTime(value string) []byte {
	const dateTimeTag = 0x0132
	const asciiType = 2

	value += "\x00"
	var buf bytes.Buffer
	buf.WriteString("II*\x00")                                  // little-endian TIFF magic
	buf.Write([]byte{0x08, 0x00, 0x00, 0x00})                   // IFD0 at offset 8
	buf.Write([]byte{0x01, 0x00})                               // one entry
	binary.Write(&buf, binary.LittleEndian, uint16(dateTimeTag)) //nolint:errcheck // bytes.Buffer never fails
	binary.Write(&buf, binary.LittleEndian, uint16(asciiType))   //nolint:errcheck
	binary.Write(&buf, binary.LittleEndian, uint32(len(value)))  //nolint:errcheck
	// Value is longer than 4 bytes, so the field holds an offset instead:
	// 8 (header) + 2 (count) + 12 (entry) + 4 (next-IFD pointer).
	binary.Write(&buf, binary.LittleEndian, uint32(26)) //nolint:errcheck
	buf.Write([]byte{0, 0, 0, 0})                       // no next IFD
	buf.WriteString(value)
	return buf.Bytes()
}

// heifWithEXIF assembles the smallest file shaped like a HEIC still: an Exif
// item declared in iinf, located by iloc, with its payload living in mdat.
func heifWithEXIF(tiff []byte) []byte {
	const exifItemID = 1
	const offsetPlaceholder = 0xdeadbeef

	// The payload is prefixed with the offset from here to the TIFF header.
	payload := append(be32(0), tiff...)

	infe := fullBox("infe", 2, be16(exifItemID), be16(0), []byte("Exif"), []byte{0})
	iinf := fullBox("iinf", 0, be16(1), infe)
	iloc := fullBox("iloc", 1,
		[]byte{0x44, 0x00}, // 4-byte offsets and lengths, no base offset or index
		be16(1),            // item count
		be16(exifItemID),
		be16(0), // construction method 0: offsets address the file
		be16(0), // data reference index
		be16(1), // one extent
		be32(offsetPlaceholder),
		be32(uint32(len(payload))),
	)
	meta := fullBox("meta", 0, box("hdlr"), iinf, iloc)
	ftyp := box("ftyp", []byte("heic"), be32(0), []byte("mif1heic"))

	// mdat's payload starts after every preceding box plus its own header.
	extentOffset := uint32(len(ftyp) + len(meta) + 8)
	meta = bytes.Replace(meta, be32(offsetPlaceholder), be32(extentOffset), 1)

	return bytes.Join([][]byte{ftyp, meta, box("mdat", payload)}, nil)
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// HEIC keeps EXIF in a container item rather than a JPEG APP1 segment, so
// goexif alone finds nothing. Verified against real tiled and non-tiled HEIC;
// this fixture is synthetic because ffmpeg cannot mux HEIC.
func TestImageCapturedAtReadsHEIFContainerEXIF(t *testing.T) {
	path := writeTemp(t, "photo.heic", heifWithEXIF(tiffWithDateTime("2019:07:04 12:34:56")))

	captured, err := ImageCapturedAt(path)
	if err != nil {
		t.Fatalf("ImageCapturedAt: %v", err)
	}
	if captured == nil {
		t.Fatal("expected a capture time from the container EXIF item")
	}
	if got := captured.UTC().Format("2006-01-02 15:04:05"); got != "2019-07-04 12:34:56" {
		t.Errorf("capture time = %s, want 2019-07-04 12:34:56", got)
	}
}

// A photo without a readable capture time is normal, so every malformed shape
// has to degrade to "no timestamp" rather than erroring or panicking.
func TestHEIFEXIFDegradesQuietly(t *testing.T) {
	full := heifWithEXIF(tiffWithDateTime("2019:07:04 12:34:56"))

	cases := map[string][]byte{
		"empty":              {},
		"header only":        full[:8],
		"truncated mid-meta": full[:len(full)/2],
		"no exif item":       box("ftyp", []byte("heic")),
		"zeroed":             make([]byte, len(full)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTemp(t, "broken.heic", data)
			if tiff := heifEXIFTIFF(path); tiff != nil {
				t.Errorf("expected no EXIF payload, got %d bytes", len(tiff))
			}
			captured, err := ImageCapturedAt(path)
			if err != nil || captured != nil {
				t.Errorf("expected no capture time and no error, got %v / %v", captured, err)
			}
		})
	}
}

// An extent pointing past the end of the file must be refused rather than
// trusted into an out-of-range read.
func TestHEIFEXIFRejectsOutOfRangeExtent(t *testing.T) {
	full := heifWithEXIF(tiffWithDateTime("2019:07:04 12:34:56"))
	payloadLen := uint32(len(tiffWithDateTime("2019:07:04 12:34:56")) + 4)
	beyondEOF := bytes.Replace(full, be32(payloadLen), be32(1<<30), 1)

	path := writeTemp(t, "overrun.heic", beyondEOF)
	if tiff := heifEXIFTIFF(path); tiff != nil {
		t.Errorf("expected the oversized extent to be refused, got %d bytes", len(tiff))
	}
}
