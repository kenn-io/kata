package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
)

const daemonChildExitGrace = 5 * time.Second

// WaitForDaemonStartup waits for readiness, extending the ordinary startup
// budget while SQLite still needs a cutover. If the child exits first, it
// checks for a compatible daemon started concurrently: briefly, or for the
// remaining budget while another daemon holds any of ownerLocks (see
// daemonruntime.StoreOwnerLockPaths; none skips that check). The detached
// child owns its lifetime: neither a readiness timeout nor cancellation kills it.
func WaitForDaemonStartup(
	ctx context.Context, ownerLocks []string, wait time.Duration, exited <-chan error,
	ready func(context.Context) (bool, error),
) error {
	return waitForDaemonStartup(ctx, ownerLocks, wait, exited, ready, true)
}

func waitForDaemonStartup(
	ctx context.Context, ownerLocks []string, wait time.Duration, exited <-chan error,
	ready func(context.Context) (bool, error), migrationAllowed bool,
) error {
	return waitForDaemonStartupWithProbe(ctx, wait, exited, ready, migrationAllowed, sqliteCutoverLocked,
		func(context.Context) bool { return daemonruntime.StoreOwnerLocked(ownerLocks) })
}

func waitForDaemonStartupWithProbe(
	ctx context.Context, wait time.Duration, exited <-chan error,
	ready func(context.Context) (bool, error), migrationAllowed bool,
	cutoverLocked, storeOwned func(context.Context) bool,
) error {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var exitedWhileMigrating, exitedBeforeReadiness error
	var sawCutoverLock, waitingAfterCutover, exitedWhileStoreOwned bool
	// observeCutover reports whether the lock is held and whether this probe
	// saw its release, which grants a fresh readiness budget.
	observeCutover := func() (locked, released bool) {
		locked = migrationAllowed && cutoverLocked(ctx)
		sawCutoverLock = sawCutoverLock || locked
		if locked && waitingAfterCutover {
			// A new cutover phase needs its own readiness budget after unlock.
			waitingAfterCutover = false
		}
		if sawCutoverLock && !locked && !waitingAfterCutover {
			// The daemon still has to open storage and publish readiness after
			// AutoCutover returns. Give that handoff an ordinary startup budget.
			deadline.Reset(wait)
			waitingAfterCutover = true
			return false, true
		}
		return locked, false
	}
	for {
		observeCutover()
		ok, err := ready(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("process exited without becoming ready")
			}
			// Another concurrent starter may own the cutover lock. Its daemon
			// can become ready even though our redundant child has exited.
			if ok, probeErr := ready(ctx); ok || probeErr != nil {
				return probeErr
			}
			if migrationAllowed && cutoverLocked(ctx) {
				sawCutoverLock = true
			}
			if sawCutoverLock {
				exitedWhileMigrating = err
				exited = nil
				continue
			}
			// Another starter can win a listener bind before publishing its
			// runtime record. Keep checking for a compatible daemon briefly,
			// even when no SQLite cutover is involved.
			exitedBeforeReadiness = err
			exited = nil
			// That starter may still be waiting for a replaced daemon to
			// release the store, so keep the remaining budget while it is owned.
			exitedWhileStoreOwned = storeOwned(ctx)
			if !exitedWhileStoreOwned {
				deadline.Reset(daemonChildExitGrace)
			}
			continue
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting for daemon readiness; detached daemon was left running; inspect kata daemon logs: %w", ctx.Err())
		case <-deadline.C:
			// The lock can be released after this iteration's probe.
			cutoverActive, released := observeCutover()
			if released {
				continue
			}
			if exitedWhileMigrating != nil && waitingAfterCutover && !cutoverActive {
				return fmt.Errorf("daemon exited before readiness and the concurrent migration has stopped: %w; inspect kata daemon logs", exitedWhileMigrating)
			}
			if exitedBeforeReadiness != nil {
				if cutoverActive {
					// The old schema alone is not evidence of active migration once
					// this process has exited; only wait for a live cutover owner.
					exitedWhileMigrating = exitedBeforeReadiness
					exitedBeforeReadiness = nil
					sawCutoverLock = true
					deadline.Reset(wait)
					continue
				}
				if sawCutoverLock && waitingAfterCutover {
					return fmt.Errorf("daemon exited before readiness and the concurrent migration has stopped: %w; inspect kata daemon logs", exitedBeforeReadiness)
				}
				if exitedWhileStoreOwned {
					return fmt.Errorf("daemon exited before readiness while another daemon owned the store, and no compatible daemon became ready within %s: %w; inspect kata daemon logs", wait, exitedBeforeReadiness)
				}
				return fmt.Errorf("daemon exited before readiness and no compatible daemon became ready within %s: %w; inspect kata daemon logs", daemonChildExitGrace, exitedBeforeReadiness)
			}
			if migrationAllowed && sqliteMigrationPending(ctx) {
				fmt.Fprintln(os.Stderr, "kata: SQLite migration in progress; waiting for daemon readiness (cancelling stops waiting and leaves the daemon running)")
				deadline.Reset(wait)
				continue
			}
			return fmt.Errorf("%w: daemon startup is still in progress after %s; detached daemon was left running; inspect kata daemon status and kata daemon logs", ErrLocalDaemonUnreachable, wait)
		case <-tick.C:
		}
	}
}

func sqliteMigrationPending(ctx context.Context) bool {
	dsn, err := config.KataDSN(ctx)
	if err != nil || strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return false
	}
	inspector, err := installedStorageInspector()
	if err != nil {
		return false
	}
	return sqliteMigrationPendingForPath(ctx, strings.TrimPrefix(dsn, "sqlite://"), inspector)
}

func sqliteMigrationPendingForPath(ctx context.Context, path string, inspector StorageInspector) bool {
	// Hold the lock through the read and its connection close. A lock probe
	// followed by a separate peek lets a cutover start between those operations.
	lock := flock.New(path + ".import.lock")
	locked, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return false
	}
	defer func() { _ = lock.Close() }()
	if !locked {
		return true
	}
	// A read-only peek never initializes or upgrades storage. Check the source
	// schema rather than temp-file growth: preflight and export can themselves
	// take longer than the readiness budget, before any temp file exists.
	version, _, err := inspector.PeekCurrentSchema(ctx)
	return err == nil && version > 0 && version < inspector.CurrentSchemaVersion()
}

func sqliteCutoverLocked(ctx context.Context) bool {
	dsn, err := config.KataDSN(ctx)
	if err != nil || strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return false
	}
	lock := flock.New(strings.TrimPrefix(dsn, "sqlite://")+".import.lock", flock.SetFlag(os.O_RDONLY))
	locked, err := lock.TryLock()
	if err != nil {
		return false
	}
	defer func() { _ = lock.Close() }()
	return !locked
}
