package store

import (
	"context"
	"fmt"
	"time"

	"event-gallery/backend/internal/models"
)

func (s *Store) ListTrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]models.MediaItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaSelectColumns+`
		FROM media_items m
		WHERE m.status = 'trashed' AND m.deleted_at IS NOT NULL
		  AND julianday(m.deleted_at) < julianday(?)
		ORDER BY julianday(m.deleted_at), m.id
		LIMIT ?`, "", formatTime(cutoff), limit)
	if err != nil {
		return nil, fmt.Errorf("list expired trash: %w", err)
	}
	defer rows.Close()
	var items []models.MediaItem
	for rows.Next() {
		row, err := scanMediaRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan expired trash: %w", err)
		}
		item, err := row.toModel()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired trash: %w", err)
	}
	return items, nil
}

// PurgeTrashed deletes matching trashed rows and records their permanent-delete
// audit entries in the same SQLite transaction. Filesystem staging happens
// before this method; this commit is the purge point used by crash recovery.
func (s *Store) PurgeTrashed(ctx context.Context, items []models.MediaItem, actor string) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin permanent delete: %w", err)
	}
	defer tx.Rollback()
	var changed []string
	createdAt := formatTime(time.Now())
	for _, item := range items {
		result, err := tx.ExecContext(ctx, `DELETE FROM media_items
			WHERE id = ? AND stored_filename = ? AND status = 'trashed'`, item.ID, item.StoredFilename)
		if err != nil {
			return nil, fmt.Errorf("permanently delete %s: %w", item.ID, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read permanent delete result: %w", err)
		}
		if count == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log
			(action, actor, media_id, filename, details, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, string(models.ActionPurge), actor, item.ID, item.OriginalFilename, "permanently deleted", createdAt); err != nil {
			return nil, fmt.Errorf("audit permanent delete %s: %w", item.ID, err)
		}
		changed = append(changed, item.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit permanent delete: %w", err)
	}
	return changed, nil
}
