package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestTranscriptServiceConfigurationIsExplicitAndProjectScoped(t *testing.T) {
	cfg, err := readDocbank(t, `[transcript_service.project."00000000000000000000000001"]
enabled=true
receiver_url="https://receiver.example/internal"
locator_url="https://archive.example/team"
`)
	require.NoError(t, err)
	policy := cfg.TranscriptService.Project["00000000000000000000000001"]
	require.True(t, policy.Enabled)
	require.False(t, cfg.TranscriptService.Project["same-display-name"].Enabled)
	_, err = config.NormalizeTranscriptServiceConfig(config.TranscriptServiceConfig{Project: map[string]config.TranscriptServiceTarget{"same-display-name": policy}})
	require.Error(t, err)
	policy.ReceiverURL = "https://user:secret@receiver.example"
	_, err = config.NormalizeTranscriptServiceConfig(config.TranscriptServiceConfig{Project: map[string]config.TranscriptServiceTarget{"00000000000000000000000001": policy}})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}
