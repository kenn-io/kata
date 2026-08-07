package telemetry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKitPostHogDisabledBuildTagDisablesStandaloneBinary(t *testing.T) {
	goEnv := exec.Command("go", "env", "GOMODCACHE") //nolint:gosec // Fixed Go command resolves the caller's provisioned module cache.
	moduleCacheOutput, err := goEnv.CombinedOutput()
	require.NoErrorf(t, err, "resolve caller module cache: %s", moduleCacheOutput)
	moduleCache := strings.TrimSpace(string(moduleCacheOutput))
	require.NotEmpty(t, moduleCache)

	scratch := t.TempDir()
	binary := filepath.Join(scratch, "telemetry-testprogram")
	home := filepath.Join(scratch, "home")
	command := exec.Command("go", "build", "-tags", "kit_posthog_disabled", "-o", binary, "./testprogram") //nolint:gosec // Fixed Go tool and arguments build the test fixture.
	command.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(scratch, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "xdg-config"),
		"GOCACHE="+filepath.Join(scratch, "gocache"),
		"GOMODCACHE="+moduleCache,
		"GOPROXY=off",
	)
	output, err := command.CombinedOutput()
	require.NoErrorf(t, err, "build standalone test program: %s", output)

	command = exec.Command(binary) //nolint:gosec // The test owns and just built this binary in its scratch directory.
	command.Env = append(os.Environ(),
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(scratch, "xdg-cache"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "xdg-config"),
		"TELEMETRY_ENABLED=1",
		EnabledEnv+"=1",
	)
	output, err = command.CombinedOutput()
	require.NoErrorf(t, err, "run standalone test program: %s", output)
	assert.Equal(t, "disabled", strings.TrimSpace(string(output)))
}

func TestEnabledFromEnvDisabledDuringGoTests(t *testing.T) {
	t.Setenv("TELEMETRY_ENABLED", "1")
	t.Setenv(EnabledEnv, "1")

	assert.False(t, EnabledFromEnv())
}

func TestNewReporterDisabledDuringGoTests(t *testing.T) {
	t.Setenv("TELEMETRY_ENABLED", "1")
	t.Setenv(EnabledEnv, "1")

	reporter, err := NewReporter(Options{})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}
