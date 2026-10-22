package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPreflightDaemonSQLiteBusyTimeout(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", "2s")
	startup, err := preflightDaemonStartup(context.Background(), "", false, false)
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, startup.StoreConfig.SQLiteBusyTimeout)
	t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", "0s")
	_, err = preflightDaemonStartup(context.Background(), "", false, false)
	require.ErrorContains(t, err, "storage.sqlite.busy_timeout")
}
