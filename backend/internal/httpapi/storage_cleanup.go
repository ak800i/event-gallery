package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"event-gallery/backend/internal/media"
	"event-gallery/backend/internal/models"
)

var errMediaNotTrashed = errors.New("media must be trashed before permanent deletion")

func (s *Server) purgeMedia(ctx context.Context, ids []string, actor string) ([]string, error) {
	seen := make(map[string]struct{}, len(ids))
	var items []models.MediaItem
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		item, err := s.store.GetByID(ctx, id, "")
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if item.Status != models.StatusTrashed {
			return nil, fmt.Errorf("%w: %s", errMediaNotTrashed, id)
		}
		items = append(items, *item)
	}

	stages := make(map[string]*media.PurgeStage, len(items))
	for _, item := range items {
		stage, err := s.processor.StageForPurge(item)
		if err != nil {
			for _, staged := range stages {
				_ = staged.Restore()
			}
			return nil, err
		}
		stages[item.ID] = stage
	}

	changed, err := s.store.PurgeTrashed(ctx, items, actor)
	if err != nil {
		for _, stage := range stages {
			_ = stage.Restore()
		}
		return nil, err
	}
	changedSet := make(map[string]struct{}, len(changed))
	for _, id := range changed {
		changedSet[id] = struct{}{}
	}
	for id, stage := range stages {
		if _, deleted := changedSet[id]; deleted {
			if err := stage.Finalize(); err != nil {
				slog.Warn("failed to finalize purge stage; janitor will retry", "media_id", id, "error", err)
			}
		} else if err := stage.Restore(); err != nil {
			slog.Error("failed to restore uncommitted purge stage", "media_id", id, "error", err)
		}
	}
	return changed, nil
}

func (s *Server) reconcilePurgeStages(ctx context.Context) {
	stages, problems := media.LoadPurgeStages(s.processor)
	for _, problem := range problems {
		slog.Error("invalid purge recovery stage left for manual inspection", "error", problem)
	}
	for _, stage := range stages {
		item, err := s.store.GetByID(ctx, stage.MediaID(), "")
		switch {
		case err == nil:
			if item.StoredFilename != stage.StoredFilename() {
				slog.Error("purge stage does not match database row; leaving for manual inspection", "media_id", stage.MediaID())
				continue
			}
			if err := stage.Restore(); err != nil {
				slog.Error("failed to restore interrupted purge", "media_id", stage.MediaID(), "error", err)
			} else {
				slog.Info("restored interrupted purge", "media_id", stage.MediaID())
			}
		case errors.Is(err, sql.ErrNoRows):
			if err := stage.Finalize(); err != nil {
				slog.Error("failed to finalize interrupted purge", "media_id", stage.MediaID(), "error", err)
			} else {
				slog.Info("finalized interrupted purge", "media_id", stage.MediaID())
			}
		default:
			slog.Error("failed to reconcile purge stage", "media_id", stage.MediaID(), "error", err)
		}
	}
}

func (s *Server) runStorageCleanup(stop <-chan struct{}) {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	go func() {
		select {
		case <-stop:
			rootCancel()
		case <-rootCtx.Done():
		}
	}()
	run := func() {
		ctx, cancel := context.WithTimeout(rootCtx, 15*time.Minute)
		defer cancel()
		s.reconcilePurgeStages(ctx)
		s.purgeExpiredTrash(ctx)
		s.cleanupIncompleteTusUploads(ctx)
	}
	run()
	ticker := time.NewTicker(s.cfg.StorageCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			run()
		case <-rootCtx.Done():
			return
		}
	}
}

func (s *Server) purgeExpiredTrash(ctx context.Context) {
	if s.cfg.TrashRetention == 0 {
		return
	}
	cutoff := time.Now().Add(-s.cfg.TrashRetention)
	for batch := 0; batch < 10; batch++ {
		items, err := s.store.ListTrashedBefore(ctx, cutoff, 100)
		if err != nil {
			slog.Error("failed to list expired trash", "error", err)
			return
		}
		if len(items) == 0 {
			return
		}
		ids := make([]string, 0, len(items))
		for _, item := range items {
			ids = append(ids, item.ID)
		}
		changed, err := s.purgeMedia(ctx, ids, "system")
		if err != nil {
			slog.Error("failed to purge expired trash", "error", err)
			return
		}
		slog.Info("purged expired trash", "items", len(changed), "retention_hours", s.cfg.TrashRetention.Hours())
		if len(items) < 100 {
			return
		}
	}
}

