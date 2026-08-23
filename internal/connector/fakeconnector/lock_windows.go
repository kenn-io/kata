//go:build windows

package fakeconnector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockContext(ctx context.Context, path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304,G703 -- path is derived from the explicit test-owned state path.
	if err != nil {
		return nil, fmt.Errorf("open fixture state lock: %w", err)
	}
	handle := windows.Handle(file.Fd())
	var overlapped windows.Overlapped
	for {
		err = windows.LockFileEx(
			handle,
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&overlapped,
		)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire fixture state lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("acquire fixture state lock: %w", ctx.Err())
		case <-time.After(stateLockPoll):
		}
	}
}
