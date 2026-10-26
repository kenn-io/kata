package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/harness"
)

// Property 17: startup removes identity before preflight, including from children.
func TestDaemonChildEnvHasNoHarnessKeys(t *testing.T) {
	if os.Getenv("KATA_DAEMON_ENV_CHILD") != "1" {
		home := setupKataEnv(t)
		// Stop at config preflight without opening storage or starting workers.
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("["), 0o600))
		for i, key := range harness.EnvKeys() {
			value := "inherited-identity"
			if i%2 == 0 {
				value = ""
			}
			t.Setenv(key, value)
		}
		t.Setenv("KATA_DAEMON_ENV_CHILD", "1")
		require.Error(t, runDaemonProcess(t.Context(), "", false, false, nil))
		child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDaemonChildEnvHasNoHarnessKeys$") //nolint:gosec // G204: current test executable with fixed test arguments.
		output, err := child.CombinedOutput()
		assert.NoError(t, err, "spawned child: %s", output)
	}
	for _, key := range harness.EnvKeys() {
		_, present := os.LookupEnv(key)
		assert.False(t, present, "%s survived daemon startup", key)
	}
}
