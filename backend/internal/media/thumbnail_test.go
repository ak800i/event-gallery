package media

import (
	"path/filepath"
	"testing"
)

// The pure-Go decoder materializes a whole still at 4 bytes per pixel, so a
// 48 MP JPEG costs ~194 MB in one allocation regardless of how few ingest
// workers run. This guard is what keeps that allocation from ever happening
// on a memory-capped host.
func TestShouldDecodeInProcess(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.jpg")
	writeJPEG(t, small, 40, 30) // 1200 pixels

	cases := []struct {
		name      string
		path      string
		maxPixels int64
		want      bool
	}{
		{"cap disabled by zero", small, 0, true},
		{"cap disabled by negative", small, -1, true},
		{"well under cap", small, 1 << 20, true},
		{"exactly at cap", small, 1200, true},
		{"one pixel over cap", small, 1199, false},
		// An unreadable header must not silently skip the fast path: letting
		// it run produces the real decode error, which is what selects the
		// ffmpeg fallback today.
		{"unreadable header falls through to the Go path", filepath.Join(dir, "missing.jpg"), 10, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldDecodeInProcess(c.path, c.maxPixels); got != c.want {
				t.Errorf("shouldDecodeInProcess(%q, %d) = %v, want %v", c.path, c.maxPixels, got, c.want)
			}
		})
	}
}
