package store

import (
	"context"
	"fmt"
	"time"
)

// AddLike records a like from the given device for the given media item. It
// is idempotent: liking twice from the same device has no additional
// effect. Returns true if a new like row was created.
func (s *Store) AddLike(ctx context.Context, mediaID, deviceID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO likes (media_id, device_id, created_at) VALUES (?, ?, ?)`,
		mediaID, deviceID, formatTime(time.Now()),
	)
	if err != nil {
		return false, fmt.Errorf("add like: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RemoveLike deletes a device's like of a media item, if present. Returns
// true if a like was actually removed.
func (s *Store) RemoveLike(ctx context.Context, mediaID, deviceID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM likes WHERE media_id = ? AND device_id = ?`, mediaID, deviceID)
	if err != nil {
		return false, fmt.Errorf("remove like: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
