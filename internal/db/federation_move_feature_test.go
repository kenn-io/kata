package db_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Move payloads carry a projection snapshot that older peers cannot apply.
// The existing feature negotiation must reject them before delivering data.
func TestFederationMoveRequiresFeature(t *testing.T) {
	require.Equal(t, "issue_move_v1", db.EventRequiredFeatures("issue.moved"))
	require.ErrorIs(t, db.RequireEventFeatures(db.CronEventFeature, db.EventRequiredFeatures("issue.moved")), db.ErrUnsupportedEventFeatures)
}

func TestMoveFeatureDoesNotImplyRecurringOwnership(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	tx, err := store.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	err = db.CheckRecurringSpokeEvent(t.Context(), tx, db.RemoteEvent{
		EventUID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Type: "issue.moved", Payload: []byte("{}"),
	})
	require.NoError(t, err, "ordinary move support does not imply hub-owned recurring metadata")
}
