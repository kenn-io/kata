//go:build !windows

package processtree

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTreeTerminateWithGraceExitedProcess(t *testing.T) {
	cmd := exec.Command("true")
	tree, err := New(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })
	require.NoError(t, tree.Start())
	require.NoError(t, tree.Wait())

	require.NoError(t, tree.TerminateWithGrace(time.Millisecond))
}

func TestExitedProcessSignalsAreSuccessful(t *testing.T) {
	cmd := exec.Command("true")
	prepare(cmd)
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())

	require.NoError(t, terminate(cmd))
	require.NoError(t, kill(cmd))
}
