package store

import (
	"context"
	"database/sql"
	"fmt"
)

// VideoMetadataRecord contains only the fields needed by one-time metadata
// repair tasks; it intentionally does not expose a general unpaginated media
// listing to HTTP handlers.
type VideoMetadataRecord struct {
	ID             string
	StoredFilename string
	Width          int
	Height         int
}

func (s *Store) ListVideoMetadata(ctx context.Context) ([]VideoMetadataRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, stored_filename, width, height
		FROM media_items
		WHERE kind = 'video'
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list video metadata: %w", err)
	}
	defer rows.Close()

	var records []VideoMetadataRecord
	for rows.Next() {
		var record VideoMetadataRecord
		var width, height sql.NullInt64
		if err := rows.Scan(&record.ID, &record.StoredFilename, &width, &height); err != nil {
			return nil, fmt.Errorf("scan video metadata: %w", err)
		}
		if width.Valid {
			record.Width = int(width.Int64)
		}
		if height.Valid {
			record.Height = int(height.Int64)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate video metadata: %w", err)
	}
	return records, nil
}

func (s *Store) UpdateVideoDimensions(ctx context.Context, id string, width, height int) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("video dimensions must be positive")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE media_items SET width = ?, height = ?
		WHERE id = ? AND kind = 'video'`, width, height, id)
	if err != nil {
		return fmt.Errorf("update video dimensions for %s: %w", id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read video dimension update result: %w", err)
	}
	if changed == 0 {
		return fmt.Errorf("video %s not found", id)
	}
	return nil
}
