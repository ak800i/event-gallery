package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"event-gallery/backend/internal/models"
)

const (
	purgeManifestName    = "manifest.json"
	purgeManifestVersion = 1
)

type purgeManifest struct {
	Version        int    `json:"version"`
	MediaID        string `json:"mediaId"`
	StoredFilename string `json:"storedFilename"`
}

// PurgeStage is a recoverable same-filesystem staging area. If the database
// row still exists after a crash, Restore returns files to their live paths;
// if the row is gone, Finalize permanently removes the staged files.
type PurgeStage struct {
	Dir       string
	manifest  purgeManifest
	processor *Processor
}

func (p *Processor) PurgingDir() string { return filepath.Join(p.MediaDir, ".purging") }

func (p *Processor) StageForPurge(item models.MediaItem) (*PurgeStage, error) {
	if item.ID == "" || filepath.Base(item.ID) != item.ID || item.StoredFilename == "" || filepath.Base(item.StoredFilename) != item.StoredFilename {
		return nil, fmt.Errorf("invalid media purge identity")
	}
	if err := os.MkdirAll(p.PurgingDir(), 0o750); err != nil {
		return nil, fmt.Errorf("create purging directory: %w", err)
	}
	dir, err := os.MkdirTemp(p.PurgingDir(), "purge-")
	if err != nil {
		return nil, fmt.Errorf("create purge stage: %w", err)
	}
	stage := &PurgeStage{
		Dir: dir,
		manifest: purgeManifest{Version: purgeManifestVersion, MediaID: item.ID, StoredFilename: item.StoredFilename},
		processor: p,
	}
	if err := writePurgeManifest(dir, stage.manifest); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := syncDir(p.PurgingDir()); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("sync purging directory: %w", err)
	}
	if err := moveIfExists(p.OriginalPath(item.StoredFilename), stage.originalStagePath()); err != nil {
		_ = stage.Restore()
		return nil, fmt.Errorf("stage original: %w", err)
	}
	if err := moveIfExists(p.ThumbnailPath(item.ID), stage.thumbnailStagePath()); err != nil {
		_ = stage.Restore()
		return nil, fmt.Errorf("stage thumbnail: %w", err)
	}
	if err := syncDir(dir); err != nil {
		_ = stage.Restore()
		return nil, fmt.Errorf("sync purge stage: %w", err)
	}
	return stage, nil
}

// LoadPurgeStages returns valid stages and isolates malformed entries as
// problems so one corrupt directory cannot block recovery of every other item.
func LoadPurgeStages(processor *Processor) (stages []*PurgeStage, problems []error) {
	entries, err := os.ReadDir(processor.PurgingDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{fmt.Errorf("read purge stages: %w", err)}
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(processor.PurgingDir(), entry.Name())
		raw, err := os.ReadFile(filepath.Join(dir, purgeManifestName))
		if errors.Is(err, os.ErrNotExist) {
			children, readErr := os.ReadDir(dir)
			if readErr == nil && len(children) == 0 {
				_ = os.Remove(dir)
				continue
			}
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("read purge manifest %s: %w", entry.Name(), err))
			continue
		}
		var manifest purgeManifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			problems = append(problems, fmt.Errorf("decode purge manifest %s: %w", entry.Name(), err))
			continue
		}
		if manifest.Version != purgeManifestVersion || manifest.MediaID == "" || filepath.Base(manifest.MediaID) != manifest.MediaID || manifest.StoredFilename == "" || filepath.Base(manifest.StoredFilename) != manifest.StoredFilename {
			problems = append(problems, fmt.Errorf("invalid purge manifest %s", entry.Name()))
			continue
		}
		stages = append(stages, &PurgeStage{Dir: dir, manifest: manifest, processor: processor})
	}
	return stages, problems
}

func (s *PurgeStage) MediaID() string        { return s.manifest.MediaID }
func (s *PurgeStage) StoredFilename() string { return s.manifest.StoredFilename }

func (s *PurgeStage) originalStagePath() string {
	return filepath.Join(s.Dir, "original"+filepath.Ext(s.manifest.StoredFilename))
}
func (s *PurgeStage) thumbnailStagePath() string { return filepath.Join(s.Dir, "thumbnail.jpg") }
func (s *PurgeStage) manifestPath() string       { return filepath.Join(s.Dir, purgeManifestName) }

func (s *PurgeStage) Restore() error {
	if err := restoreIfExists(s.originalStagePath(), s.processor.OriginalPath(s.manifest.StoredFilename)); err != nil {
		return err
	}
	if err := restoreIfExists(s.thumbnailStagePath(), s.processor.ThumbnailPath(s.manifest.MediaID)); err != nil {
		return err
	}
	if err := syncDir(s.processor.OriginalsDir()); err != nil {
		return err
	}
	if err := syncDir(s.processor.ThumbnailsDir()); err != nil {
		return err
	}
	return s.removeManifestAndDir()
}

func (s *PurgeStage) Finalize() error {
	for _, path := range []string{s.originalStagePath(), s.thumbnailStagePath()} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := syncDir(s.Dir); err != nil {
		return err
	}
	return s.removeManifestAndDir()
}

func (s *PurgeStage) removeManifestAndDir() error {
	if err := os.Remove(s.manifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(s.Dir); err != nil {
		return err
	}
	if err := os.Remove(s.Dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(s.processor.PurgingDir())
}

func writePurgeManifest(dir string, manifest purgeManifest) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, purgeManifestName+".tmp")
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("create purge manifest: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return fmt.Errorf("write purge manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync purge manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, purgeManifestName)); err != nil {
		return err
	}
	return syncDir(dir)
}

func moveIfExists(src, dst string) error {
	if err := os.Rename(src, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDir(filepath.Dir(src)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func restoreIfExists(src, dst string) error {
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("refusing to overwrite restored media path %s", dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
