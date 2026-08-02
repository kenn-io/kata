//go:build !windows

package processtree

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTerminateWithGraceExitedProcess(t *testing.T) {
	cmd := exec.Command("true")
	Prepare(cmd)
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())

	require.NoError(t, TerminateWithGrace(cmd, time.Millisecond))
}

func TestExitedProcessSignalsAreSuccessful(t *testing.T) {
	cmd := exec.Command("true")
	Prepare(cmd)
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())

	require.NoError(t, terminate(cmd))
	require.NoError(t, kill(cmd))
}
