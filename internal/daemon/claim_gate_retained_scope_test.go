package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testfix"
)

func TestRetainedIssueStateWithScopedMovedHistory(t *testing.T) {
	ctx := t.Context()
	store, err := testfix.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "offline task", Author: "old-actor"})
	require.NoError(t, err)
	move, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "old-actor"})
	require.NoError(t, err)
	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: target.UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	require.NoError(t, store.AdvanceFederationPushCursor(ctx, target.ID, move.Event.ID))
	binding.PushCursorEventID = move.Event.ID
	ctx = db.WithAuthorizedProjects(ctx, []string{target.UID})
	retained, _, _, err := retainedIssueState(ctx, store, binding, issue.UID)
	require.NoError(t, err)
	require.True(t, retained)
	ctx = db.WithAuthorizedProjects(ctx, []string{source.UID})
	_, _, _, err = retainedIssueState(ctx, store, binding, issue.UID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
