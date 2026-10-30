package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkMovedIssueAdoptionResets(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	unrelated, err := store.CreateProject(ctx, "unrelated-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Departed issue", Author: "worker"})
	require.NoError(t, err)
	beforeMove, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	sourceHistory, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, ThroughID: beforeMove, Limit: 100})
	require.NoError(t, err)
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: target.ID, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Actor: "worker"})
	require.NoError(t, err)
	reset, err := store.PurgeResetCheck(ctx, beforeMove, source.ID)
	require.NoError(t, err)
	require.Greater(t, reset, beforeMove, "former source clients must invalidate their cached issue after adoption deletes the departure")
	current, err := store.PurgeResetCheck(ctx, reset, source.ID)
	require.NoError(t, err)
	require.Zero(t, current)
	other, err := store.PurgeResetCheck(ctx, beforeMove, unrelated.ID)
	require.NoError(t, err)
	require.Zero(t, other, "unrelated streams keep their cursor")
	retainedHistory, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, ThroughID: beforeMove, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, sourceHistory, retainedHistory, "adoption must preserve source history and its content hashes")
	return nil
}
