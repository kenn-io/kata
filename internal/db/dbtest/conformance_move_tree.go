package dbtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
// whole tree, in previews and writes, and names the refusing descendant; a stale
// root revision also rejects it.
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
	pinned, err := store.IssueByID(ctx, materialized.NewIssueID)
	require.NoError(t, err)
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker", DryRun: dry})
		require.ErrorAs(t, err, new(*db.RecurrencePinnedError))
		descendant, ok := errors.AsType[*db.IssueMoveDescendantError](err)
		require.True(t, ok, "%v", err)
		require.Equal(t, db.IssueMoveDescendant{UID: pinned.UID, ShortID: pinned.ShortID, ProjectUID: source.UID, ProjectName: source.Name}, descendant.Descendant)
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

// Contract: a reused local reference never falls back to a moved issue after
// deletion or purge; including deleted issues still selects the local issue.
func checkMoveReusedOldRefs(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "reuse-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "reuse-target")
	require.NoError(t, err)
	moved, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "moved task", Author: "worker"})
	require.NoError(t, err)
	require.Equal(t, "d4ex", moved.ShortID)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: moved.Revision, Actor: "worker"})
	require.NoError(t, err)

	// Seed a reference reused under the previous allocator, which did not
	// reserve moved short IDs. The current allocator would extend this ID.
	replacement, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", ShortIDOverride: "d4ex", Title: "replacement task", Author: "worker"})
	require.NoError(t, err)
	got, err := store.IssueByShortID(ctx, source.ID, "d4ex", db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, replacement.UID, got.UID)

	_, _, _, err = store.SoftDeleteIssue(ctx, replacement.ID, "worker")
	require.NoError(t, err)
	_, err = store.IssueByShortID(ctx, source.ID, "d4ex", db.IncludeDeletedNo)
	assert.ErrorIs(t, err, db.ErrNotFound, "a deleted local issue must block historical fallback")
	got, err = store.IssueByShortID(ctx, source.ID, "d4ex", db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Equal(t, replacement.UID, got.UID)
	require.NotNil(t, got.DeletedAt)
	_, _, changed, err := store.RestoreIssue(ctx, got.ID, "worker")
	require.NoError(t, err)
	require.True(t, changed)
	got, err = store.IssueByShortID(ctx, source.ID, "d4ex", db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, replacement.UID, got.UID)
	require.Nil(t, got.DeletedAt)

	_, err = store.PurgeIssue(ctx, replacement.ID, "worker", nil)
	require.NoError(t, err)
	for _, include := range []db.IncludeDeleted{db.IncludeDeletedNo, db.IncludeDeletedYes} {
		_, err = store.IssueByShortID(ctx, source.ID, "d4ex", include)
		assert.ErrorIs(t, err, db.ErrNotFound, "a purge tombstone must block historical fallback (include deleted: %v)", include)
	}
	got, err = store.IssueByUID(ctx, moved.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, target.ID, got.ProjectID)
	got, err = store.IssueByShortID(ctx, target.ID, "d4ex", db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, moved.UID, got.UID)
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
		fenced := db.WithTransactionFence(ctx, func(_ context.Context, _ db.Transaction) error {
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
			AuthorizeProjects: func(uids []string) error {
				require.ElementsMatch(t, []string{source.UID, other.UID}, uids)
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

// Contract: preview and execution reject mapping keys that would collide when
// descendants from different source projects arrive in the same destination.
func checkMoveTreeMappingCollision(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "mapping-source")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "mapping-other")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "mapping-target")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: other.ID, Title: "child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	for _, issue := range []db.Issue{root, child} {
		_, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{
			ProjectID: issue.ProjectID, Source: "connector", ExternalID: "same-task", ObjectType: "issue", IssueID: &issue.ID,
		})
		require.NoError(t, err)
	}
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker", DryRun: dry,
		})
		var collision *db.ProjectMergeImportMappingCollisionError
		require.ErrorAs(t, err, &collision)
		if collision == nil {
			t.Fatal("missing import mapping collision detail")
		}
		require.Equal(t, []db.ProjectMergeImportMappingCollision{{Source: "connector", ExternalID: "same-task", ObjectType: "issue"}}, collision.Mappings)
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

