package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssueScopedFenceRejectsParentRemovalWhileWaiting(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Root", Author: "coordinator"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Child", Author: "coordinator", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	token, _, err := store.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope:     &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID},
		ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
	})
	require.NoError(t, err)
	parent, err := store.ParentOf(ctx, child.ID)
	require.NoError(t, err)
	moving, err := store.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = moving.Rollback() }()
	_, err = moving.ExecContext(ctx, `DELETE FROM links WHERE id = $1`, parent.ID)
	require.NoError(t, err)
	fenced := db.WithIssueScopeTargets(ctx)
	db.RecordIssueScopeTarget(fenced, child.ID)
	fenced = db.WithTransactionFence(fenced, store.IssueScopedTokenTransactionFence(token))
	result := make(chan error, 1)
	go func() {
		_, _, err := store.CreateComment(fenced, db.CreateCommentParams{IssueID: child.ID, Body: "Must not land", Author: token.Actor})
		result <- err
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := store.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%locked_links AS%')`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, moving.Commit())
	require.ErrorIs(t, <-result, db.ErrNotFound)
	comments, err := store.CommentsByIssue(ctx, child.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}
