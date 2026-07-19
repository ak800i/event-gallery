package media

import (
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

	x, err := exif.Decode(f)
	if err != nil {
		// No EXIF data, or unparsable: not a hard error, just no capture time.
		return nil, nil
	}

	if t, err := x.DateTime(); err == nil {
		utc := t.UTC()
		return &utc, nil
	}
	return nil, nil
}
