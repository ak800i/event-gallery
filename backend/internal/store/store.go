package store

import "database/sql"

// Store wraps the SQLite database handle and provides typed data-access
// methods used by the HTTP API layer. It intentionally has no knowledge of
// HTTP; all request-shaped concerns (pagination limits, filters) are passed
// in as plain parameters.
type Store struct {
	db *sql.DB
}

// New creates a Store backed by the given database handle.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB exposes the underlying handle for callers (e.g. health checks) that
// need it directly.
func (s *Store) DB() *sql.DB {
	return s.db
}
