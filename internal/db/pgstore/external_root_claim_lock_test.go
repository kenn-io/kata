package pgstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestExternalRootCompletionClaimsWaitForIssueLock(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "external_root_claim_lock", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Closed external root", Author: "tester",
	})
	require.NoError(t, err)
	binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "notes",
		ExternalRootKey: "root-claim-lock", ExternalAccountKey: "opaque-account-key",
		Actor: "tester", ReceiveCommentsAfter: time.Now().UTC(),
	})
	require.NoError(t, err)
	_, _, _, err = store.CloseIssue(ctx, issue.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	now := time.Now().UTC().Add(time.Minute)
	claimers := []struct {
		name  string
		claim func(string) (db.ExternalRootBinding, bool, error)
	}{
		{
			name: "scheduled",
			claim: func(token string) (db.ExternalRootBinding, bool, error) {
				return store.ClaimExternalRootBinding(ctx, binding.ID, token, now, now.Add(-time.Minute))
			},
		},
		{
			name: "manual reconcile",
			claim: func(token string) (db.ExternalRootBinding, bool, error) {
				return store.ClaimExternalRootBindingForManualReconcile(
					ctx, binding.ID, token, now, now.Add(-time.Minute),
				)
			},
		},
	}
	for _, test := range claimers {
		t.Run(test.name, func(t *testing.T) {
			blocker, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback() }()
			var lockedID int64
			require.NoError(t, blocker.QueryRowContext(ctx,
				`SELECT id FROM issues WHERE id=$1 FOR UPDATE`, issue.ID,
			).Scan(&lockedID))
			require.Equal(t, issue.ID, lockedID)

			type claimResult struct {
				binding db.ExternalRootBinding
				claimed bool
				err     error
			}
			result := make(chan claimResult, 1)
			token := "claim-" + test.name
			go func() {
				claimedBinding, claimed, claimErr := test.claim(token)
				result <- claimResult{binding: claimedBinding, claimed: claimed, err: claimErr}
			}()

			select {
			case got := <-result:
				require.Failf(t, "claim bypassed issue lock", "result=%+v", got)
			case <-time.After(100 * time.Millisecond):
			}
			require.NoError(t, blocker.Rollback())
			select {
			case got := <-result:
				require.NoError(t, got.err)
				require.True(t, got.claimed)
				require.Equal(t, token, got.binding.ClaimToken)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "claim remained blocked after issue lock released")
			}
			_, err = store.ReleaseExternalRootClaim(ctx, binding.ID, token)
			require.NoError(t, err)
		})
	}
}
