package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetConfig retrieves a single app_config value. ok is false if the key has
// never been set.
func (s *Store) GetConfig(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM app_config WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get config %s: %w", key, err)
	}
	return value, true, nil
}

// SetConfig upserts a single app_config value.
func (s *Store) SetConfig(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_config (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("set config %s: %w", key, err)
	}
	return nil
}

// DeleteConfig removes a key, restoring default behavior for it.
func (s *Store) DeleteConfig(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM app_config WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("delete config %s: %w", key, err)
	}
	return nil
}

const (
	// ConfigKeyUploadExpiresAt stores the RFC3339 timestamp after which new
	// uploads are refused. Viewing and downloading existing media is never
	// affected by this setting.
	ConfigKeyUploadExpiresAt = "upload_expires_at"

	// ConfigKeyBranding stores the admin-managed main-page text and color
	// theme as one validated JSON document. A single value keeps updates atomic
	// and allows the schema to grow without a database migration.
	ConfigKeyBranding = "branding"

	// ConfigKeyApprovalRequired is present with value "true" only while new
	// uploads must wait for admin approval before public visibility.
	ConfigKeyApprovalRequired = "approval_required"
)
