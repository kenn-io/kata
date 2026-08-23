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
	var handleWaitErr error
	require.NoError(t, cmd.Process.WithHandle(func(handle uintptr) {
		waitResult, handleWaitErr = windows.WaitForSingleObject(windows.Handle(handle), windows.INFINITE)
	}))
	require.NoError(t, handleWaitErr)
	require.Equal(t, uint32(windows.WAIT_OBJECT_0), waitResult)

	require.NoError(t, kill(cmd))
	require.NoError(t, cmd.Wait())
}

func TestKillProcessExitedWithStillActiveCode(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit", "259")
	require.NoError(t, cmd.Start())
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	var waitResult uint32
	var handleWaitErr error
	require.NoError(t, cmd.Process.WithHandle(func(handle uintptr) {
		waitResult, handleWaitErr = windows.WaitForSingleObject(windows.Handle(handle), windows.INFINITE)
	}))
	require.NoError(t, handleWaitErr)
	require.Equal(t, uint32(windows.WAIT_OBJECT_0), waitResult)

	require.NoError(t, kill(cmd))
	waitErr := cmd.Wait()
	waited = true
	var exitErr *exec.ExitError
	require.ErrorAs(t, waitErr, &exitErr)
	require.Equal(t, 259, exitErr.ExitCode())
}

func TestTreeAttachFailureKillsAndReapsSuspendedProcess(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit", "0")
	tree, err := New(cmd)
	require.NoError(t, err)
	require.NoError(t, tree.Close())

	require.Error(t, tree.Start())
	require.NotNil(t, cmd.ProcessState)
	require.True(t, cmd.ProcessState.Exited())
	require.NoError(t, tree.Close())
}

func TestTreeResumeFailureKillsAndReapsAssignedProcess(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit", "0")
	tree, err := New(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })
	require.NoError(t, cmd.Start())
	var assignErr error
	require.NoError(t, cmd.Process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(tree.platform.job, windows.Handle(handle))
	}))
	require.NoError(t, assignErr)
	resumeErr := errors.New("synthetic resume failure")

	err = tree.platform.failStart(cmd, resumeErr)

	require.ErrorIs(t, err, resumeErr)
	require.NotNil(t, cmd.ProcessState)
	require.True(t, cmd.ProcessState.Exited())
}
