package daemonruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const storeOwnerLockRetry = 50 * time.Millisecond

// StoreOwnerLockPath names the lock a daemon holds for its whole life so no
// second daemon opens the same store. A SQLite lock sits beside the database
// file: a container bind mount or sandbox can reach that file through another
// path, which hashes to a different runtime namespace but the same lock file.
// PostgreSQL has no file, so its lock lives in the runtime namespace.
func StoreOwnerLockPath(dataDir, dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return filepath.Join(dataDir, "store-owner.lock")
	}
	return strings.TrimPrefix(dsn, "sqlite://") + ".owner.lock"
}

// AcquireStoreOwnerLock waits until ctx ends to hold path exclusively. The
// kernel lock is visible across PID namespaces and ends when the process exits.
func AcquireStoreOwnerLock(ctx context.Context, path string) (func(), error) {
	lock := flock.New(path)
	locked, err := lock.TryLockContext(ctx, storeOwnerLockRetry)
	if err != nil || !locked {
		_ = lock.Close()
		if err == nil {
			err = ctx.Err()
		}
		return nil, err
	}
	return func() { _ = lock.Close() }, nil
}

// StoreOwnerLocked reports whether another process holds path. It never
// creates the lock file; a missing file has no owner.
func StoreOwnerLocked(path string) bool {
	lock := flock.New(path, flock.SetFlag(os.O_RDONLY))
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLock()
	return err == nil && !locked
}
