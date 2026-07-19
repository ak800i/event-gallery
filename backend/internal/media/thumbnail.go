package media

import (
	"fmt"
	"image"
	"os"

	"github.com/disintegration/imaging"

	// Blank-imported to register the WebP decoder with the standard
	// image.Decode/DecodeConfig machinery. We only ever decode WebP
	// (uploaded by guests); we never need to encode it.
	_ "golang.org/x/image/webp"
)

// ImageDimensions returns the pixel width and height of an image file
// without fully decoding pixel data, using whichever decoder is registered
// for its format (jpeg/png/gif/webp/bmp/tiff).
func ImageDimensions(path string) (width, height int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("decode image config: %w", err)
	}
	return cfg.Width, cfg.Height, nil
}

// GenerateImageThumbnail creates a JPEG thumbnail of the source image, with
// its longest edge scaled down to maxDimension (smaller images are left
// unchanged), honoring EXIF orientation. It also returns the original
// image's full-resolution dimensions.
func GenerateImageThumbnail(srcPath, dstPath string, maxDimension int) (width, height int, err error) {
	img, err := imaging.Open(srcPath, imaging.AutoOrientation(true))
	if err != nil {
		return 0, 0, fmt.Errorf("open image: %w", err)
	}
	bounds := img.Bounds()
	width, height = bounds.Dx(), bounds.Dy()

	thumb := img
	if width > maxDimension || height > maxDimension {
		thumb = imaging.Fit(img, maxDimension, maxDimension, imaging.Lanczos)
	}

	if err := imaging.Save(thumb, dstPath, imaging.JPEGQuality(85)); err != nil {
		return 0, 0, fmt.Errorf("save thumbnail: %w", err)
	}
	return width, height, nil
}
