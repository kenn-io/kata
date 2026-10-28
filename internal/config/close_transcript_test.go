package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCloseTranscriptConfig_InvalidAgentNamesKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript.session_id_env]\nPi = \"PI_SESSION_ID\"\n"), 0600))
	_, err := ReadCloseTranscriptConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Pi")
}

func TestReadCloseTranscriptConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	cfg, err := ReadCloseTranscriptConfig()
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	// Client attachment must not resolve unrelated daemon secrets or policy.
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[auth]
token_file = "/missing/daemon-secret"
[close.throttle]
window = "invalid"
[close.transcript]
enabled = true
agentsview_url = "https://agentsview.example"
[close.transcript.session_id_env]
pi = "PI_SESSION_ID"
`), 0600))
	cfg, err = ReadCloseTranscriptConfig()
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, "https://agentsview.example", cfg.AgentsViewURL)
	assert.Equal(t, map[string]string{"pi": "PI_SESSION_ID"}, cfg.SessionIDEnv)
}

func TestReadCloseTranscriptConfig_ParseErrorNamesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[close.transcript\n"), 0600))
	_, err := ReadCloseTranscriptConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	_, ok := errors.AsType[toml.ParseError](err)
	assert.True(t, ok, "the parse position must reach the warning")
}

// The client preference shares the daemon's strict config file, so the
// daemon must still start when a user enables it.
func TestReadDaemonConfigAcceptsCloseTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = true
agentsview_url = "https://agentsview.example"
[close.transcript.session_id_env]
pi = "PI_SESSION_ID"
`), 0600))
	_, err := ReadDaemonConfig()
	require.NoError(t, err)
}
