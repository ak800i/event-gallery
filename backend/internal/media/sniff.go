// Package media handles everything related to inspecting and deriving
// artifacts from an uploaded file: MIME sniffing (never trusting
// client-declared content types), thumbnail generation, EXIF/ffprobe
// capture-time extraction, and whole-file hashing.
package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"wedding-gallery/backend/internal/models"
)

// ErrUnsupportedType is returned when a file's sniffed content does not
// match any allow-listed image or video type.
type ErrUnsupportedType struct {
	Sniffed string
}

func (e *ErrUnsupportedType) Error() string {
	return fmt.Sprintf("unsupported media type: %s", e.Sniffed)
}

// sniffSignature holds a magic-byte matcher for one supported format.
type sniffSignature struct {
	mime  string
	kind  models.MediaKind
	match func([]byte) bool
}

func hasPrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && bytes.Equal(b[:len(prefix)], prefix)
}

func isFtypBrand(b []byte, brands ...string) bool {
	// ISO base media file format: bytes 4-8 are "ftyp", followed by a
	// 4-byte major brand, then minor version, then a list of compatible
	// brands. We check the major brand and the first compatible brand.
	if len(b) < 12 || !hasPrefix(b[4:], []byte("ftyp")) {
		return false
	}
	major := string(b[8:12])
	for _, br := range brands {
		if major == br {
			return true
		}
	}
	if len(b) >= 16 {
		for i := 16; i+4 <= len(b) && i < 32; i += 4 {
			compat := string(b[i : i+4])
			for _, br := range brands {
				if compat == br {
					return true
				}
			}
		}
	}
	return false
}

var signatures = []sniffSignature{
	{mime: "image/jpeg", kind: models.KindImage, match: func(b []byte) bool { return hasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) }},
	{mime: "image/png", kind: models.KindImage, match: func(b []byte) bool {
		return hasPrefix(b, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A})
	}},
	{mime: "image/gif", kind: models.KindImage, match: func(b []byte) bool {
		return hasPrefix(b, []byte("GIF87a")) || hasPrefix(b, []byte("GIF89a"))
	}},
	{mime: "image/webp", kind: models.KindImage, match: func(b []byte) bool {
		return len(b) >= 12 && hasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
	}},
	{mime: "image/heic", kind: models.KindImage, match: func(b []byte) bool {
		return isFtypBrand(b, "heic", "heix", "heim", "heis", "hevc", "hevx")
	}},
	{mime: "image/heif", kind: models.KindImage, match: func(b []byte) bool {
		return isFtypBrand(b, "mif1", "msf1")
	}},
	{mime: "video/mp4", kind: models.KindVideo, match: func(b []byte) bool {
		return isFtypBrand(b, "isom", "iso2", "mp41", "mp42", "avc1", "M4V ", "dash")
	}},
	{mime: "video/quicktime", kind: models.KindVideo, match: func(b []byte) bool {
		return isFtypBrand(b, "qt  ")
	}},
	{mime: "video/webm", kind: models.KindVideo, match: func(b []byte) bool {
		return hasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3})
	}},
}

// Sniff reads the leading bytes of a file and determines its true content
// type based on magic-number signatures, ignoring any client-supplied
// content type or filename extension. It returns ErrUnsupportedType if no
// known signature matches.
func Sniff(path string) (mimeType string, kind models.MediaKind, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", fmt.Errorf("open for sniffing: %w", err)
	}
	defer f.Close()

	buf := make([]byte, 64)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", "", fmt.Errorf("read for sniffing: %w", err)
	}
	buf = buf[:n]

	for _, sig := range signatures {
		if sig.match(buf) {
			return sig.mime, sig.kind, nil
		}
	}
	return "", "", &ErrUnsupportedType{Sniffed: hex.EncodeToString(buf)}
}

// IsAllowed checks whether the given sniffed MIME type is present in the
// provided allow-lists.
func IsAllowed(mimeType string, kind models.MediaKind, allowedImages, allowedVideos []string) bool {
	list := allowedImages
	if kind == models.KindVideo {
		list = allowedVideos
	}
	for _, m := range list {
		if m == mimeType {
			return true
		}
	}
	return false
}

// SHA256File computes the hex-encoded SHA-256 digest of a file on disk,
// streaming it to avoid loading the whole file into memory.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open for hashing: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
