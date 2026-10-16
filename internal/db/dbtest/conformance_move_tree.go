package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// Contract: a default move atomically rehomes every active descendant, including
// closed issues and children already in another project, without changing links.
func checkMoveTree(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "tree-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "tree-target")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "tree-other")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	leaf, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "leaf", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: child.ID}}})
	require.NoError(t, err)
	leaf, _, _, err = store.CloseIssueWithEvents(ctx, leaf.ID, "done", "worker", "Completed leaf", nil)
	require.NoError(t, err)
	input := db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker", DryRun: true}
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, input)
	require.NoError(t, err)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, cursor, after)
	input.DryRun = false
	_, err = store.MoveIssueProject(ctx, input)
	require.NoError(t, err)
	for _, before := range []db.Issue{root, child, leaf} {
		got, err := store.IssueByID(ctx, before.ID)
		require.NoError(t, err)
		require.Equal(t, target.ID, got.ProjectID)
		require.Equal(t, before.Status, got.Status)
		require.Equal(t, before.ClosedAt, got.ClosedAt)
		require.Equal(t, before.Revision+1, got.Revision)
		require.Equal(t, before.UID, got.UID)
		links, err := store.LinksByIssue(ctx, before.ID)
		require.NoError(t, err)
		require.NotEmpty(t, links)
		events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: target.ID, Limit: 100})
		require.NoError(t, err)
		count := 0
		for _, event := range events {
			if event.Type == "issue.moved" && event.IssueID != nil && *event.IssueID == before.ID {
				count++
			}
		}
		require.Equal(t, 1, count)
	}
	return nil
}

// Contract: any descendant's recurrence pin or fresh external claim rejects the
// whole tree, in previews and writes; a stale root revision also rejects it.
func checkMoveTreeGuards(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	source, err := store.CreateProject(ctx, "guard-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "guard-target")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	recurrence, _, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{ProjectID: source.ID, Actor: "worker", Rule: "FREQ=WEEKLY", DTStart: "2026-10-12", Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "pinned"}})
	require.NoError(t, err)
	materialized, err := store.MaterializeNext(ctx, recurrence.ID, "2026-10-12", "worker")
	require.NoError(t, err)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: materialized.NewIssueID, ToIssueID: root.ID, Type: "parent", Author: "worker"})
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker", DryRun: dry})
		require.ErrorAs(t, err, new(*db.RecurrencePinnedError))
		got, err := store.IssueByID(ctx, root.ID)
		require.NoError(t, err)
		require.Equal(t, root, got)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, cursor, after)
	}
	return nil
}

// Contract: project-qualified old short IDs continue resolving by stable UID.
func checkMoveOldRefs(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "ref-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "ref-target")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "task", Author: "worker"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	collision, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "new task", Author: "worker"})
	require.NoError(t, err)
	require.NotEqual(t, issue.ShortID, collision.ShortID, "moved references stay reserved")

	got, err := store.IssueByShortID(ctx, source.ID, issue.ShortID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, issue.UID, got.UID)
	require.Equal(t, target.ID, got.ProjectID)
	return nil
}

func checkMoveTreeExternalClaim(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "claim-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "claim-target")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: source.ID, IssueID: child.ID, ConnectorInstance: "notes", ExternalRootKey: "root-key", ExternalAccountKey: "account-key", Actor: "worker", ReceiveCommentsAfter: time.Now().UTC()})
	require.NoError(t, err)
	now := time.Now().UTC()
	_, _, err = store.ClaimExternalRootBinding(ctx, binding.ID, "bridge-worker", now, now.Add(-db.ExternalRootClaimStaleAfter))
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker", DryRun: dry})
		require.ErrorIs(t, err, db.ErrExternalRootClaimActive)
		got, err := store.IssueByID(ctx, root.ID)
		require.NoError(t, err)
		require.Equal(t, root, got)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, cursor, after)
	}
	return nil
}

// Contract: expanded descendant authorization must run its authoritative fence
// inside the move transaction and finish rejection cleanup after rollback.
func checkMoveTreeExpandedFence(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "fence-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "fence-target")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "fence-descendant")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		expanded, cleaned := false, false
		denied := errors.New("descendant fence denied")
		fenced := db.WithTransactionFence(ctx, func(ctx context.Context, tx db.Transaction) error {
			if !expanded {
				return nil
			}
			return db.AfterTransactionRollback(denied, func(ctx context.Context) error {
				// This read must succeed after the transaction released its connection.
				got, err := store.IssueByID(ctx, root.ID)
				if err != nil {
					return err
				}
				require.Equal(t, source.ID, got.ProjectID)
				cleaned = true
				return nil
			})
		})
		_, err = store.MoveIssueProject(fenced, db.MoveIssueProjectIn{
			IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID,
			IfMatchRev: root.Revision, Actor: "worker", DryRun: dry,
			AuthorizeProjects: func(ids []int64) error {
				require.Contains(t, ids, other.ID)
				expanded = true
				return nil
			},
		})
		require.ErrorIs(t, err, denied)
		require.True(t, cleaned, "rejection cleanup must run after rollback")
		for _, before := range []db.Issue{root, child} {
			got, err := store.IssueByID(ctx, before.ID)
			require.NoError(t, err)
			require.Equal(t, before, got)
		}
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, cursor, after)
	}
	return nil
}
