package sqlitestore

import (
	"errors"

	sqlite3 "modernc.org/sqlite/lib"
)

const sqlitePrimaryCodeMask = 0xff

type sqliteCodeError interface {
	Code() int
}

// IsTransient reports whether err is a SQLite busy/locked condition that may
// clear if the whole mutation is retried after a short delay. It is the
// backend-specific predicate fed to db.RetryTransient by the Store's
// RetryTransient method.
func IsTransient(err error) bool {
	var coded sqliteCodeError
	if !errors.As(err, &coded) {
		return false
	}
	switch coded.Code() & sqlitePrimaryCodeMask {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return true
	default:
		return false
	}
}
