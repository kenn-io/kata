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
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript.session_id_env]\nPi = \"PI_SESSION_ID\"\n"), 0600))
	_, err := ReadCloseTranscriptConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Pi")
}

func TestReadCloseTranscriptConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	cfg, err := ReadCloseTranscriptConfig()
	require.NoError(t, err)
	assert.Equal(t, CloseTranscriptOff, cfg.Mode)
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
	assert.Equal(t, CloseTranscriptOn, cfg.Mode)
	assert.Equal(t, "https://agentsview.example", cfg.AgentsViewURL)
	assert.Equal(t, map[string]string{"pi": "PI_SESSION_ID"}, cfg.SessionIDEnv)
}

func TestReadCloseTranscriptConfig_ParseErrorNamesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	path := filepath.Join(home, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[close.transcript\n"), 0600))
	_, err := ReadCloseTranscriptConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), path)
	_, ok := errors.AsType[toml.ParseError](err)
	assert.True(t, ok, "the parse position must reach the warning")
}

func TestReadCloseTranscriptConfig_EnabledEnvironment(t *testing.T) {
	t.Run("true keeps the file's link and session settings", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", " true ")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = false
agentsview_url = "https://agentsview.example"
[close.transcript.session_id_env]
pi = "PI_SESSION_ID"
`), 0600))
		cfg, err := ReadCloseTranscriptConfig()
		require.NoError(t, err)
		assert.Equal(t, CloseTranscriptConfig{Mode: CloseTranscriptOn, AgentsViewURL: "https://agentsview.example", SessionIDEnv: map[string]string{"pi": "PI_SESSION_ID"}, override: new(CloseTranscriptOn)}, cfg)
	})
	t.Run("false ignores the file's invalid session settings", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "false")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = true\n[close.transcript.session_id_env]\nPi = \"PI_SESSION_ID\"\n"), 0600))
		cfg, err := ReadCloseTranscriptConfig()
		require.NoError(t, err)
		assert.Equal(t, CloseTranscriptOff, cfg.Mode)
	})
	t.Run("auto keeps the file's link and session settings", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", " Auto ")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = false\nagentsview_url = \"https://agentsview.example\"\n"), 0600))
		cfg, err := ReadCloseTranscriptConfig()
		require.NoError(t, err)
		assert.Equal(t, CloseTranscriptConfig{Mode: CloseTranscriptAuto, AgentsViewURL: "https://agentsview.example", override: new(CloseTranscriptAuto)}, cfg)
	})
	t.Run("an invalid value is an error naming the variable", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("KATA_HOME", home)
		t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "sometimes")
		require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = true\n"), 0600))
		_, err := ReadCloseTranscriptConfig()
		require.ErrorContains(t, err, "KATA_CLOSE_TRANSCRIPT_ENABLED")
	})
}

func TestReadCloseTranscriptConfig_ProjectAuto(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = "auto"
[close.transcript.project.a]
enabled = "auto"
[close.transcript.project.b]
enabled = false
[close.transcript.project.c]
agentsview_url = "https://c.example"
`), 0600))
	cfg, err := ReadCloseTranscriptConfig()
	require.NoError(t, err)
	assert.Equal(t, CloseTranscriptAuto, *cfg.Target("a").Mode)
	assert.Equal(t, CloseTranscriptOff, *cfg.Target("b").Mode)
	assert.Equal(t, CloseTranscriptAuto, *cfg.Target("c").Mode, "an omitted enabled inherits the global mode")
	assert.True(t, cfg.AnyEnabled())
	assert.False(t, cfg.AnyOn(), "auto never asks for a missing-session warning")

}

func TestReadCloseTranscriptConfig_ProjectTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = true
agentsview_url = "https://global.example"
[close.transcript.project.team]
enabled = false
agentsview_url = "https://team.example"
`), 0600))
	for _, tc := range []struct {
		override string
		team     CloseTranscriptMode
		global   CloseTranscriptMode
	}{
		{"", CloseTranscriptOff, CloseTranscriptOn},
		{"true", CloseTranscriptOn, CloseTranscriptOn},
		{"auto", CloseTranscriptAuto, CloseTranscriptAuto},
		{"false", CloseTranscriptOff, CloseTranscriptOff},
	} {
		t.Run("override="+tc.override, func(t *testing.T) {
			t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", tc.override)
			cfg, err := ReadCloseTranscriptConfig()
			require.NoError(t, err)
			team, global := cfg.Target("team"), cfg.Target("other")
			require.NotNil(t, team.Mode)
			require.NotNil(t, global.Mode)
			assert.Equal(t, tc.team, *team.Mode)
			assert.Equal(t, tc.global, *global.Mode)
			if tc.override != "false" {
				assert.Equal(t, "https://team.example", team.AgentsViewURL)
				assert.Equal(t, "https://global.example", global.AgentsViewURL)
			}
		})
	}
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
[close.transcript.project.team]
enabled = false
agentsview_url = "https://team.example"
`), 0600))
	_, err := ReadDaemonConfig()
	require.NoError(t, err)
}

func TestReadCloseTranscriptConfig_AutoInFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[close.transcript]\nenabled = \"AUTO\"\n"), 0600))
	cfg, err := ReadCloseTranscriptConfig()
	require.NoError(t, err)
	assert.Equal(t, CloseTranscriptAuto, cfg.Mode)
	// The daemon shares the file and must still start.
	_, err = ReadDaemonConfig()
	require.NoError(t, err)
}

// Only kata close checks enabled, so a mistake there never stops the daemon
// or drops the file's auth settings from other commands.
func TestReadCloseTranscriptConfig_InvalidEnabledStaysClientSide(t *testing.T) {
	for _, tc := range []struct{ name, section, value, key string }{
		{"typo", "[close.transcript]", `"atuo"`, "close.transcript.enabled"},
		{"quoted boolean", "[close.transcript]", `"true"`, "close.transcript.enabled"},
		{"integer", "[close.transcript]", `1`, "close.transcript.enabled"},
		{"project typo", "[close.transcript.project.team]", `"atuo"`, "close.transcript.project.team.enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			t.Setenv("KATA_CLOSE_TRANSCRIPT_ENABLED", "")
			t.Setenv("KATA_AUTH_TOKEN", "")
			path := filepath.Join(home, "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[auth]\ntoken = \"file-token\"\n"+tc.section+"\nenabled = "+tc.value+"\n"), 0600))
			_, err := ReadCloseTranscriptConfig()
			require.ErrorContains(t, err, path+": "+tc.key+` must be true, false or "auto"`)
			_, err = ReadDaemonConfig()
			require.NoError(t, err)
			auth, err := ReadAuthConfig()
			require.NoError(t, err)
			assert.Equal(t, "file-token", auth.Token)
		})
	}
}