const (
	maxTusScanEntries = 10000
	maxTusDeletes     = 100
	maxTusSidecarSize = 64 * 1024
)

type tusFileInfo struct {
	ID             string `json:"ID"`
	Size           int64  `json:"Size"`
	SizeIsDeferred bool   `json:"SizeIsDeferred"`
	Storage        struct {
		Type     string `json:"Type"`
		Path     string `json:"Path"`
		InfoPath string `json:"InfoPath"`
	} `json:"Storage"`
}

type tusCandidate struct {
	id       string
	size     int64
	activity time.Time
}

func (s *Server) cleanupIncompleteTusUploads(ctx context.Context) {
	if s.cfg.TusIncompleteRetention == 0 {
		return
	}
	dir, err := os.Open(s.cfg.TusUploadDir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		slog.Error("failed to scan tus upload directory", "error", err)
		return
	}
	entries, err := dir.ReadDir(maxTusScanEntries)
	dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		slog.Error("failed to read tus upload directory", "error", err)
		return
	}
	cutoff := time.Now().Add(-s.cfg.TusIncompleteRetention)
	attempts := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if attempts >= maxTusDeletes || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".info") {
			continue
		}
		candidate, err := inspectTusCandidate(s.cfg.TusUploadDir, entry.Name())
		if err != nil {
			slog.Warn("invalid tus sidecar retained", "sidecar", entry.Name(), "error", err)
			continue
		}
		if candidate == nil || !candidate.activity.Before(cutoff) {
			continue
		}
		// Re-read immediately before DELETE. Any resumed write or fresh lock
		// invalidates this pass and leaves the upload untouched.
		current, err := inspectTusCandidate(s.cfg.TusUploadDir, entry.Name())
		if err != nil || current == nil || current.size != candidate.size || !current.activity.Equal(candidate.activity) || !current.activity.Before(cutoff) {
			continue
		}
		attempts++
		if err := s.terminateTusUpload(ctx, candidate.id); err != nil {
			slog.Warn("failed to expire incomplete tus upload", "upload_id", candidate.id, "error", err)
			continue
		}
		slog.Info("expired incomplete tus upload", "upload_id", candidate.id, "age_hours", time.Since(candidate.activity).Hours(), "bytes_reclaimed", candidate.size)
	}
}

func inspectTusCandidate(dir, infoName string) (*tusCandidate, error) {
	id := strings.TrimSuffix(infoName, ".info")
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return nil, fmt.Errorf("unsafe upload ID")
	}
	infoPath := filepath.Join(dir, id+".info")
	infoStat, err := os.Lstat(infoPath)
	if err != nil {
		return nil, err
	}
	if !infoStat.Mode().IsRegular() {
		return nil, fmt.Errorf("sidecar is not a regular file")
	}
	file, err := os.Open(infoPath)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxTusSidecarSize+1))
	file.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTusSidecarSize {
		return nil, fmt.Errorf("sidecar exceeds %d bytes", maxTusSidecarSize)
	}
	var info tusFileInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, err
	}
	dataPath := filepath.Join(dir, id)
	if info.ID != id || info.Size <= 0 || info.SizeIsDeferred || info.Storage.Type != "filestore" || filepath.Clean(info.Storage.Path) != filepath.Clean(dataPath) || filepath.Clean(info.Storage.InfoPath) != filepath.Clean(infoPath) {
		return nil, fmt.Errorf("sidecar identity/storage mismatch")
	}
	dataStat, err := os.Lstat(dataPath)
	if err != nil {
		return nil, err
	}
	if !dataStat.Mode().IsRegular() {
		return nil, fmt.Errorf("upload data is not a regular file")
	}
	if dataStat.Size() >= info.Size {
		return nil, nil // complete uploads belong to post-finish recovery
	}
	activity := infoStat.ModTime()
	if dataStat.ModTime().After(activity) {
		activity = dataStat.ModTime()
	}
	if lockStat, err := os.Lstat(filepath.Join(dir, id+".lock")); err == nil {
		if lockStat.ModTime().After(activity) {
			activity = lockStat.ModTime()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &tusCandidate{id: id, size: dataStat.Size(), activity: activity}, nil
}

func (s *Server) terminateTusUpload(ctx context.Context, id string) error {
	endpoint := strings.TrimRight(s.cfg.TusInternalURL, "/") + "/files/" + url.PathEscape(id)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set(internalProxySecretHeader, s.cfg.TusHookSecret)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil
	}
	return fmt.Errorf("tusd DELETE returned %s", resp.Status)
}
