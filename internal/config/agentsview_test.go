package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestAgentsViewConfigurationIsExplicitAndProjectScoped(t *testing.T) {
	cfg, err := readDocbank(t, `[agentsview.project."00000000000000000000000001"]
enabled=true
receiver_url="https://receiver.example/internal"
locator_url="https://archive.example/team"
`)
	require.NoError(t, err)
	policy := cfg.AgentsView.Project["00000000000000000000000001"]
	require.True(t, policy.Enabled)
	require.False(t, cfg.AgentsView.Project["same-display-name"].Enabled)
	_, err = config.NormalizeAgentsViewConfig(config.AgentsViewConfig{Project: map[string]config.AgentsViewTarget{"same-display-name": policy}})
	require.Error(t, err)
	policy.ReceiverURL = "https://user:secret@receiver.example"
	_, err = config.NormalizeAgentsViewConfig(config.AgentsViewConfig{Project: map[string]config.AgentsViewTarget{"00000000000000000000000001": policy}})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}

func TestLegacyAgentsViewConfigurationMigratesAndRejectsAmbiguity(t *testing.T) {
	legacy, err := readDocbank(t, `[transcript_service.project."00000000000000000000000001"]
enabled=true
receiver_url="https://receiver.example/internal"
locator_url="https://archive.example/team"
`)
	require.NoError(t, err)
	require.True(t, legacy.AgentsView.Project["00000000000000000000000001"].Enabled)

	_, err = readDocbank(t, `[agentsview.project."00000000000000000000000001"]
enabled=true
receiver_url="https://receiver.example/internal"
locator_url="https://archive.example/team"

[transcript_service.project."00000000000000000000000002"]
enabled=true
receiver_url="https://other.example/internal"
locator_url="https://other.example/team"
`)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "receiver.example")
}
