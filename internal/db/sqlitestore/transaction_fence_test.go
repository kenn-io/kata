package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssueScopedTokenTransactionFenceRejectsExpiryBeforeFirstWrite(t *testing.T) { //nolint:paralleltest // token expires 75ms after creation in real time
	d, ctx, project, _ := setupSoftDeletedIssue(t)
	root, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(75 * time.Millisecond)
	token, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "scoped-worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)

	time.Sleep(time.Until(expiresAt) + 20*time.Millisecond)
	fenced := db.WithTransactionFence(ctx, d.IssueScopedTokenTransactionFence(token))
	_, _, err = d.CreateIssue(fenced, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Too late", Author: token.Actor,
	})
	require.ErrorIs(t, err, db.ErrNotFound)
	issues, err := d.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID})
	require.NoError(t, err)
	for _, issue := range issues {
		assert.NotEqual(t, "Too late", issue.Title)
	}
}

func TestTransactionFenceSkipsExplicitReadOnlyTransactions(t *testing.T) {
	t.Parallel()
	d, ctx, _, _ := setupSoftDeletedIssue(t)
	var calls atomic.Int64
	fenced := db.WithTransactionFence(ctx, func(context.Context, db.Transaction) error {
		calls.Add(1)
		return errors.New("must not run for a read-only transaction")
	})

	tx, err := d.BeginTx(fenced, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	assert.Zero(t, calls.Load())
}

func TestTransactionFenceRollsBackAutocommitAndImmediateMutations(t *testing.T) {
	t.Parallel()
	d, ctx, _, issue := setupSoftDeletedIssue(t)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d.SetMaxOpenConns(1)
	_, err := d.ExecContext(ctx, `CREATE TABLE fence_markers (operation TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = d.AddLabel(ctx, issue.ID, "retained", "tester")
	require.NoError(t, err)

	rejected := errors.New("test transaction fence rejected")
	finishFailed := errors.New("host record unavailable")
	callbacks := 0
	fenced := db.WithTransactionFence(ctx, func(ctx context.Context, tx db.Transaction) error {
		_, insertErr := tx.ExecContext(ctx,
			`INSERT INTO fence_markers(operation) VALUES(?)`, "rejected")
		if insertErr != nil {
			return insertErr
		}
		return db.AfterTransactionRollback(rejected, func(ctx context.Context) error {
			callbacks++
			var markers int
			if err := d.QueryRowContext(ctx, `SELECT count(*) FROM fence_markers`).Scan(&markers); err != nil {
				return err
			}
			assert.Zero(t, markers, "rollback must finish before the callback takes the connection")
			return finishFailed
		})
	})
	fenced = db.WithAdditionalTransactionFence(fenced, func(context.Context, db.Transaction) error {
		t.Error("an earlier denial must skip the next fence")
		return nil
	})

	err = d.RemoveLabel(fenced, issue.ID, "retained")
	require.ErrorIs(t, err, rejected)
	require.ErrorIs(t, err, finishFailed)
	hasLabel, err := d.HasLabel(ctx, issue.ID, "retained")
	require.NoError(t, err)
	assert.True(t, hasLabel)

	_, err = d.PurgeIssue(fenced, issue.ID, "tester", nil)
	require.ErrorIs(t, err, rejected)
	require.ErrorIs(t, err, finishFailed)
	assert.Equal(t, 2, callbacks)
	retained, err := d.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, issue.ID, retained.ID)

	var markerCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM fence_markers`).Scan(&markerCount))
	assert.Zero(t, markerCount)
}

func TestLargeScopeAllowlistUsesBoundedSQLParameters(t *testing.T) {
	t.Parallel()
	store, ctx, project, _ := setupSoftDeletedIssue(t)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Needle", Author: "worker"})
	require.NoError(t, err)
	ids := make([]int64, 40000)
	for i := range ids {
		ids[i] = issue.ID
	}
	listed, err := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID, AllowedIssueIDs: ids})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	hits, err := store.SearchFTS(ctx, db.SearchFTSParams{ProjectID: project.ID, Query: "Needle", AllowedIssueIDs: ids})
	require.NoError(t, err)
	require.Len(t, hits, 1)
}
