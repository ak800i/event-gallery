package media

import (
	"os"
	"path/filepath"
	"testing"

	"event-gallery/backend/internal/models"
)

func purgeTestProcessor(t *testing.T) (*Processor, models.MediaItem) {
	t.Helper()
	processor := NewProcessor(t.TempDir(), 800, nil, nil)
	if err := processor.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	item := models.MediaItem{ID: "media-id", StoredFilename: "media-id.jpg", Status: models.StatusTrashed}
	if err := os.WriteFile(processor.OriginalPath(item.StoredFilename), []byte("original"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(processor.ThumbnailPath(item.ID), []byte("thumbnail"), 0o640); err != nil {
		t.Fatal(err)
	}
	return processor, item
}

func TestPurgeStageRestore(t *testing.T) {
	processor, item := purgeTestProcessor(t)
	stage, err := processor.StageForPurge(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(processor.OriginalPath(item.StoredFilename)); !os.IsNotExist(err) {
		t.Fatal("original should be staged")
	}
	stages, problems := LoadPurgeStages(processor)
	if len(problems) != 0 || len(stages) != 1 || stages[0].StoredFilename() != item.StoredFilename {
		t.Fatalf("unexpected loaded stages/problems: %v %v", stages, problems)
	}
	if err := stage.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(processor.OriginalPath(item.StoredFilename)); err != nil {
		t.Fatalf("original not restored: %v", err)
	}
	if _, err := os.Stat(processor.ThumbnailPath(item.ID)); err != nil {
		t.Fatalf("thumbnail not restored: %v", err)
	}
}

func TestPurgeStageFinalize(t *testing.T) {
	processor, item := purgeTestProcessor(t)
	stage, err := processor.StageForPurge(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.Finalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage.Dir); !os.IsNotExist(err) {
		t.Fatal("purge stage should be removed")
	}
	if _, err := os.Stat(processor.OriginalPath(item.StoredFilename)); !os.IsNotExist(err) {
		t.Fatal("original should be permanently removed")
	}
}

func TestLoadPurgeStagesIsolatesMalformedStage(t *testing.T) {
	processor := NewProcessor(t.TempDir(), 800, nil, nil)
	bad := filepath.Join(processor.PurgingDir(), "bad")
	if err := os.MkdirAll(bad, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, purgeManifestName), []byte("not-json"), 0o640); err != nil {
		t.Fatal(err)
	}
	stages, problems := LoadPurgeStages(processor)
	if len(stages) != 0 || len(problems) != 1 {
		t.Fatalf("expected isolated malformed stage, got stages=%d problems=%d", len(stages), len(problems))
	}
}
