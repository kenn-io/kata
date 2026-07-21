package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestTransactionFenceSkipsExplicitReadOnlyTransactions(t *testing.T) {
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
	d, ctx, _, issue := setupSoftDeletedIssue(t)
	_, err := d.ExecContext(ctx, `CREATE TABLE fence_markers (operation TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = d.AddLabel(ctx, issue.ID, "retained", "tester")
	require.NoError(t, err)

	rejected := errors.New("test transaction fence rejected")
	fenced := db.WithTransactionFence(ctx, func(ctx context.Context, tx db.Transaction) error {
		_, insertErr := tx.ExecContext(ctx,
			`INSERT INTO fence_markers(operation) VALUES(?)`, "rejected")
		if insertErr != nil {
			return insertErr
		}
		return rejected
	})

	err = d.RemoveLabel(fenced, issue.ID, "retained")
	require.ErrorIs(t, err, rejected)
	hasLabel, err := d.HasLabel(ctx, issue.ID, "retained")
	require.NoError(t, err)
	assert.True(t, hasLabel)

	_, err = d.PurgeIssue(fenced, issue.ID, "tester", nil)
	require.ErrorIs(t, err, rejected)
	retained, err := d.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, issue.ID, retained.ID)

	var markerCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT count(*) FROM fence_markers`).Scan(&markerCount))
	assert.Zero(t, markerCount)
}
