package daemon

import (
	"go.kenn.io/kata/internal/db/sqlitelock"
)

// AcquireDatabaseLock retains the daemon-facing release callback while the
// lock implementation is shared with every writable SQLite open.
func AcquireDatabaseLock(path string) (func(), error) {
	lock, err := sqlitelock.Acquire(path)
	if err != nil {
		return nil, err
	}
	return lock.Release, nil
}
