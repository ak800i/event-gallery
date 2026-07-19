package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrSessionNotFound is returned when a session ID does not correspond to a
// valid, unexpired session.
var ErrSessionNotFound = errors.New("session not found or expired")

// AdminSession represents a server-side authenticated admin session.
type AdminSession struct {
	ID        string
	CSRFToken string
	ExpiresAt time.Time
}

func randomToken(numBytes int) (string, error) {
	b := make([]byte, numBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// CreateSession creates a new admin session valid for ttl, returning the
// session cookie value and the CSRF token that must accompany
// state-changing requests.
func (s *Store) CreateSession(ctx context.Context, ttl time.Duration) (*AdminSession, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	expires := now.Add(ttl)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO admin_sessions (id, csrf_token, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, csrf, formatTime(now), formatTime(expires),
	)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &AdminSession{ID: id, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// GetSession looks up a session by ID, returning ErrSessionNotFound if it
// does not exist or has expired.
func (s *Store) GetSession(ctx context.Context, id string) (*AdminSession, error) {
	var csrf, expiresStr string
	err := s.db.QueryRowContext(ctx, `SELECT csrf_token, expires_at FROM admin_sessions WHERE id = ?`, id).Scan(&csrf, &expiresStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	expires, err := parseTime(expiresStr)
	if err != nil {
		return nil, err
	}
	if time.Now().After(expires) {
		return nil, ErrSessionNotFound
	}
	return &AdminSession{ID: id, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// DeleteSession removes a session (used on logout).
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions purges expired sessions; intended to be called
// periodically by a background sweeper.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at < ?`, formatTime(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return res.RowsAffected()
}
