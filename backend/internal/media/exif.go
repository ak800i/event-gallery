package media

import (
	"bytes"
	"io"
	"os"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

// exifTimeLayout is the format used by EXIF DateTimeOriginal/DateTime tags.
const exifTimeLayout = "2006:01:02 15:04:05"

// ImageCapturedAt attempts to read the original capture time from an
// image's EXIF metadata (DateTimeOriginal, falling back to DateTime). It
// returns nil without error if no EXIF timestamp is present, which is
// common for screenshots, downloaded images, or formats without EXIF.
func ImageCapturedAt(path string) (*time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if captured := decodeEXIFDateTime(f); captured != nil {
		return captured, nil
	}
	// HEIC/HEIF/AVIF have no APP1 segment for goexif to find; their EXIF is a
	// container item that has to be located first.
	if tiff := heifEXIFTIFF(path); len(tiff) > 0 {
		if captured := decodeEXIFDateTime(bytes.NewReader(tiff)); captured != nil {
			return captured, nil
		}
	}
	return nil, nil
}

func decodeEXIFDateTime(r io.Reader) *time.Time {
	x, err := exif.Decode(r)
	if err != nil {
		// No EXIF data, or unparsable: not a hard error, just no capture time.
		return nil
	}
	t, err := x.DateTime()
	if err != nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}
