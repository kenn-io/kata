package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func writeKataLocal(t *testing.T, dir, body string) {
	t.Helper()
	path := filepath.Join(dir, ".kata.local.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600)) //nolint:gosec
}

func TestReadLocalConfig_Missing(t *testing.T) {
	cfg, err := config.ReadLocalConfig(t.TempDir())
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, config.ErrLocalConfigMissing)
}

func TestReadLocalConfig_ServerOnly(t *testing.T) {
	dir := t.TempDir()
	writeKataLocal(t, dir, `version = 1

[server]
url = "http://100.64.0.5:7777"
`)
	cfg, err := config.ReadLocalConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.Version)
	assert.Equal(t, "http://100.64.0.5:7777", cfg.Server.URL)
}

func TestReadLocalConfig_NamedServerDaemonOnly(t *testing.T) {
	dir := t.TempDir()
	writeKataLocal(t, dir, `version = 1

[server]
daemon = "work"
`)
	cfg, err := config.ReadLocalConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "work", cfg.Server.Daemon)
}

func TestReadLocalConfig_RejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	writeKataLocal(t, dir, `version = 2

[server]
url = "http://x"
`)
	_, err := config.ReadLocalConfig(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported .kata.local.toml version")
}

func TestReadLocalConfig_EmptyServerURLIsZeroValue(t *testing.T) {
	dir := t.TempDir()
	writeKataLocal(t, dir, `version = 1

[server]
url = ""
`)
	cfg, err := config.ReadLocalConfig(dir)
	require.NoError(t, err)
	assert.Empty(t, cfg.Server.URL)
}

func TestReadLocalConfig_Malformed(t *testing.T) {
	dir := t.TempDir()
	writeKataLocal(t, dir, `version = 1
[server
url = "http://x"
`)
	_, err := config.ReadLocalConfig(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".kata.local.toml")
}

func TestMergeLocal_NilLocalReturnsBase(t *testing.T) {
	base := &config.ProjectConfig{
		Version: 1,
		Project: config.ProjectBindings{Name: "kata"},
	}
	got := config.MergeLocal(base, nil)
	assert.Equal(t, base, got)
	assert.NotSame(t, base, got, "MergeLocal must always return a copy, never the base pointer")
}

func TestMergeLocal_LocalServerWins(t *testing.T) {
	base := &config.ProjectConfig{
		Version: 1,
		Project: config.ProjectBindings{Name: "kata"},
		Server:  config.ServerConfig{Daemon: "work"},
	}
	local := &config.ProjectConfig{
		Version: 1,
		Server:  config.ServerConfig{URL: "http://100.64.0.5:7777"},
	}
	var stderr bytes.Buffer
	got := config.MergeLocalWithStderr(base, local, &stderr)
	assert.Equal(t, "kata", got.Project.Name)
	assert.Equal(t, "http://100.64.0.5:7777", got.Server.URL)
	assert.Empty(t, stderr.String())
}

func TestMergeLocal_LocalDaemonOverridesBase(t *testing.T) {
	base := &config.ProjectConfig{
		Version: 1,
		Project: config.ProjectBindings{Name: "kata"},
		Server:  config.ServerConfig{Daemon: "work"},
	}
	local := &config.ProjectConfig{
		Version: 1,
		Server:  config.ServerConfig{Daemon: "personal"},
	}
	got := config.MergeLocal(base, local)
	assert.Equal(t, "personal", got.Server.Daemon)
}

func TestMergeLocal_LocalNameOverridesBase(t *testing.T) {
	base := &config.ProjectConfig{
		Version: 1,
		Project: config.ProjectBindings{Name: "kata"},
	}
	local := &config.ProjectConfig{
		Version: 1,
		Project: config.ProjectBindings{Name: "Kata Local"},
	}
	got := config.MergeLocal(base, local)
	assert.Equal(t, "Kata Local", got.Project.Name)
}

func TestMergeLocal_LegacyIdentityIsIgnored(t *testing.T) {
	base := &config.ProjectConfig{
		Version: 1,
		Project: config.ProjectBindings{Name: "kata"},
	}
	local := &config.ProjectConfig{
		Version: 1,
		Project: config.ProjectBindings{LegacyIdentity: "github.com/other/repo"},
	}
	var stderr bytes.Buffer
	got := config.MergeLocalWithStderr(base, local, &stderr)
	assert.Equal(t, "kata", got.Project.Name)
	assert.Empty(t, stderr.String())
}
