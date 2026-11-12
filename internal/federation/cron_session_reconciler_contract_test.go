package federation_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/federation"
)

// The replica service can return its existing read-only result at adoption.
// Reconciliation should explain the configuration conflict to the operator.
type readOnlyAdoptionStore struct{ dbtest.TransactionalStore }

func (s readOnlyAdoptionStore) AdoptProjectIntoFederation(context.Context, db.AdoptProjectIntoFederationParams) (db.AdoptProjectIntoFederationResult, error) {
	return db.AdoptProjectIntoFederationResult{}, db.ErrFederatedReadOnly
}

func TestReconcileReadOnlyAdoptionGuidance(t *testing.T) {
	store := openReconcileStore(t)
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Example", Author: "worker"})
	require.NoError(t, err)
	wrapped := readOnlyAdoptionStore{dbtest.TransactionalStore{Storage: store}}
	err = federation.ReconcileMapping(t.Context(), wrapped, newFakeCredentialStore(), newFakeHub(), testCatalog(), testMapping(), nil)
	require.ErrorIs(t, err, federation.ErrConfigurationConflict)
	require.Contains(t, strings.ToLower(err.Error()), "hub")
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
