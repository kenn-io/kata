//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCloseTranscriptPushCleansDescendantHoldingOutput(t *testing.T) {
	for _, mode := range []string{"descendant-hang", "descendant-exit"} {
		t.Run(mode, func(t *testing.T) {
			transcriptPushFixture(t, mode)
			dir := os.Getenv("AGENTSVIEW_DATA_DIR")
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			pid, waited := 0, false
			// Cleanup remains independent of assertions and of production cleanup.
			t.Cleanup(func() {
				cancel()
				if data, err := os.ReadFile(filepath.Join(dir, "parent.pid")); err == nil { // #nosec G304,G703 -- dir was set to a synthetic t.TempDir by transcriptPushFixture.
					if parentPID, err := strconv.Atoi(string(data)); err == nil && parentPID > 0 {
						_ = syscall.Kill(-parentPID, syscall.SIGKILL)
					}
				}
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				if !waited {
					select {
					case <-result:
					case <-time.After(3 * time.Second):
						t.Error("upload fixture did not retire after cleanup")
					}
				}
			})
			target := transcriptPushFixtureTarget()
			ref := nativePushTranscript(t, "codex")
			go func() { result <- runCloseTranscriptPush(ctx, target, ref) }()
			ready := filepath.Join(dir, "descendant.pid")
			require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: the external child publishes its actual PID through a readiness file.
				data, err := os.ReadFile(ready) // #nosec G304,G703 -- readiness file is inside the synthetic fixture directory.
				if err != nil {
					return false
				}
				pid, err = strconv.Atoi(string(data))
				return err == nil && pid > 0
			}, 3*time.Second, 10*time.Millisecond)
			if mode == "descendant-hang" {
				cancel()
			}
			select {
			case err := <-result:
				waited = true
				if mode == "descendant-hang" {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upload was held open by a descendant's output handles")
			}
			require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: observing native process termination requires the real kernel, outside synctest.
				return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
			}, 2*time.Second, 10*time.Millisecond, "upload descendant survived cleanup")
		})
	}
}
