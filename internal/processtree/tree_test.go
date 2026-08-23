package processtree

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTreeTerminateWithGraceBeforeStart(t *testing.T) {
	cmd := exec.Command("unused")
	tree, err := New(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })
	require.NoError(t, tree.TerminateWithGrace(time.Millisecond))
}

func TestTreeStartsAndWaitsForProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessTreeExitHelper$") // #nosec G204,G702 -- fixed current test executable.
	cmd.Env = append(os.Environ(), "GO_WANT_PROCESS_TREE_EXIT_HELPER=1")
	tree, err := New(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	require.NoError(t, tree.Start())
	require.NoError(t, tree.Wait())
}

func TestProcessTreeExitHelper(_ *testing.T) {
	if os.Getenv("GO_WANT_PROCESS_TREE_EXIT_HELPER") != "1" {
		return
	}
	os.Exit(0)
}
