package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const untrustedWebConfig = "[web]\npublic_origin = \"http://daemon.example\"\n"

func TestExportIgnoresInvalidServerConfigWithWarning(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	t.Setenv("KATA_SERVER", "")
	t.Chdir(t.TempDir())
	d := openKataTestDB(t, filepath.Join(home, "kata.db"))
	_, err := d.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	require.NoError(t, d.Close())
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(untrustedWebConfig), 0600))
	output := filepath.Join(home, "export.jsonl")
	_, stderr, err := runCmdCapture(t, nil, "export", "--output", output)
	require.NoError(t, err)
	assert.Contains(t, stderr, "[web]")
	data, err := os.ReadFile(output) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(data), "example-project")
}

func TestDaemonStartRejectsInvalidServerConfig(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(untrustedWebConfig), 0600))
	_, _, err := runCmdCapture(t, nil, "daemon", "start", "--foreground")
	require.ErrorContains(t, err, "web.public_origin")
}

func TestTUIConfigIgnoresInvalidServerConfig(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[tui]\nmouse = true\n"+untrustedWebConfig), 0600))
	got, err := resolveTUIConfig(newTUICmd(), false)
	require.NoError(t, err)
	assert.True(t, got.Mouse)
}

func TestCommandsRejectGloballyBrokenConfig(t *testing.T) {
	for name, content := range map[string]string{"syntax": "[web]\npublic_origin =\n", "unknown": "[web]\npublic_orign = \"https://daemon.example\"\n"} {
		for _, args := range [][]string{{"export"}, {"import", "--input", "missing.jsonl"}, {"list"}, {"daemon", "start", "--foreground"}, {"mcp", "serve"}, {"tui"}, {"whoami"}} {
			t.Run(name+"/"+args[0], func(t *testing.T) {
				home := setupKataEnv(t)
				require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0600))
				_, _, err := runCmdCapture(t, nil, args...)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "parse ")
			})
		}
	}
}

func TestMCPHTTPValidatesOwnListenerWithInvalidWeb(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(untrustedWebConfig), 0600))
	_, stderr, err := runCmdCapture(t, nil, "mcp", "serve", "--all", "--http", "0.0.0.0:0")
	require.ErrorContains(t, err, "--http-token-env")
	assert.Contains(t, stderr, "[web]")
}
