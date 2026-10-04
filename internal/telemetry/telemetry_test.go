package telemetry

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	binaryName := "telemetry-testprogram"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(scratch, binaryName)
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

// Exercise the standalone product wrapper: Go tests deliberately disable
// telemetry regardless of environment, so an in-process check is insufficient.
func TestStandaloneTelemetryOptOutSpellings(t *testing.T) {
	scratch := t.TempDir()
	binary := filepath.Join(scratch, "telemetry-opt-out")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "./testprogram") //nolint:gosec // fixed tool and test-owned output
	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))
	for _, value := range []string{"0", "false", "no", "off", " FALSE ", "No", "OFF"} {
		t.Run(value, func(t *testing.T) {
			command := exec.Command(binary) //nolint:gosec // test-owned standalone fixture
			command.Env = append(os.Environ(), "KATA_HOME="+scratch, "KATA_DB="+filepath.Join(scratch, "kata.db"), "TELEMETRY_ENABLED=1", EnabledEnv+"="+value)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
			require.Equal(t, "disabled", strings.TrimSpace(string(output)))
		})
	}
}

func TestNewReporterDisabledDuringGoTests(t *testing.T) {
	t.Setenv("TELEMETRY_ENABLED", "1")
	t.Setenv(EnabledEnv, "1")

	reporter, err := NewReporter(Options{})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}