// Contract: merges must reserve both an old short ID and longer replacement
// candidates belonging to issues that have moved out of the target project.
func checkMergeMoveReservations(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	target, err := store.CreateProject(ctx, "reservation-target")
	require.NoError(t, err)
	away, err := store.CreateProject(ctx, "reservation-away")
	require.NoError(t, err)
	source, err := store.CreateProject(ctx, "reservation-source")
	require.NoError(t, err)
	var historical [2]db.Issue
	for index, uid := range []string{"01HZNQ7VFPK1XGD8R5MABXD4EX", "01HZNQ7VFPK1XGD8R5MCDXD4EX"} {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: target.ID, UID: uid, Title: "moved task", Author: "worker"})
		require.NoError(t, err)
		historical[index] = issue
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: target.ID, ToProjectID: away.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
	}
	require.Equal(t, "d4ex", historical[0].ShortID)
	require.Equal(t, "xd4ex", historical[1].ShortID)
	incoming, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MEFXD4EX", Title: "incoming", Author: "worker"})
	require.NoError(t, err)
	require.Equal(t, "d4ex", incoming.ShortID)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{SourceProjectID: source.ID, TargetProjectID: target.ID, Actor: "worker"})
	require.NoError(t, err)
	got, err := store.IssueByID(ctx, incoming.ID)
	require.NoError(t, err)
	require.Equal(t, "fxd4ex", got.ShortID)
	for _, before := range historical {
		resolved, err := store.IssueByShortID(ctx, target.ID, before.ShortID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, before.UID, resolved.UID)
	}
	return nil
}

// Contract: a soft-deleted descendant ends the subtree. Its active children stay
// with it, so restoring it never splits its subtree across projects.
func checkMoveTreeStopsAtDeletedIssue(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "deleted-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "deleted-target")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	deleted, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "deleted child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	grandchild, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "grandchild", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: deleted.ID}}})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, deleted.ID, "worker")
	require.NoError(t, err)
	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker"})
	require.NoError(t, err)
	require.Len(t, moved.Issues, 1)
	for _, issue := range []db.Issue{deleted, grandchild} {
		got, err := store.IssueByID(ctx, issue.ID)
		require.NoError(t, err)
		require.Equal(t, source.ID, got.ProjectID)
	}
	return nil
}

// Contract: a descendant in an archived project refuses the whole tree and
// names that descendant, in previews and writes.
func checkMoveTreeArchivedDescendant(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "archived-source")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "archived-target")
	require.NoError(t, err)
	archived, err := store.CreateProject(ctx, "archived-other")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "root", Author: "worker"})
	require.NoError(t, err)
	child, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: archived.ID, Title: "archived child", Author: "worker", Links: []db.InitialLink{{Type: "parent", ToNumber: root.ID}}})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: archived.ID, Actor: "worker", Force: true})
	require.NoError(t, err)
	for _, dry := range []bool{true, false} {
		_, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: root.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: root.Revision, Actor: "worker", DryRun: dry})
		require.ErrorIs(t, err, db.ErrIssueMoveDescendantArchived)
		descendant, ok := errors.AsType[*db.IssueMoveDescendantError](err)
		require.True(t, ok, "%v", err)
		require.Equal(t, db.IssueMoveDescendant{UID: child.UID, ShortID: child.ShortID, ProjectUID: archived.UID, ProjectName: archived.Name}, descendant.Descendant)
		got, err := store.IssueByID(ctx, root.ID)
		require.NoError(t, err)
		require.Equal(t, source.ID, got.ProjectID)
	}
	return nil
}

// Contract: a close rechecks resolved duplicate targets in its transaction. A
// target outside the request's projects or since deleted rolls the close back.
func checkCloseRechecksEvidenceTargets(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "evidence-source")
	require.NoError(t, err)
	excluded, err := store.CreateProject(ctx, "evidence-excluded")
	require.NoError(t, err)
	closing, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "closing", Author: "worker"})
	require.NoError(t, err)
	hidden, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: excluded.ID, Title: "hidden original", Author: "worker"})
	require.NoError(t, err)
	deleted, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "deleted original", Author: "worker"})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(ctx, deleted.ID, "worker")
	require.NoError(t, err)
	denied := errors.New("project denied")
	scoped := db.WithProjectAccessTransactionCheck(ctx, func(_ context.Context, _ db.Transaction, projectUID string) error {
		if projectUID == excluded.UID {
			return denied
		}
		return nil
	})
	cursor, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	for target, expected := range map[string]error{hidden.UID: denied, deleted.UID: db.ErrEvidenceTargetUnavailable} {
		_, _, _, err := store.CloseIssueGuarded(scoped, db.CloseIssueParams{IssueID: closing.ID, Reason: "duplicate", Actor: "worker", Message: "Same report.", Evidence: []db.Evidence{{Type: "duplicate-of", IssueRef: target}}})
		require.ErrorIs(t, err, expected)
		got, err := store.IssueByID(ctx, closing.ID)
		require.NoError(t, err)
		require.Equal(t, "open", got.Status)
		after, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		require.Equal(t, cursor, after)
	}
	return nil
}
