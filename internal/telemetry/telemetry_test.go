package telemetry

import (
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

func TestNewReporterDisabledByEnvDoesNotRequireDistinctID(t *testing.T) {
	t.Setenv(EnabledEnv, "0")

	reporter, err := NewReporter(Options{})
	require.NoError(t, err)

	assert.False(t, reporter.Enabled())
}

func TestNewReporterRequiresAnonymousDistinctID(t *testing.T) {
	t.Setenv(EnabledEnv, "")

	_, err := NewReporter(Options{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "distinct id")
}

func TestReporterCaptureUsesAnonymousDistinctID(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-instance-id",
		enabled:    true,
	}

	err := reporter.Capture("daemon_started", map[string]any{
		"$geoip_disable": false,
		"distinct_id":    "user-provided",
		"project":        "secret-project",
		"project_count":  3,
	})
	require.NoError(t, err)

	capture, ok := client.message.(posthog.Capture)
	require.True(t, ok)
	assert.Equal(t, "anonymous-instance-id", capture.DistinctId)
	assert.Equal(t, "daemon_started", capture.Event)
	assert.Equal(t, 3, capture.Properties["project_count"])
	assert.NotContains(t, capture.Properties, "distinct_id")
	assert.NotContains(t, capture.Properties, "project")
	assert.True(t, capture.Properties["$geoip_disable"].(bool))
}

func TestReporterCaptureAllowsDaemonActive(t *testing.T) {
	client := &fakePostHogClient{}
	reporter := &Reporter{
		client:     client,
		distinctID: "anonymous-instance-id",
		enabled:    true,
	}

	err := reporter.Capture("daemon_active", map[string]any{"project_count": 2})
	require.NoError(t, err)

	capture, ok := client.message.(posthog.Capture)
	require.True(t, ok)
	assert.Equal(t, "daemon_active", capture.Event)
	assert.Equal(t, 2, capture.Properties["project_count"])
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
