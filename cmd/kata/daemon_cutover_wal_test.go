package main

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestForegroundRefusesStartupBeforeCutover(t *testing.T) {
	for _, tc := range []struct {
		name             string
		occupiedEndpoint bool
		wantError        string
	}{
		{name: "live local record", wantError: "daemon already running"},
		{name: "occupied endpoint", occupiedEndpoint: true, wantError: "bind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupKataEnv(t)
			t.Setenv("KATA_DSN", "")
			t.Setenv("PORT", "")
			path := filepath.Join(home, "kata.db")
			source, err := sqlitestore.Open(t.Context(), path)
			require.NoError(t, err)
			_, err = source.ExecContext(t.Context(), `UPDATE meta SET value='28' WHERE key='schema_version'`)
			require.NoError(t, err)
			require.NoError(t, source.Close())
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			if tc.occupiedEndpoint {
				occupied, listenErr := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, listenErr)
				t.Cleanup(func() { require.NoError(t, occupied.Close()) })
				err = runDaemonWithListen(ctx, occupied.Addr().String(), false, false)
			} else {
				writeRuntimePID(t, home, startSleepProcess(t).Process.Pid)
				err = runDaemon(ctx)
			}
			require.ErrorContains(t, err, tc.wantError)
			version, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
			require.NoError(t, err)
			require.Equal(t, 28, version, "must refuse before migrating storage")
		})
	}
}
