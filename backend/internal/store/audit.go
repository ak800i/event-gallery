package store

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"event-gallery/backend/internal/models"
)

// RecordAudit appends an entry to the audit log. mediaID and filename may be
// empty for actions not tied to a specific file (login, config changes).
func (s *Store) RecordAudit(ctx context.Context, action models.AuditAction, actor, mediaID, filename, details string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (action, actor, media_id, filename, details, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		string(action), actor, nullableString(mediaID), nullableString(filename), nullableString(details), formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ListAuditParams configures a page of the audit log.
type ListAuditParams struct {
	Cursor *Cursor
	Limit  int
}

// ListAudit returns audit entries newest first, paginated by created_at+id.
func (s *Store) ListAudit(ctx context.Context, p ListAuditParams) ([]models.AuditEntry, string, error) {
	query := `SELECT id, action, actor, COALESCE(media_id, ''), COALESCE(filename, ''), COALESCE(details, ''), created_at FROM audit_log WHERE 1=1`
	args := []any{}
	if p.Cursor != nil {
		cursorID, err := strconv.ParseInt(p.Cursor.ID, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid audit cursor id: %w", err)
		}
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, p.Cursor.SortKey, p.Cursor.SortKey, cursorID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, p.Limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()

	var entries []models.AuditEntry
	for rows.Next() {
		var e models.AuditEntry
		var createdAtStr string
		if err := rows.Scan(&e.ID, &e.Action, &e.Actor, &e.MediaID, &e.Filename, &e.Details, &createdAtStr); err != nil {
			return nil, "", fmt.Errorf("scan audit row: %w", err)
		}
		createdAt, err := parseTime(createdAtStr)
		if err != nil {
			return nil, "", err
		}
		e.CreatedAt = createdAt
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(entries) > p.Limit {
		entries = entries[:p.Limit]
		last := entries[len(entries)-1]
		nextCursor = EncodeCursor(&Cursor{SortKey: formatTime(last.CreatedAt), ID: fmt.Sprintf("%d", last.ID)})
	}
	return entries, nextCursor, nil
}
