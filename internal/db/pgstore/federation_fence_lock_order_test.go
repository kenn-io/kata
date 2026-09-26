package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// TestFederationEnrollmentFenceDoesNotDeadlockAgainstBindingFirstWriter pins the
// lock order of FederationEnrollmentTransactionFence against the order the
// federation ingest writer uses. Ingest locks the project binding first
// (requireFederationIngestHub) and updates the enrollment later, so the fence
// must take the binding before the enrollment. The reverse order lets the two
// transactions acquire the same two rows in opposite order and Postgres kills
// one after deadlock_timeout; the losing fenced request then surfaces as a 503.
func TestFederationEnrollmentFenceDoesNotDeadlockAgainstBindingFirstWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "fence_lock_order_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "fence-lock-order-hub")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleHub,
		HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	spokeInstanceUID, err := uid.New()
	require.NoError(t, err)
	created, err := store.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token: "fence-lock-order-secret", SpokeInstanceUID: spokeInstanceUID,
		ProjectID: &project.ID, Capabilities: "pull", Actor: "member",
	})
	require.NoError(t, err)
	admitted, err := store.AuthorizeFederationToken(ctx, created.Token, project.ID, "pull")
	require.NoError(t, err)
	fence := store.FederationEnrollmentTransactionFence(admitted, project.ID, "pull")

	// Transaction A models the ingest writer: it locks the project binding
	// first and only then updates the enrollment.
	bindingFirst, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = bindingFirst.Rollback() }()
	_, err = bindingFirst.ExecContext(ctx,
		`SELECT 1 FROM federation_bindings WHERE project_id=$1 FOR UPDATE`, project.ID)
	require.NoError(t, err)

	fenceResult := make(chan error, 1)
	go func() {
		tx, beginErr := store.BeginTx(db.WithTransactionFence(ctx, fence), nil)
		if beginErr != nil {
			fenceResult <- beginErr
			return
		}
		_ = tx.Rollback()
		fenceResult <- nil
	}()

	// Let the fence acquire its first lock before A takes its second. A fence
	// that locks the binding first blocks here holding nothing the enrollment
	// update needs; a fence that locks the enrollment first sets up the cycle.
	select {
	case fenceErr := <-fenceResult:
		require.NoError(t, fenceErr)
		require.NoError(t, bindingFirst.Commit())
		return
	case <-time.After(300 * time.Millisecond):
	}

	_, updateErr := bindingFirst.ExecContext(ctx,
		`UPDATE federation_enrollments SET updated_at=updated_at WHERE id=$1`,
		created.Enrollment.ID)
	if updateErr == nil {
		updateErr = bindingFirst.Commit()
	}
	fenceErr := <-fenceResult

	assert.NoError(t, updateErr,
		"binding-first writer must not deadlock against the enrollment fence")
	assert.NoError(t, fenceErr,
		"enrollment fence must not deadlock against a binding-first writer")
}
