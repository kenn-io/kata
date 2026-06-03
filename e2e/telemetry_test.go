package e2e_test

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2EBinaryUsesTelemetryDisabledBuildTag(t *testing.T) {
	bin := buildKataBinary(t)

	out, err := exec.Command("go", "version", "-m", bin).CombinedOutput() //nolint:gosec // fixed go tool args against test-built binary
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "kata_e2e")
}
