package telemetry

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionEndedDurationBuckets(t *testing.T) {
	for _, tc := range []struct {
		elapsed time.Duration
		bucket  string
	}{
		{time.Minute - time.Nanosecond, "under_1m"}, {time.Minute, "1_to_5m"},
		{5 * time.Minute, "5_to_30m"},
		{30 * time.Minute, "5_to_30m"}, {30*time.Minute + time.Nanosecond, "over_30m"},
	} {
		assert.Equal(t, tc.bucket, DurationBucket(tc.elapsed))
	}
}

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

func TestAppOpenedSurfaceAllowlist(t *testing.T) {
	reporter, err := NewReporter(Options{DistinctID: "anonymous-instance-id"})
	require.NoError(t, err)

	for _, surface := range []string{"cli", "tui", "web"} {
		props, err := reporter.SanitizeProperties("app_opened", map[string]any{"surface": surface, "path": "/example"})
		require.NoError(t, err)
		assert.Equal(t, surface, props["surface"])
		assert.NotContains(t, props, "path")
	}
	for _, surface := range []any{"desktop", "", 1} {
		props, err := reporter.SanitizeProperties("app_opened", map[string]any{"surface": surface})
		require.NoError(t, err)
		assert.NotContainsf(t, props, "surface", "surface %#v must be dropped", surface)
	}
	for _, bucket := range []string{"under_1m", "1_to_5m", "5_to_30m", "over_30m"} {
		props, err := reporter.SanitizeProperties("session_ended", map[string]any{"surface": "tui", "duration_bucket": bucket, "seconds": 120})
		require.NoError(t, err)
		assert.Equal(t, bucket, props["duration_bucket"])
		assert.Equal(t, "tui", props["surface"])
		assert.NotContains(t, props, "seconds")
	}
	for _, value := range []any{"invalid", 120} {
		props, err := reporter.SanitizeProperties("session_ended", map[string]any{"surface": "cli", "duration_bucket": value})
		require.NoError(t, err)
		assert.NotContains(t, props, "surface")
		assert.NotContains(t, props, "duration_bucket")
	}
	for _, event := range []string{"agent_active", "agent_call_count"} {
		for _, bucket := range []any{"1-10", "11-100", "over-100", "101", 11} {
			props, err := reporter.SanitizeProperties(event, map[string]any{"call_count_bucket": bucket, "actor": "example-agent", "path": "/example", "call_count": 11})
			require.NoError(t, err)
			assert.NotContains(t, props, "actor")
			assert.NotContains(t, props, "path")
			assert.NotContains(t, props, "call_count")
			if bucket == "1-10" || bucket == "11-100" || bucket == "over-100" {
				assert.Equal(t, bucket, props["call_count_bucket"])
			} else {
				assert.NotContains(t, props, "call_count_bucket")
			}
		}
	}
}

func TestScreenViewedProperties(t *testing.T) {
	reporter, err := NewReporter(Options{})
	require.NoError(t, err)
	for _, screen := range []string{"inbox", "today", "delegated", "scheduled", "issues", "logbook", "issue", "graph", "credentials", "projects", "daemons", "federation", "help", "empty"} {
		props, err := reporter.SanitizeProperties("screen_viewed", map[string]any{"screen": screen, "surface": "web", "title": "private"})
		require.NoError(t, err)
		assert.Equal(t, screen, props["screen"])
		assert.Equal(t, "web", props["surface"])
		assert.NotContains(t, props, "title")
	}
	for _, screen := range []any{"unknown", "", 1} {
		props, err := reporter.SanitizeProperties("screen_viewed", map[string]any{"screen": screen, "surface": "cli"})
		require.NoError(t, err)
		assert.NotContains(t, props, "screen")
		assert.NotContains(t, props, "surface")
	}
}
