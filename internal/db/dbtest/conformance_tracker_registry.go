package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

func checkTrackerRegistry(t *testing.T, store db.Storage) error {
	registry := store.(db.TrackerRegistryStore)
	epoch := "01ARZ3NDEKTSV4RRFFQ69G5FB0"
	policy := tracker.RegistryPolicy{Selection: tracker.Selection{OwnerInstanceUID: store.InstanceUID(), ConnectionUID: "01ARZ3NDEKTSV4RRFFQ69G5FAY", Generation: "first"}, Provider: "notion", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	records, err := registry.ReconcileTrackerRegistry(t.Context(), epoch, []tracker.RegistryPolicy{policy})
	require.NoError(t, err)
	require.Len(t, records, 1)
	_, err = registry.ReconcileTrackerRegistry(t.Context(), epoch, nil)
	require.NoError(t, err)
	_, err = registry.ReconcileTrackerRegistry(t.Context(), epoch, []tracker.RegistryPolicy{policy})
	require.ErrorIs(t, err, tracker.ErrGenerationRetired)
	policy.Selection.Generation = "second"
	records, err = registry.ReconcileTrackerRegistry(t.Context(), epoch, []tracker.RegistryPolicy{policy})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Len(t, records[0].Retired, 1)
	policy.Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	_, err = registry.ReconcileTrackerRegistry(t.Context(), epoch, []tracker.RegistryPolicy{policy})
	require.ErrorIs(t, err, tracker.ErrGenerationChanged)
	return nil
}
