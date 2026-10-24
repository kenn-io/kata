package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestForegroundRefusesLiveDaemonBeforeCutover(t *testing.T) {
	for _, recordType := range []string{"local-live", "unknown"} {
		t.Run(recordType, func(t *testing.T) {
			resetFlags(t)
			home := setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			path := filepath.Join(home, "kata.db")
			source, err := sqlitestore.Open(t.Context(), path)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
			require.NoError(t, err)
			require.NoError(t, source.Close())
			child := startSleepProcess(t)
			var recordPath string
			if recordType == "local-live" {
				writeRuntimePID(t, home, child.Process.Pid)
			} else {
				recordPath = writeUnknownRuntimePID(t, child.Process.Pid)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err = runDaemon(ctx)
			require.ErrorContains(t, err, "daemon already running")
			if recordType == "unknown" {
				require.ErrorContains(t, err, recordPath)
				require.ErrorContains(t, err, "remove that file if no kata daemon owns that PID")
			}
			version, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
			require.NoError(t, err)
			require.Equal(t, 28, version, "must refuse before migrating the live daemon's database")
		})
	}
}
