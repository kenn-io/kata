package config_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestConnectorConfigRejectsRelativeCommandAndDuplicateID(t *testing.T) {
	_, err := config.NormalizeConnectorConfigs([]config.ConnectorConfig{
		{ID: "notes", Command: "connector"},
		{ID: "notes", Command: "/opt/connectors/second"},
	})
	require.ErrorContains(t, err, `connector "notes": command must be absolute`)
}

func TestNormalizeConnectorConfigs(t *testing.T) {
	absoluteCommand := filepath.Join(t.TempDir(), "notes-connector")
	configs, err := config.NormalizeConnectorConfigs([]config.ConnectorConfig{{
		ID:             " Notes.Main ",
		Command:        absoluteCommand,
		TimeoutSeconds: 12,
		Env:            map[string]string{"TOKEN": "CONNECTOR_TOKEN"},
		Settings:       map[string]any{"enabled": true},
	}})
	require.NoError(t, err)
	require.Len(t, configs, 1)
	assert.Equal(t, "notes.main", configs[0].ID)
	assert.Equal(t, 12*time.Second, configs[0].Timeout())

	tests := []struct {
		name string
		cfg  config.ConnectorConfig
		want string
	}{
		{name: "missing ID", cfg: config.ConnectorConfig{Command: absoluteCommand}, want: "id is required"},
		{name: "invalid ID", cfg: config.ConnectorConfig{ID: "notes/one", Command: absoluteCommand}, want: "invalid id"},
		{name: "too many args", cfg: config.ConnectorConfig{ID: "notes", Command: absoluteCommand, Args: make([]string, 65)}, want: "at most 64 args"},
		{name: "negative timeout", cfg: config.ConnectorConfig{ID: "notes", Command: absoluteCommand, TimeoutSeconds: -1}, want: "timeout_seconds must be between 0 and 300"},
		{name: "too long timeout", cfg: config.ConnectorConfig{ID: "notes", Command: absoluteCommand, TimeoutSeconds: 301}, want: "timeout_seconds must be between 0 and 300"},
		{name: "invalid target environment", cfg: config.ConnectorConfig{ID: "notes", Command: absoluteCommand, Env: map[string]string{"bad-name": "SOURCE"}}, want: "invalid environment name"},
		{name: "invalid source environment", cfg: config.ConnectorConfig{ID: "notes", Command: absoluteCommand, Env: map[string]string{"TOKEN": "bad-name"}}, want: "invalid environment name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.NormalizeConnectorConfigs([]config.ConnectorConfig{tt.cfg})
			require.ErrorContains(t, err, tt.want)
		})
	}
}
