package dbtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkHistoricalCloseReceipts(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "receipt-source")
	if err != nil {
		return err
	}
	target, err := store.CreateProject(ctx, "receipt-target")
	if err != nil {
		return err
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Retained close receipt", Author: "agent"})
	if err != nil {
		return err
	}
	since := time.Now().Add(-time.Minute)
	closed, events, changed, err := store.CloseIssueGuarded(ctx, db.CloseIssueParams{IssueID: issue.ID, ExpectedProjectID: source.ID, Reason: "wontfix", Actor: "agent", Message: "Reviewed this work and recorded why it should stop here.", IdempotencyKey: "stable-close", IdempotencyFingerprint: "original-fingerprint"})
	if err != nil {
		return err
	}
	require.True(t, changed)
	require.NotEmpty(t, events)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: closed.Revision, Actor: "agent"})
	if err != nil {
		return err
	}
	_, _, _, err = store.SoftDeleteIssue(ctx, moved.Issue.ID, "agent")
	if err != nil {
		return err
	}
	match, err := store.LookupIssueMutationIdempotencyByIssue(ctx, issue.UID, "issue.closed", "stable-close", since)
	if err != nil {
		return err
	}
	require.NotNil(t, match)
	assert.Equal(t, issue.UID, match.IssueUID)
	assert.Equal(t, events[0].UID, match.Event.UID)
	assert.Equal(t, source.ID, match.Event.ProjectID)
	assert.Equal(t, "original-fingerprint", match.Fingerprint)
	for _, tc := range []struct {
		name, uid, typ, key string
		since               time.Time
	}{
		{"wrong issue", target.UID, "issue.closed", "stable-close", since},
		{"wrong type", issue.UID, "issue.created", "stable-close", since},
		{"wrong key", issue.UID, "issue.closed", "missing-close", since},
		{"expired", issue.UID, "issue.closed", "stable-close", time.Now().Add(time.Minute)},
	} {
		missing, err := store.LookupIssueMutationIdempotencyByIssue(ctx, tc.uid, tc.typ, tc.key, tc.since)
		if err != nil {
			return fmt.Errorf("%s: %w", tc.name, err)
		}
		assert.Nil(t, missing, tc.name)
	}
	return nil
}
