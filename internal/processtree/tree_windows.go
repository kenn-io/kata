//go:build windows

package processtree

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platformTree struct {
	job windows.Handle
}

func newPlatformTree(cmd *exec.Cmd) (platformTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return platformTree{}, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), // #nosec G103 -- Windows requires a pointer to this typed job limit structure.
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return platformTree{}, err
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	return platformTree{job: job}, nil
}

func (t *platformTree) start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return errors.Join(err, t.close())
	}
	var assignErr error
	if err := cmd.Process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(t.job, windows.Handle(handle))
	}); err != nil {
		return t.failStart(cmd, err)
	}
	if assignErr != nil {
		return t.failStart(cmd, assignErr)
	}
	if err := resumeProcess(cmd.Process.Pid); err != nil {
		return t.failStart(cmd, err)
	}
	return nil
}

func (t *platformTree) failStart(cmd *exec.Cmd, cause error) error {
	var cleanupErrs []error
	if t.job != 0 {
		if err := windows.TerminateJobObject(t.job, 1); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
	}
	if cmd.Process != nil {
		if err := kill(cmd); err != nil {
			cleanupErrs = append(cleanupErrs, err)
		}
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
	}
	cleanupErrs = append(cleanupErrs, t.close())
	return errors.Join(append([]error{cause}, cleanupErrs...)...)
}

func resumeProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot) //nolint:errcheck // The snapshot is read-only and already exhausted on return.

	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == uint32(pid) { // #nosec G115 -- Windows process IDs are unsigned 32-bit values.
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			_, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			return errors.Join(resumeErr, closeErr)
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return errors.New("suspended process thread not found")
			}
			return err
		}
	}
}

func (t *platformTree) close() error {
	if t.job == 0 {
		return nil
	}
	job := t.job
	t.job = 0
	return windows.CloseHandle(job)
}

func (*platformTree) terminate(_ *exec.Cmd) error { return nil }

func (t *platformTree) kill(cmd *exec.Cmd) error {
	err := windows.TerminateJobObject(t.job, 1)
	if err == nil {
		return nil
	}
	exited, statusErr := processExited(cmd)
	return killResult(err, exited, statusErr)
}

func (t *platformTree) alive(_ *exec.Cmd) bool {
	return t.job != 0
}

func prepare(_ *exec.Cmd) {}

func terminate(_ *exec.Cmd) error { return nil }

func kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return killResult(err, false, nil)
	}
	exited, statusErr := processExited(cmd)
	return killResult(err, exited, statusErr)
}

func killResult(err error, exited bool, statusErr error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) &&
		((statusErr == nil && exited) || errors.Is(statusErr, os.ErrProcessDone)) {
		return nil
	}
	return err
}

func processExited(cmd *exec.Cmd) (bool, error) {
	var waitResult uint32
	var statusErr error
	if err := cmd.Process.WithHandle(func(handle uintptr) {
		waitResult, statusErr = windows.WaitForSingleObject(windows.Handle(handle), 0)
	}); err != nil {
		return false, err
	}
	if statusErr != nil {
		return false, statusErr
	}
	return waitResult == windows.WAIT_OBJECT_0, nil
}

func alive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
}
