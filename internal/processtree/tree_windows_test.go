//go:build windows

package processtree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestKillResultAccessDenied(t *testing.T) {
	accessDenied := fmt.Errorf("terminate process: %w", windows.ERROR_ACCESS_DENIED)
	other := errors.New("status unavailable")

	require.NoError(t, killResult(accessDenied, true, nil))
	require.NoError(t, killResult(os.ErrProcessDone, false, other))
	require.NoError(t, killResult(accessDenied, false, os.ErrProcessDone))
	require.ErrorIs(t, killResult(accessDenied, false, nil), windows.ERROR_ACCESS_DENIED)
	require.ErrorIs(t, killResult(accessDenied, true, other), windows.ERROR_ACCESS_DENIED)
	require.ErrorIs(t, killResult(other, true, nil), other)
}

func TestKillExitedProcessBeforeWait(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit", "0")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	var waitResult uint32
	var waitErr error
	require.NoError(t, cmd.Process.WithHandle(func(handle uintptr) {
		waitResult, waitErr = windows.WaitForSingleObject(windows.Handle(handle), windows.INFINITE)
	}))
	require.NoError(t, waitErr)
	require.Equal(t, uint32(windows.WAIT_OBJECT_0), waitResult)

	require.NoError(t, kill(cmd))
	require.NoError(t, cmd.Wait())
}
