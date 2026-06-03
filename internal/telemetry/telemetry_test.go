package telemetry

import (
	"runtime"
	"testing"

	"github.com/posthog/posthog-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePostHogClient struct {
	message posthog.Message
}

func (f *fakePostHogClient) Enqueue(message posthog.Message) error {
	f.message = message
	return nil
}

func (f *fakePostHogClient) Close() error { return nil }

func TestEnabledFromEnvDisabledDuringGoTests(t *testing.T) {
	t.Setenv(EnabledEnv, "1")

	assert.False(t, EnabledFromEnv())
}

func TestNewReporterDisabledByEnvDoesNotRequireDistinctID(t *testing.T) {
	t.Setenv(EnabledEnv, "0")

	reporter, err := NewReporter(Options{})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func TestNewReporterRequiresAnonymousDistinctIDWhenEnabled(t *testing.T) {
	_, err := newReporter(Options{}, true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "distinct id")
}

func TestReporterCaptureUsesAnonymousDistinctID(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-instance-id",
		version:    "v-test",
		commit:     "abc1234",
		enabled:    true,
	}

	err := reporter.Capture("daemon_started", map[string]any{
		"$process_person_profile": true,
		"$geoip_disable":          false,
		"application":             "evil",
		"distinct_id":             "user-provided",
		"project":                 "secret-project",
		"project_count":           3,
		"source":                  "evil",
		"version":                 "evil",
	})
	require.NoError(t, err)

	capture, ok := client.message.(posthog.Capture)
	require.True(t, ok)
	assert.Equal(t, "anonymous-instance-id", capture.DistinctId)
	assert.Equal(t, "daemon_started", capture.Event)
	assert.Equal(t, 3, capture.Properties["project_count"])
	assert.NotContains(t, capture.Properties, "distinct_id")
	assert.NotContains(t, capture.Properties, "project")
	assert.False(t, capture.Properties["$process_person_profile"].(bool))
	assert.True(t, capture.Properties["$geoip_disable"].(bool))
	assert.Equal(t, "kata", capture.Properties["application"])
	assert.Equal(t, "v-test", capture.Properties["version"])
	assert.Equal(t, "abc1234", capture.Properties["commit"])
	assert.Equal(t, runtime.GOOS, capture.Properties["goos"])
	assert.Equal(t, runtime.GOARCH, capture.Properties["goarch"])
	assert.Equal(t, "daemon", capture.Properties["source"])
	assert.NotContains(t, capture.Properties, "app")
}

func TestReporterCaptureAllowsDaemonActive(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-instance-id",
		version:    "v-test",
		commit:     "abc1234",
		enabled:    true,
	}

	err := reporter.Capture("daemon_active", map[string]any{"project_count": 2})
	require.NoError(t, err)

	capture, ok := client.message.(posthog.Capture)
	require.True(t, ok)
	assert.Equal(t, "daemon_active", capture.Event)
	assert.Equal(t, 2, capture.Properties["project_count"])
	assert.False(t, capture.Properties["$process_person_profile"].(bool))
	assert.True(t, capture.Properties["$geoip_disable"].(bool))
	assert.Equal(t, "kata", capture.Properties["application"])
	assert.Equal(t, "v-test", capture.Properties["version"])
	assert.Equal(t, "abc1234", capture.Properties["commit"])
	assert.Equal(t, runtime.GOOS, capture.Properties["goos"])
	assert.Equal(t, runtime.GOARCH, capture.Properties["goarch"])
	assert.Equal(t, "daemon", capture.Properties["source"])
	assert.NotContains(t, capture.Properties, "app")
}

func TestReporterCaptureRejectsUnsupportedEvents(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-instance-id",
		enabled:    true,
	}

	err := reporter.Capture("issue_created", nil)
	require.ErrorIs(t, err, ErrUnsupportedEvent)
}
