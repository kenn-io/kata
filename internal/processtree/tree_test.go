package processtree

import (
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTerminateWithGraceNoProcess(t *testing.T) {
	cmd := exec.Command("unused")
	require.NoError(t, TerminateWithGrace(cmd, time.Millisecond))
}
