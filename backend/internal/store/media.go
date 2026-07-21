package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"wedding-gallery/backend/internal/models"
)

const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func formatTimePtr(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(*t), Valid: true}
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(timeLayout, s)
}

func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// InsertMedia stores a newly processed media item. sha256 has a UNIQUE
// constraint so duplicate uploads (a race between two concurrent uploads of
// the same file) are rejected at the database layer as well as by the
// application-level preflight check.
func (s *Store) InsertMedia(ctx context.Context, m *models.MediaItem) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO media_items (
			id, original_filename, stored_filename, kind, mime_type, size_bytes, sha256,
			width, height, duration_seconds, has_thumbnail, captured_at, uploaded_at, approved_at,
			uploader_name, uploader_ip, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			CASE WHEN COALESCE((SELECT value FROM app_config WHERE key = ?), 'false') = 'false' THEN ? ELSE NULL END,
			?, ?, ?)`,
		m.ID, m.OriginalFilename, m.StoredFilename, string(m.Kind), m.MimeType, m.SizeBytes, m.SHA256,
		nullableInt(m.Width), nullableInt(m.Height), nullableFloat(m.DurationSeconds), boolToInt(m.HasThumbnail),
		formatTimePtr(m.CapturedAt), formatTime(m.UploadedAt), ConfigKeyApprovalRequired, formatTime(m.UploadedAt),
		m.UploaderName, m.UploaderIP, string(models.StatusActive),
	)
	if err != nil {
		return fmt.Errorf("insert media: %w", err)
	}
	return nil
}

func nullableInt(v int) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(v), Valid: true}
}

func nullableFloat(v float64) sql.NullFloat64 {
	if v == 0 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: v, Valid: true}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type mediaRow struct {
	ID               string
	OriginalFilename string
	StoredFilename   string
	Kind             string
	MimeType         string
	SizeBytes        int64
	SHA256           string
	Width            sql.NullInt64
	Height           sql.NullInt64
	DurationSeconds  sql.NullFloat64
	HasThumbnail     int
	CapturedAt       sql.NullString
	UploadedAt       string
	ApprovedAt       sql.NullString
	UploaderName     string
	UploaderIP       sql.NullString
	Status           string
	DeletedAt        sql.NullString
	LikeCount        int
	Liked            int
}

func scanMediaRow(row *sql.Rows) (mediaRow, error) {
	var r mediaRow
	err := row.Scan(
		&r.ID, &r.OriginalFilename, &r.StoredFilename, &r.Kind, &r.MimeType, &r.SizeBytes, &r.SHA256,
		&r.Width, &r.Height, &r.DurationSeconds, &r.HasThumbnail, &r.CapturedAt, &r.UploadedAt, &r.ApprovedAt,
		&r.UploaderName, &r.UploaderIP, &r.Status, &r.DeletedAt, &r.LikeCount, &r.Liked,
	)
	return r, err
}

func (r mediaRow) toModel() (models.MediaItem, error) {
	captured, err := parseNullTime(r.CapturedAt)
	if err != nil {
		return models.MediaItem{}, err
	}
	uploaded, err := parseTime(r.UploadedAt)
	if err != nil {
		return models.MediaItem{}, err
	}
	approved, err := parseNullTime(r.ApprovedAt)
	if err != nil {
		return models.MediaItem{}, err
	}
	deleted, err := parseNullTime(r.DeletedAt)
	if err != nil {
		return models.MediaItem{}, err
	}
	return models.MediaItem{
		ID:               r.ID,
		OriginalFilename: r.OriginalFilename,
		StoredFilename:   r.StoredFilename,
		Kind:             models.MediaKind(r.Kind),
		MimeType:         r.MimeType,
		SizeBytes:        r.SizeBytes,
		SHA256:           r.SHA256,
		Width:            int(r.Width.Int64),
		Height:           int(r.Height.Int64),
		DurationSeconds:  r.DurationSeconds.Float64,
		HasThumbnail:     r.HasThumbnail != 0,
		CapturedAt:       captured,
		UploadedAt:       uploaded,
		ApprovedAt:       approved,
		UploaderName:     r.UploaderName,
		UploaderIP:       r.UploaderIP.String,
		Status:           models.MediaStatus(r.Status),
		DeletedAt:        deleted,
		LikeCount:        r.LikeCount,
		LikedByDevice:    r.Liked != 0,
	}, nil
}

const mediaSelectColumns = `
	m.id, m.original_filename, m.stored_filename, m.kind, m.mime_type, m.size_bytes, m.sha256,
	m.width, m.height, m.duration_seconds, m.has_thumbnail, m.captured_at, m.uploaded_at, m.approved_at,
	m.uploader_name, m.uploader_ip, m.status, m.deleted_at,
	COALESCE((SELECT COUNT(*) FROM likes l WHERE l.media_id = m.id), 0) AS like_count,
	COALESCE((SELECT 1 FROM likes l2 WHERE l2.media_id = m.id AND l2.device_id = ?), 0) AS liked
