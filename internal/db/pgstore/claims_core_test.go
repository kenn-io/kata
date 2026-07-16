package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestClaimRetryClearsRolledBackExpiredOutcome(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "claim_retry_store", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "claim-retry")
	require.NoError(t, err)
	instanceUID, err := uid.New()
	require.NoError(t, err)
	principal := db.ClaimPrincipal{
		HolderInstanceUID: instanceUID, Holder: "claim-holder", ClientKind: "agent",
	}
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)

	for _, operation := range []string{"renew", "release"} {
		t.Run(operation, func(t *testing.T) {
			issue, _, createErr := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: operation + " retry", Author: "tester",
			})
			require.NoError(t, createErr)
			_, acquireErr := store.AcquireClaim(ctx, db.AcquireClaimParams{
				ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
				ClaimKind: "timed", TTL: time.Hour, Now: now,
			})
			require.NoError(t, acquireErr)
			futureExpiry := now.Add(4 * time.Hour)
			runner := rollbackThenRetry(t, store, func() {
				_, updateErr := store.ExecContext(ctx, `UPDATE issue_claims
SET expires_at=$1, updated_at=$2 WHERE issue_uid=$3 AND released_at IS NULL`,
					formatStoredTime(futureExpiry), formatStoredTime(now.Add(2*time.Hour)), issue.UID)
				require.NoError(t, updateErr)
			})

			var result db.LeaseResult
			if operation == "renew" {
				result, err = store.renewClaim(ctx, db.RenewClaimParams{
					ProjectID: project.ID, IssueRef: issue.UID, Principal: principal,
					TTL: time.Hour, Now: now.Add(2 * time.Hour),
				}, runner)
			} else {
				result, err = store.releaseClaim(ctx, project.ID, issue.UID, principal,
					principal.Holder, "complete", now.Add(2*time.Hour), false, runner)
			}
			require.NoError(t, err)
			assert.True(t, result.Granted)
			assert.Empty(t, result.Events, "rolled-back expiry events must not escape a successful retry")
			if operation == "release" {
				require.NotNil(t, result.Event)
				assert.Equal(t, "claim.released", result.Event.Type)
			}
		})
	}
}
