package store

import (
	"context"
	"testing"
	"time"

	"wedding-gallery/backend/internal/models"
)

func TestVideoMetadataListAndDimensionUpdate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	video := sampleMedia("video-id", "video-sha", time.Now())
	video.Kind = models.KindVideo
	video.MimeType = "video/mp4"
	video.OriginalFilename = "portrait.mp4"
	video.StoredFilename = "video-id.mp4"
	video.Width = 1920
	video.Height = 1080
	if err := st.InsertMedia(ctx, video); err != nil {
		t.Fatalf("insert video: %v", err)
	}

	records, err := st.ListVideoMetadata(ctx)
	if err != nil {
		t.Fatalf("list metadata: %v", err)
	}
	if len(records) != 1 || records[0].StoredFilename != "video-id.mp4" {
		t.Fatalf("unexpected records: %+v", records)
	}
	if err := st.UpdateVideoDimensions(ctx, "video-id", 1080, 1920); err != nil {
		t.Fatalf("update dimensions: %v", err)
	}
	updated, err := st.GetByID(ctx, "video-id", "")
	if err != nil {
		t.Fatalf("get updated video: %v", err)
	}
	if updated.Width != 1080 || updated.Height != 1920 {
		t.Fatalf("got %dx%d, want 1080x1920", updated.Width, updated.Height)
	}
}
