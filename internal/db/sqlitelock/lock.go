// Package sqlitelock coordinates process-lifetime locks for SQLite databases.
package sqlitelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/pathresolve"
)

// Lock is the process-lifetime lock for one canonical SQLite database. It
// must be acquired before opening or replacing the database.
type Lock struct {
	file     *os.File
	database string
	release  sync.Once
}

// Acquire takes a non-blocking lock beside the canonical SQLite database. Its
// identity never depends on the home, socket, or build version. Keep the lock
// file after release: unlinking it would let another process lock a different
// inode while the old one is still held.
func Acquire(path string) (*Lock, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return nil, err
	}
	database, err := canonicalDatabasePath(absolute)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(database+".daemon.lock", os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // G304: the lock path is derived from the canonical configured SQLite database.
	if err != nil {
		return nil, fmt.Errorf("open database lock: %w", err)
	}
	if err := tryDatabaseLock(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("daemon already running or database lock unavailable for %s; stop the other daemon before starting: %w", database, err)
	}
	return &Lock{file: file, database: database}, nil
}

// Matches reports whether this live lock protects path's canonical SQLite
// database. It lets daemon startup pass its existing lock into storeopen.
func (l *Lock) Matches(path string) error {
	if l == nil || l.file == nil {
		return errors.New("SQLite database lock is missing")
	}
	if _, err := l.file.Stat(); err != nil {
		return fmt.Errorf("inspect SQLite database lock: %w", err)
	}
	database, err := canonicalDatabasePath(path)
	if err != nil {
		return err
	}
	if database != l.database {
		return fmt.Errorf("SQLite database lock for %s does not match database %s", l.database, database)
	}
	return nil
}

// Release drops this process's ownership while leaving the lock file in place.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	l.release.Do(func() {
		if l.file != nil {
			_ = l.file.Close()
		}
	})
}

func canonicalDatabasePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := pathresolve.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", fmt.Errorf("resolve database lock directory: %w", err)
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	canonical, err := pathresolve.EvalSymlinks(absolute)
	if errors.Is(err, os.ErrNotExist) {
		// A fresh database takes ownership before SQLite creates the file.
		// A dangling symlink is an invalid target, not a fresh path.
		if _, linkErr := os.Lstat(absolute); linkErr == nil {
			return "", fmt.Errorf("resolve database lock path: %w", err)
		}
		return absolute, nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve database lock path: %w", err)
	}
	return canonical, nil
}
