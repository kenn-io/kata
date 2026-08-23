//go:build !windows

package connector

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

type processClientHelperObservation int

func observeProcessClientHelper(_ *testing.T, pid int) processClientHelperObservation {
	return processClientHelperObservation(pid)
}

func requireProcessClientHelperGone(t *testing.T, observed processClientHelperObservation) {
	t.Helper()
	pid := int(observed)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("connector descendant %d survived cancellation", pid)
}
