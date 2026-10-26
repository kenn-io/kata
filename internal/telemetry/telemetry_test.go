package telemetry

import (
	"fmt"
	"maps"
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

func TestMain(m *testing.M) {
	for _, key := range []string{"CI", "DO_NOT_TRACK"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

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

// testProjectKeyFlag links a placeholder PostHog project key, as release builds
// link the real one.
const testProjectKeyFlag = "-X go.kenn.io/kit/telemetry/posthog.projectKey=phc_test_project_key"

// Exercise the standalone product wrapper with and without a linked project key.
func TestStandaloneTelemetryAdmission(t *testing.T) {
	scratch := t.TempDir()
	build := func(name string, args ...string) string {
		binary := filepath.Join(scratch, name)
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		command := exec.Command("go", append(append([]string{"build"}, args...), "-o", binary, "./testprogram")...) //nolint:gosec // fixed tool and test-owned output
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		return binary
	}
	release := build("telemetry-release", "-ldflags", testProjectKeyFlag)
	source := build("telemetry-source")
	for _, tc := range []struct{ name, binary, env, version, want string }{
		{"enabled", release, EnabledEnv + "=1", "v1.2.3", "enabled"},
		{"0", release, EnabledEnv + "=0", "v1.2.3", "disabled"},
		{"development", release, EnabledEnv + "=1", "dev", "disabled"},
		{"no project key", source, EnabledEnv + "=1", "v1.2.3", "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := exec.Command(tc.binary, "-version", tc.version) //nolint:gosec // test-owned standalone fixture
			command.Env = append(os.Environ(), "KATA_HOME="+scratch, "KATA_DB="+filepath.Join(scratch, "kata.db"), "TELEMETRY_ENABLED=1", EnabledEnv+"=1", tc.env)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
			require.Equal(t, tc.want, strings.TrimSpace(string(output)))
		})
	}
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

func TestProjectCountBucket(t *testing.T) {
	for _, tc := range []struct {
		count  int
		bucket string
	}{
		{0, "0-5"}, {1, "0-5"}, {5, "0-5"},
		{6, "6-20"}, {20, "6-20"}, {21, "21-100"},
		{100, "21-100"}, {101, "over-100"},
	} {
		assert.Equal(t, tc.bucket, ProjectCountBucket(tc.count))
	}
}

func TestDaemonProjectCountAllowlist(t *testing.T) {
	reporter, err := NewReporter(Options{})
	require.NoError(t, err)
	for _, event := range []string{"daemon_started", "daemon_active"} {
		for _, bucket := range []string{"0-5", "6-20", "21-100", "over-100"} {
			props, err := reporter.SanitizeProperties(event, map[string]any{"project_count": bucket})
			require.NoError(t, err)
			assert.Equal(t, bucket, props["project_count"])
		}
		for _, count := range []any{37, "37"} {
			props, err := reporter.SanitizeProperties(event, map[string]any{"project_count": count})
			require.NoError(t, err)
			assert.NotContainsf(t, props, "project_count", "%s must drop count %#v", event, count)
		}
	}
}

func TestCoreActionSurfaceAllowlist(t *testing.T) {
	reporter, err := NewReporter(Options{})
	require.NoError(t, err)
	for _, event := range []string{"issue_created", "issue_closed"} {
		props, err := reporter.SanitizeProperties(event, map[string]any{"surface": "web"})
		require.NoError(t, err)
		assert.Equal(t, "web", props["surface"])
		for _, surface := range []any{"unsupported", nil} {
			_, err := reporter.SanitizeProperties(event, map[string]any{"surface": surface})
			require.Error(t, err)
		}
		_, err = reporter.SanitizeProperties(event, nil)
		require.Error(t, err)
	}
}

func TestSetupFinishedProperties(t *testing.T) {
	reporter, err := NewReporter(Options{DistinctID: "anonymous-install"})
	require.NoError(t, err)
	require.True(t, reporter.EventAllowed("setup_finished"))
	properties, err := reporter.SanitizeProperties("setup_finished", map[string]any{"project_count": 1})
	require.NoError(t, err)
	require.NotContains(t, properties, "project_count")
}

// Outcomes send only fixed values, with unknown values collapsed to other.
func TestOutcomesProperties(t *testing.T) {
	reporter, err := NewReporter(Options{})
	require.NoError(t, err)
	for _, tc := range []struct {
		event       string
		props, want map[string]any
	}{
		{"error_shown", map[string]any{"surface": "web"}, nil},
		{"search_empty", map[string]any{"surface": "cli", "query": "private text"}, map[string]any{"surface": "cli"}},
		{"error_shown", map[string]any{"surface": "tui", "kind": "issue_created"}, map[string]any{"surface": "tui", "kind": "issue_created"}},
		{"error_shown", map[string]any{"surface": "web", "kind": "unlisted"}, map[string]any{"surface": "web", "kind": "other"}},
		{"sync_failed", map[string]any{"reason": "auth"}, nil},
		{"sync_failed", map[string]any{"sync_source": "github"}, nil},
		{"sync_failed", map[string]any{"sync_source": "github", "reason": "auth"}, map[string]any{"sync_source": "github", "reason": "auth"}},
		{"sync_failed", map[string]any{"sync_source": "unlisted", "reason": "private text"}, map[string]any{"sync_source": "other", "reason": "other"}},
	} {
		properties, err := reporter.SanitizeProperties(tc.event, tc.props)
		if tc.want == nil {
			require.Error(t, err)
			continue
		}
		require.NoError(t, err)
		maps.Copy(tc.want, map[string]any{
			"$geoip_disable": true, "$process_person_profile": false,
			"application": "", "commit": "", "distribution": "", "source": "", "version": "",
			"goarch": runtime.GOARCH, "goos": runtime.GOOS,
		})
		require.Equal(t, tc.want, properties)
	}
	for _, source := range []string{"github", "linear", "notion", "plane", "ticktick", "todoist", "twenty", "federation"} {
		properties, err := reporter.SanitizeProperties("sync_failed", map[string]any{"sync_source": source, "reason": "server"})
		require.NoError(t, err)
		require.Equal(t, source, properties["sync_source"])
	}
}
