package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
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

func TestReadCloseTranscriptProjectTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[close.transcript]
enabled = true
agentsview_url = "https://default.example"
[close.transcript.project."team-project"]
agentsview_url = "https://team.example/archive"
push = true
token_env = "MISSING_TRANSCRIPT_TOKEN"
[close.transcript.project."disabled-project"]
enabled = false
`), 0600))
	cfg, err := ReadCloseTranscriptConfig()
	require.NoError(t, err)
	target := cfg.Target("team-project")
	require.NotNil(t, target.Enabled)
	assert.True(t, *target.Enabled)
	assert.Equal(t, "https://team.example/archive", target.AgentsViewURL)
	assert.True(t, target.Push)
	assert.Equal(t, "MISSING_TRANSCRIPT_TOKEN", target.TokenEnv)
	assert.False(t, *cfg.Target("disabled-project").Enabled)
	assert.Empty(t, cfg.Target("disabled-project").AgentsViewURL)
	assert.Equal(t, "https://default.example", cfg.Target("other-project").AgentsViewURL)
	_, err = ReadDaemonConfig()
	require.NoError(t, err, "client preferences must not resolve AgentsView secrets at daemon startup")
}

func TestCloseTranscriptTargetURLIsolation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		globalURL := hegel.Draw(ht, hegel.Text())
		targetURL := hegel.Draw(ht, hegel.Text())
		push := hegel.Draw(ht, hegel.Booleans())
		cfg := CloseTranscriptConfig{AgentsViewURL: globalURL, Project: map[string]CloseTranscriptTarget{
			name: {AgentsViewURL: targetURL, Push: push, TokenEnv: "SELECTED_TOKEN"},
		}}
		got := cfg.Target(name)
		if got.AgentsViewURL != targetURL || got.Push != push || got.TokenEnv != "SELECTED_TOKEN" {
			ht.Fatalf("selected target changed: %#v", got)
		}
		other := cfg.Target(name + "\x00")
		if other.AgentsViewURL != globalURL || other.Push || other.TokenEnv != "" {
			ht.Fatalf("unmatched project inherited another project's upload authority: %#v", other)
		}
	}, hegel.WithTestCases(2000))
}

func TestCloseTranscriptTargetEnabledInheritance(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		global := hegel.Draw(ht, hegel.Booleans())
		override := hegel.Draw(ht, hegel.Booleans())
		present := hegel.Draw(ht, hegel.Booleans())
		name := hegel.Draw(ht, hegel.Text())
		target := CloseTranscriptTarget{}
		want := global
		if present {
			target.Enabled = &override
			want = override
		}
		cfg := CloseTranscriptConfig{Enabled: global, Project: map[string]CloseTranscriptTarget{name: target}}
		got := cfg.Target(name)
		if got.Enabled == nil || *got.Enabled != want || cfg.Project[name].Enabled != target.Enabled {
			ht.Fatalf("enabled inheritance mutated configuration or changed the selected value")
		}
	}, hegel.WithTestCases(2000))
}
