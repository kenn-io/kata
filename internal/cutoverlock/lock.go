// Package cutoverlock coordinates SQLite inspection and cutover.
package cutoverlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"go.kenn.io/kata/internal/linkpath"
)

// ErrInProgress identifies an exclusive cutover lock held by another process.
var ErrInProgress = errors.New("jsonl cutover in progress")

func resolvedPath(path string) string {
	chain := linkpath.Chain(path)
	return chain[len(chain)-1]
}

// Paths returns the marker paths in acquisition order.
func Paths(path string) []string {
	locks := []string{linkpath.Chain(path + ".import.lock")[0]}
	if target := resolvedPath(path) + ".import.lock"; target != locks[0] {
		locks = append(locks, target)
	}
	return locks
}

// WithReadLock allows concurrent inspections while excluding cutover.
func WithReadLock(ctx context.Context, path string, fn func() error) error {
	release, busy, err := readLock(path, true, true, func(lock *flock.Flock) (bool, error) {
		lockCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		locked, err := lock.TryRLockContext(lockCtx, 10*time.Millisecond)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = nil
		}
		return locked, err
	})
	if err != nil {
		return err
	}
	if busy != "" {
		return fmt.Errorf("%w: another process holds %s; wait for migration and inspect kata daemon logs", ErrInProgress, busy)
	}
	defer release()
	return fn()
}

// TryReadLock protects an inspection from a concurrent SQLite cutover.
// The caller must release the locks after closing the inspected database.
func TryReadLock(path string, create bool) (release func(), busy bool, err error) {
	release, busyPath, err := readLock(path, create, false, (*flock.Flock).TryRLock)
	return release, busyPath != "", err
}

// readLock uses the waiting opener's strict creation policy when wait is true.
func readLock(path string, create, wait bool, try func(*flock.Flock) (bool, error)) (release func(), busy string, err error) {
	var locks []*flock.Flock
	release = func() {
		for _, lock := range slices.Backward(locks) {
			_ = lock.Close()
		}
	}
	for _, name := range Paths(path) {
		flag := os.O_RDONLY
		if create {
			flag |= os.O_CREATE
		}
		var lock *flock.Flock
		if wait {
			lock = flock.New(name)
		} else {
			lock = flock.New(name, flock.SetFlag(flag))
		}
		locked, lockErr := try(lock)
		if lockErr != nil {
			_ = lock.Close()
			if !wait {
				if !create && errors.Is(lockErr, os.ErrNotExist) {
					continue
				}
				if create && (errors.Is(lockErr, os.ErrPermission) || errors.Is(lockErr, syscall.EROFS)) {
					if _, statErr := os.Lstat(name); errors.Is(statErr, os.ErrNotExist) {
						continue
					}
				}
			}
			release()
			return nil, "", fmt.Errorf("lock SQLite inspection: %w", lockErr)
		}
		if !locked {
			_ = lock.Close()
			release()
			return nil, name, nil
		}
		locks = append(locks, lock)
	}
	return release, "", nil
}
