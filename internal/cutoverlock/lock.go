// Package cutoverlock coordinates SQLite inspection and cutover.
package cutoverlock

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	var locks []*flock.Flock
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Close()
		}
	}()
	for _, name := range Paths(path) {
		lock := flock.New(name)
		locks = append(locks, lock)
		lockCtx, cancel := context.WithTimeout(ctx, time.Second)
		locked, err := lock.TryRLockContext(lockCtx, 10*time.Millisecond)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			err = nil
		}
		if err != nil {
			return fmt.Errorf("lock SQLite inspection: %w", err)
		}
		if !locked {
			return fmt.Errorf("%w: another process holds %s; wait for migration and inspect kata daemon logs", ErrInProgress, name)
		}
	}
	return fn()
}

// TryReadLock protects an inspection from a concurrent SQLite cutover.
// The caller must release the locks after closing the inspected database.
func TryReadLock(path string, create bool) (release func(), busy bool, err error) {
	var locks []*flock.Flock
	release = func() {
		for i := len(locks) - 1; i >= 0; i-- {
			_ = locks[i].Close()
		}
	}
	for _, name := range Paths(path) {
		flag := os.O_RDONLY
		if create {
			flag |= os.O_CREATE
		}
		lock := flock.New(name, flock.SetFlag(flag))
		locked, lockErr := lock.TryRLock()
		if lockErr != nil {
			_ = lock.Close()
			if !create && errors.Is(lockErr, os.ErrNotExist) {
				continue
			}
			if create && (errors.Is(lockErr, os.ErrPermission) || errors.Is(lockErr, syscall.EROFS)) {
				if _, statErr := os.Lstat(name); errors.Is(statErr, os.ErrNotExist) {
					continue
				}
			}
			release()
			return nil, false, fmt.Errorf("lock SQLite inspection: %w", lockErr)
		}
		if !locked {
			_ = lock.Close()
			release()
			return nil, true, nil
		}
		locks = append(locks, lock)
	}
	return release, false, nil
}
