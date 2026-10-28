package daemonruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"go.kenn.io/kata/internal/linkpath"
	kitdaemon "go.kenn.io/kit/daemon"
)

const storeOwnerLockRetry = 50 * time.Millisecond

// StoreOwnerLockPaths names the locks a daemon holds for its whole life so no
// second daemon opens the same store. A SQLite lock sits beside the database
// file: a container bind mount or sandbox can reach that file through another
// path, which hashes to a different runtime namespace but the same lock file.
//
// Every name on a symlink chain to the database gets a lock, including a
// target SQLite has not created yet. Another daemon can be configured with
// any of those names, and a link can later be replaced by a regular file (an
// older release's cutover did that), so every name must stay owned.
// Directory symlinks are resolved first so one file never gets two locks.
// Hard links to one file are not unified. PostgreSQL has no file, so its lock
// lives in the runtime namespace. The paths are sorted so every daemon takes
// them in one order.
func StoreOwnerLockPaths(dataDir, dsn string) []string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return []string{filepath.Join(dataDir, "store-owner.lock")}
	}
	var paths []string
	for _, name := range linkpath.Chain(strings.TrimPrefix(dsn, "sqlite://")) {
		paths = append(paths, name+".owner.lock")
	}
	slices.Sort(paths)
	return slices.Compact(paths)
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

// ForeignStoreOwnerGrace is how long startup waits for a held store lock to
// clear before it blames a live daemon in another PID namespace.
var ForeignStoreOwnerGrace = 3 * time.Second

// RefuseForeignStoreOwner reports an error when ownerLocks stay held through
// ForeignStoreOwnerGrace and dataDir has a live record from another PID
// namespace. That daemon keeps the store until it stops, unlike a restarting
// local daemon, so waiting for the lock would only delay the same refusal.
// The grace covers a local daemon still releasing the store. With no such
// record it returns nil at once and the caller's normal wait applies.
func RefuseForeignStoreOwner(ctx context.Context, dataDir string, ownerLocks []string) error {
	if _, ok := foreignRuntimeRecord(dataDir); !ok {
		return nil
	}
	deadline := time.NewTimer(ForeignStoreOwnerGrace)
	defer deadline.Stop()
	tick := time.NewTicker(storeOwnerLockRetry)
	defer tick.Stop()
	for StoreOwnerLocked(ownerLocks) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		case <-deadline.C:
			// That daemon can exit while a local starter takes the lock between
			// polls, so blame it only if its record is still there.
			owner, ok := foreignRuntimeRecord(dataDir)
			if !ok {
				return nil
			}
			return fmt.Errorf("a kata daemon in another PID namespace owns this store (pid %d there; runtime record %s); run kata where that daemon runs, or set KATA_SERVER to reach it", owner.PID, owner.SourcePath)
		}
	}
	return nil
}

// foreignRuntimeRecord returns a record in dataDir for a live process in
// another PID namespace.
func foreignRuntimeRecord(dataDir string) (kitdaemon.RuntimeRecord, bool) {
	records, err := (kitdaemon.RuntimeStore{Dir: dataDir}).List()
	if err != nil {
		return kitdaemon.RuntimeRecord{}, false
	}
	for _, record := range records {
		if kitdaemon.RuntimeProcessScope(record) == kitdaemon.ProcessScopeForeign {
			return record, true
		}
	}
	return kitdaemon.RuntimeRecord{}, false
}
