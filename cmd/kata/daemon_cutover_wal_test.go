package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestForegroundRefusesLiveDaemonBeforeCutover(t *testing.T) {
	home := setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	path := filepath.Join(home, "kata.db")
	source, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	writeRuntimePID(t, home, os.Getpid())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = runDaemon(ctx)
	require.ErrorContains(t, err, "daemon already running")
	version, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, 28, version, "must refuse before migrating the live daemon's database")
}
