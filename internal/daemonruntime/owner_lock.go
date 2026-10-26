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

// StoreOwnerLockPaths names the locks a daemon holds for its whole life so no
// second daemon opens the same store. A SQLite lock sits beside the database
// file: a container bind mount or sandbox can reach that file through another
// path, which hashes to a different runtime namespace but the same lock file.
//
// When the configured file is a symlink, the target gets a lock too. Cutover
// renames the upgraded database onto the configured path, turning the link
// into a regular file, so both names must stay owned. Directory symlinks are
// resolved first so one file never gets two locks. Hard links to one file are
// not unified. PostgreSQL has no file, so its lock lives in the runtime
// namespace. The paths are sorted so every daemon takes them in one order.
func StoreOwnerLockPaths(dataDir, dsn string) []string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return []string{filepath.Join(dataDir, "store-owner.lock")}
	}
	path := strings.TrimPrefix(dsn, "sqlite://")
	if dir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(dir, filepath.Base(path))
	}
	paths := []string{path + ".owner.lock"}
	if target, err := filepath.EvalSymlinks(path); err == nil && target != path {
		paths = append(paths, target+".owner.lock")
	}
	slices.Sort(paths)
	return paths
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
