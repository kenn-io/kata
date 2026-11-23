package daemon

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFederationPollOneCPUQuota(t *testing.T) {
	if os.Getenv("KATA_TEST_ONE_CPU_QUOTA") == "1" {
		hub := newInternalLongPollHub(t)
		require.Equal(t, 32, hub.waits.maxTotal)
		return
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run is unavailable")
	}
	probe := exec.CommandContext(t.Context(), "systemctl", "--user", "show-environment")
	if err := probe.Run(); err != nil {
		t.Skip("user systemd is unavailable")
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), "systemd-run", "--user", "--scope", "-p", "CPUQuota=100%", exe, "-test.run=^TestFederationPollOneCPUQuota$") //nolint:gosec // exe is this test binary.
	for _, env := range os.Environ() {
		if !strings.HasPrefix(env, "GOMAXPROCS=") {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "KATA_TEST_ONE_CPU_QUOTA=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}
