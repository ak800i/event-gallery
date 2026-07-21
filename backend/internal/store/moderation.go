package store

import (
	"context"
	"fmt"
	"time"
)

func (s *Store) ApprovalRequired(ctx context.Context) (bool, error) {
	value, ok, err := s.GetConfig(ctx, ConfigKeyApprovalRequired)
	if err != nil {
		return false, err
	}
	if !ok || value == "false" || value == "" {
		return false, nil
	}
	if value == "true" {
		return true, nil
	}
	return false, fmt.Errorf("invalid approval configuration value %q", value)
}

// SetApprovalRequired changes moderation mode atomically with approving every
// pending item when disabling it. SQLite serializes this transaction with
// media inserts, so an upload cannot race the toggle and remain pending after
// approval is turned off.
func (s *Store) SetApprovalRequired(ctx context.Context, required bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin approval config update: %w", err)
	}
	defer tx.Rollback()

	if required {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO app_config (key, value) VALUES (?, 'true')
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, ConfigKeyApprovalRequired); err != nil {
			return 0, fmt.Errorf("enable approval: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM app_config WHERE key = ?`, ConfigKeyApprovalRequired); err != nil {
			return 0, fmt.Errorf("disable approval: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE media_items SET approved_at = ? WHERE approved_at IS NULL`, formatTime(time.Now()))
		if err != nil {
			return 0, fmt.Errorf("auto-approve pending media: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("read auto-approval result: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit approval config update: %w", err)
		}
		return changed, nil
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit approval config update: %w", err)
	}
	return 0, nil
}

func (s *Store) ApproveBulk(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin approval: %w", err)
	}
	defer tx.Rollback()

	approvedAt := formatTime(time.Now())
	var changed []string
	for _, id := range ids {
		result, err := tx.ExecContext(ctx, `
			UPDATE media_items SET approved_at = ?
			WHERE id = ? AND status = 'active' AND approved_at IS NULL`, approvedAt, id)
		if err != nil {
			return nil, fmt.Errorf("approve %s: %w", id, err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read approval result: %w", err)
		}
		if count > 0 {
			changed = append(changed, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit approval: %w", err)
	}
	return changed, nil
}
