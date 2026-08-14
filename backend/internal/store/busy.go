package store

import (
	"errors"

	sqlite "modernc.org/sqlite"
)

// sqliteBusy is SQLITE_BUSY, the primary result code SQLite returns when a
// writer could not take the lock within busy_timeout. Extended codes such as
// SQLITE_BUSY_SNAPSHOT (261) keep the same low byte, so the primary code is
// what we compare against.
const sqliteBusy = 5

// IsBusy reports whether err is SQLite declining to wait any longer for the
// write lock.
//
// This is a transient, retryable condition, not a failure: every caller in
// this codebase retries and normally succeeds on the next attempt. It is
// exported so those callers can log it at WARN and keep ERROR meaning
// "something needs a human".
//
// Measured during the wedding load campaign: a bulk purge holding a long write
// transaction made an otherwise completely idle app emit 141 ERROR lines in
// about six minutes, every one a claim that succeeded on the following tick.
// Alerting on ERROR would have paged someone for routine tidying up.
func IsBusy(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code()&0xff == sqliteBusy
	}
	return false
}
