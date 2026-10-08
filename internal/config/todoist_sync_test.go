package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestTodoistSyncConfig(t *testing.T) {
	got, err := config.NormalizeTodoistSyncConfig(config.TodoistSyncConfig{})
	require.NoError(t, err)
	require.Equal(t, "https://api.todoist.com", got.APIOrigin)
	require.Equal(t, "KATA_TODOIST_TOKEN", got.TokenEnv)
	for _, origin := range []string{"https://foreign.example", "http://192.168.1.1", "https://person:secret@api.todoist.com"} {
		_, err := config.NormalizeTodoistSyncConfig(config.TodoistSyncConfig{APIOrigin: origin})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
	}
	got, err = config.NormalizeTodoistSyncConfig(config.TodoistSyncConfig{APIOrigin: "http://127.0.0.1:9876/", TokenEnv: " EXAMPLE_TASK_TOKEN "})
	require.NoError(t, err)
	require.Equal(t, "EXAMPLE_TASK_TOKEN", got.TokenEnv)
	_, err = config.NormalizeTodoistSyncConfig(config.TodoistSyncConfig{TokenEnv: "TOKEN=secret"})
	require.Error(t, err)
}

func TestReadDaemonTodoistConfig(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[todoist_sync]\ntoken_env='EXAMPLE_TODOIST_TOKEN'"), 0600))
	cfg, err := config.ReadDaemonConfigForHome(home)
	require.NoError(t, err)
	require.Equal(t, "EXAMPLE_TODOIST_TOKEN", cfg.TodoistSync.TokenEnv)
	require.Equal(t, "https://api.todoist.com", cfg.TodoistSync.APIOrigin)
}