`

// GetByID fetches a single media item regardless of status.
func (s *Store) GetByID(ctx context.Context, id string, deviceID string) (*models.MediaItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaSelectColumns+` FROM media_items m WHERE m.id = ?`, deviceID, id)
	if err != nil {
		return nil, fmt.Errorf("query media by id: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	row, err := scanMediaRow(rows)
	if err != nil {
		return nil, fmt.Errorf("scan media: %w", err)
	}
	item, err := row.toModel()
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// GetVisibleByID fetches only publicly visible media. Keeping this filter in
// the query prevents pending/trashed IDs from reaching thumbnails, originals,
// downloads, or likes through direct URLs.
func (s *Store) GetVisibleByID(ctx context.Context, id string, deviceID string) (*models.MediaItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaSelectColumns+`
		FROM media_items m
		WHERE m.id = ? AND m.status = 'active' AND m.approved_at IS NOT NULL`, deviceID, id)
	if err != nil {
		return nil, fmt.Errorf("query visible media by id: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	row, err := scanMediaRow(rows)
	if err != nil {
		return nil, fmt.Errorf("scan visible media: %w", err)
	}
	item, err := row.toModel()
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// GetBySHA256 looks up an existing (non-trashed or trashed) media item with
// the given whole-file hash, used for duplicate-upload detection.
func (s *Store) GetBySHA256(ctx context.Context, sha256Hex string) (*models.MediaItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mediaSelectColumns+` FROM media_items m WHERE m.sha256 = ?`, "", sha256Hex)
	if err != nil {
		return nil, fmt.Errorf("query media by sha256: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	row, err := scanMediaRow(rows)
	if err != nil {
		return nil, fmt.Errorf("scan media: %w", err)
	}
	item, err := row.toModel()
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ListGalleryParams configures a page of the public gallery feed.
type ListGalleryParams struct {
	Sort     models.GallerySort
	Order    models.SortOrder
	Cursor   *Cursor
	Limit    int
	DeviceID string
}

// ListGallery returns one page of active media items sorted by upload or
// capture time, plus a cursor for the next page (empty if this is the last
// page).
func (s *Store) ListGallery(ctx context.Context, p ListGalleryParams) ([]models.MediaItem, string, error) {
	sortExpr := "m.uploaded_at"
	if p.Sort == models.SortCaptured {
		sortExpr = "COALESCE(m.captured_at, m.uploaded_at)"
	}
	desc := p.Order != models.OrderAsc

	cmp := "<"
	orderDir := "DESC"
	if !desc {
		cmp = ">"
		orderDir = "ASC"
	}

	query := strings.Builder{}
	query.WriteString(`SELECT ` + mediaSelectColumns + `, ` + sortExpr + ` AS sort_key FROM media_items m WHERE m.status = 'active' AND m.approved_at IS NOT NULL`)
	args := []any{p.DeviceID}

	if p.Cursor != nil {
		query.WriteString(fmt.Sprintf(` AND (%s %s ? OR (%s = ? AND m.id %s ?))`, sortExpr, cmp, sortExpr, cmp))
		args = append(args, p.Cursor.SortKey, p.Cursor.SortKey, p.Cursor.ID)
	}
	query.WriteString(fmt.Sprintf(` ORDER BY %s %s, m.id %s LIMIT ?`, sortExpr, orderDir, orderDir))
	args = append(args, p.Limit+1)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", fmt.Errorf("query gallery: %w", err)
	}
	defer rows.Close()

	var items []models.MediaItem
	var sortKeys []string
	for rows.Next() {
		var r mediaRow
		var sortKey string
		if err := rows.Scan(
			&r.ID, &r.OriginalFilename, &r.StoredFilename, &r.Kind, &r.MimeType, &r.SizeBytes, &r.SHA256,
			&r.Width, &r.Height, &r.DurationSeconds, &r.HasThumbnail, &r.CapturedAt, &r.UploadedAt, &r.ApprovedAt,
			&r.UploaderName, &r.UploaderIP, &r.Status, &r.DeletedAt, &r.LikeCount, &r.Liked, &sortKey,
		); err != nil {
			return nil, "", fmt.Errorf("scan gallery row: %w", err)
		}
		item, err := r.toModel()
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
		sortKeys = append(sortKeys, sortKey)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(items) > p.Limit {
		items = items[:p.Limit]
		sortKeys = sortKeys[:p.Limit]
		last := items[len(items)-1]
		nextCursor = EncodeCursor(&Cursor{SortKey: sortKeys[len(sortKeys)-1], ID: last.ID})
	}
	return items, nextCursor, nil
}

// AdminListParams configures a page of the admin item listing.
type AdminListParams struct {
	Status   models.MediaStatus // "" means all statuses
	Approved *bool              // nil means either; false is the pending queue
	Cursor   *Cursor
	Limit    int
}

// ListAdmin returns items for the admin dashboard ordered by upload time
// descending (newest first), optionally filtered by status (active/trashed).
func (s *Store) ListAdmin(ctx context.Context, p AdminListParams) ([]models.MediaItem, string, error) {
	query := strings.Builder{}
	query.WriteString(`SELECT ` + mediaSelectColumns + ` FROM media_items m WHERE 1=1`)
	args := []any{""}

	if p.Status != "" {
		query.WriteString(` AND m.status = ?`)
		args = append(args, string(p.Status))
	}
	if p.Approved != nil {
		if *p.Approved {
			query.WriteString(` AND m.approved_at IS NOT NULL`)
		} else {
			query.WriteString(` AND m.approved_at IS NULL`)
		}
	}
	if p.Cursor != nil {
		query.WriteString(` AND (m.uploaded_at < ? OR (m.uploaded_at = ? AND m.id < ?))`)
		args = append(args, p.Cursor.SortKey, p.Cursor.SortKey, p.Cursor.ID)
	}
	query.WriteString(` ORDER BY m.uploaded_at DESC, m.id DESC LIMIT ?`)
	args = append(args, p.Limit+1)

	rows, err := s.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, "", fmt.Errorf("query admin list: %w", err)
	}
	defer rows.Close()

	var items []models.MediaItem
	for rows.Next() {
		row, err := scanMediaRow(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan admin row: %w", err)
		}
		item, err := row.toModel()
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(items) > p.Limit {
		items = items[:p.Limit]
		last := items[len(items)-1]
		nextCursor = EncodeCursor(&Cursor{SortKey: formatTime(last.UploadedAt), ID: last.ID})
	}
	return items, nextCursor, nil
}

// SetStatusBulk moves the given media IDs to the target status, returning
// the IDs that were actually changed (already-matching IDs are skipped so
// callers can build accurate audit entries).
func (s *Store) SetStatusBulk(ctx context.Context, ids []string, target models.MediaStatus) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var changed []string
	var deletedAt any
	if target == models.StatusTrashed {
		deletedAt = formatTime(time.Now())
	} else {
		deletedAt = nil
	}

	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE media_items SET status = ?, deleted_at = ? WHERE id = ? AND status != ?`,
			string(target), deletedAt, id, string(target))
		if err != nil {
			return nil, fmt.Errorf("update status for %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			changed = append(changed, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return changed, nil
}
