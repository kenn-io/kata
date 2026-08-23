// Package processtree manages platform-appropriate subprocess cleanup.
package processtree

import (
	"errors"
	"os/exec"
	"time"
)

// Tree owns platform process-tree containment for one command.
type Tree struct {
	cmd      *exec.Cmd
	platform platformTree
}

// New prepares cmd for a race-free contained start where the platform
// supports it. Callers must call Close whenever New succeeds, including when
// Start returns an error.
func New(cmd *exec.Cmd) (*Tree, error) {
	platform, err := newPlatformTree(cmd)
	if err != nil {
		return nil, err
	}
	return &Tree{cmd: cmd, platform: platform}, nil
}

// Start starts the prepared command and establishes its containment before it
// can spawn descendants.
func (t *Tree) Start() error {
	return t.platform.start(t.cmd)
}

// Wait waits for the direct process and its configured I/O copying to finish.
func (t *Tree) Wait() error {
	return t.cmd.Wait()
}

// Close releases the platform containment resources. On platforms whose
// containment is kill-on-close, any remaining descendants are terminated.
func (t *Tree) Close() error {
	return t.platform.close()
}

// TerminateWithGrace terminates the contained process tree after grace.
func (t *Tree) TerminateWithGrace(grace time.Duration) error {
	return terminateWithGrace(
		func() error { return t.platform.terminate(t.cmd) },
		func() bool { return t.platform.alive(t.cmd) },
		func() error { return t.platform.kill(t.cmd) },
		grace,
	)
}

func terminateWithGrace(
	terminate func() error,
	alive func() bool,
	kill func() error,
	grace time.Duration,
) error {
	var errs []error
	if err := terminate(); err != nil {
		errs = append(errs, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && alive() {
		time.Sleep(10 * time.Millisecond)
	}
	if alive() {
		if err := kill(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
