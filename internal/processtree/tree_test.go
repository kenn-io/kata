package processtree

import (
	"os"
	"os/exec"
	ossignal "os/signal"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTreeTerminateWithGraceBeforeStart(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "unused")
	tree, err := New(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })
	require.NoError(t, tree.TerminateWithGrace(time.Millisecond))
}

func TestTreeStartsAndWaitsForProcess(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessTreeExitHelper$") // #nosec G204,G702 -- fixed current test executable.
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

func TestTreeAliveWhileDescendantOutlivesProcess(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessTreeSpawnHelper$") // #nosec G204,G702 -- fixed current test executable.
	cmd.Env = append(os.Environ(), "GO_WANT_PROCESS_TREE_SPAWN_HELPER=1")
	tree, err := New(cmd)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tree.Close()) })

	require.NoError(t, tree.Start())
	require.NoError(t, tree.Wait())
	require.True(t, tree.Alive(), "the descendant should keep the tree alive after its parent exits")
}

func TestProcessTreeSpawnHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PROCESS_TREE_SPAWN_HELPER") != "1" {
		return
	}
	// The helper exits before returning, so t.Context never cancels the child.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessTreeSleepHelper$") // #nosec G204,G702 -- fixed current test executable.
	child.Env = append(os.Environ(), "GO_WANT_PROCESS_TREE_SPAWN_HELPER=", "GO_WANT_PROCESS_TREE_SLEEP_HELPER=1")
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestProcessTreeSleepHelper(_ *testing.T) {
	if os.Getenv("GO_WANT_PROCESS_TREE_SLEEP_HELPER") != "1" {
		return
	}
	// Block until the tree is killed. A signal wait keeps the runtime from
	// reporting a deadlock.
	interrupted := make(chan os.Signal, 1)
	ossignal.Notify(interrupted, os.Interrupt)
	<-interrupted
}
