package daemonruntime

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const storeOwnerLockRetry = 50 * time.Millisecond

// maxStoreSymlinkHops bounds a symlink chain, as the OS does when it opens one.
const maxStoreSymlinkHops = 40

// StoreOwnerLockPaths names the locks a daemon holds for its whole life so no
// second daemon opens the same store. A SQLite lock sits beside the database
// file: a container bind mount or sandbox can reach that file through another
// path, which hashes to a different runtime namespace but the same lock file.
//
// Every name on a symlink chain to the database gets a lock, including a
// target SQLite has not created yet. Cutover renames the upgraded database
// onto the configured path, which turns that link into a regular file, so
// every name another daemon might be configured with must stay owned.
// Directory symlinks are resolved first so one file never gets two locks.
// Hard links to one file are not unified. PostgreSQL has no file, so its lock
// lives in the runtime namespace. The paths are sorted so every daemon takes
// them in one order.
func StoreOwnerLockPaths(dataDir, dsn string) []string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return []string{filepath.Join(dataDir, "store-owner.lock")}
	}
	var paths []string
	for _, name := range sqliteFileNames(strings.TrimPrefix(dsn, "sqlite://")) {
		paths = append(paths, name+".owner.lock")
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// sqliteFileNames lists path and each symlink hop from it, whether or not the
// last name exists yet.
func sqliteFileNames(path string) []string {
	var names []string
	for range maxStoreSymlinkHops {
		if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			path = filepath.Join(dir, filepath.Base(path))
		}
		names = append(names, path)
		target, err := os.Readlink(path)
		if err != nil {
			break
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = target
	}
	return names
}

// AcquireStoreOwnerLock waits until ctx ends to hold every path exclusively.
// The kernel locks are visible across PID namespaces and end when the process
// exits.
func AcquireStoreOwnerLock(ctx context.Context, paths []string) (func(), error) {
	locks := make([]*flock.Flock, 0, len(paths))
	release := func() {
		for _, lock := range locks {
			_ = lock.Close()
		}
	}
	for _, path := range paths {
		lock := flock.New(path)
		locked, err := lock.TryLockContext(ctx, storeOwnerLockRetry)
		if err != nil || !locked {
			_ = lock.Close()
			release()
			if err == nil {
				err = ctx.Err()
			}
			return nil, err
		}
		locks = append(locks, lock)
	}
	return release, nil
}

// StoreOwnerLocked reports whether another process holds any of paths. It
// never creates a lock file; a missing file has no owner.
func StoreOwnerLocked(paths []string) bool {
	for _, path := range paths {
		lock := flock.New(path, flock.SetFlag(os.O_RDONLY))
		locked, err := lock.TryLock()
		_ = lock.Close()
		if err == nil && !locked {
			return true
		}
	}
	return false
}
