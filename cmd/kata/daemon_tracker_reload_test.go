package main

import (
	"errors"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/tracker"
	"path/filepath"
	"testing"
)

func TestTrackerParseFailureSuspendsRunningSnapshot(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "tracker.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	r, err := daemon.NewTrackerRuntime(ctx, store, nil, nil)
	require.NoError(t, err)
	err = reloadTrackerConnections(ctx, r, func() (*config.DaemonConfig, error) { return nil, errors.New("fixture-private-diagnostic") })
	require.Error(t, err)
	require.NotContains(t, err.Error(), "fixture-private")
	_, err = r.Connections(ctx, "01ARZ3NDEKTSV4RRFFQ69G5FAV", tracker.Principal{})
	require.ErrorIs(t, err, tracker.ErrUnavailable)
}

func TestTrackerPolicyErrorsAreClassifiedWithoutRawDiagnostics(t *testing.T) {
	require.ErrorIs(t, safeTrackerPolicyError(tracker.ErrGenerationRetired), tracker.ErrGenerationRetired)
	require.ErrorIs(t, safeTrackerPolicyError(errors.New("fixture-private-diagnostic")), tracker.ErrUnavailable)
	require.NotContains(t, safeTrackerPolicyError(errors.New("fixture-private-diagnostic")).Error(), "fixture-private")
}
