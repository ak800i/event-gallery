package media

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireFFmpeg skips the test if ffmpeg/ffprobe are not installed, which
// keeps the suite portable across environments that lack them (though the
// production Docker image always includes them).
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed, skipping video processing test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed, skipping video processing test")
	}
}

func generateTestVideo(t *testing.T, path string, seconds int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-f", "lavfi",
		"-i", "testsrc=duration="+itoa(seconds)+":size=64x48:rate=5",
		"-pix_fmt", "yuv420p",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate test video failed: %v\n%s", err, out)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestProbeVideo(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "clip.mp4")
	generateTestVideo(t, path, 2)

	info, err := ProbeVideo(context.Background(), path)
	if err != nil {
		t.Fatalf("probe video: %v", err)
	}
	if info.Width != 64 || info.Height != 48 {
		t.Errorf("expected 64x48, got %dx%d", info.Width, info.Height)
	}
	if info.DurationSeconds < 1.5 || info.DurationSeconds > 3 {
		t.Errorf("expected ~2s duration, got %f", info.DurationSeconds)
	}
}

func TestGenerateVideoThumbnail(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	generateTestVideo(t, src, 2)
	dst := filepath.Join(dir, "thumb.jpg")

	if err := GenerateVideoThumbnail(context.Background(), src, dst, 100, 2); err != nil {
		t.Fatalf("generate video thumbnail: %v", err)
	}
	w, h, err := ImageDimensions(dst)
	if err != nil {
		t.Fatalf("thumbnail dimensions: %v", err)
	}
	if w > 100 || h > 100 {
		t.Errorf("expected thumbnail within 100px, got %dx%d", w, h)
	}
}
