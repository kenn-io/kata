package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkProjectRelocation(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	source, err := store.CreateProject(ctx, "move-source")
	if err != nil {
		return fmt.Errorf("create move source: %w", err)
	}
	target, err := store.CreateProject(ctx, "move-target")
	if err != nil {
		return fmt.Errorf("create move target: %w", err)
	}
	targetCollision, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: target.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "target collision", Author: "mover",
	})
	if err != nil {
		return fmt.Errorf("create target collision issue: %w", err)
	}
	moving, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "moving", Author: "mover",
	})
	if err != nil {
		return fmt.Errorf("create moving issue: %w", err)
	}
	assert.Equal(t, "d4ex", targetCollision.ShortID)
	assert.Equal(t, "d4ex", moving.ShortID)
	peer, err := createFixtureIssue(ctx, store, source.ID, "linked peer", "mover", nil)
	if err != nil {
		return err
	}
	link, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: moving.ID, ToIssueID: peer.ID, Type: "blocks", Author: "mover",
	})
	if err != nil {
		return fmt.Errorf("create move-surviving link: %w", err)
	}
	movingID := moving.ID
	targetID := targetCollision.ID
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "tracker", ExternalID: "collision", ObjectType: "issue", ProjectID: source.ID, IssueID: &movingID,
	})
	if err != nil {
		return fmt.Errorf("create source move mapping: %w", err)
	}
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "tracker", ExternalID: "collision", ObjectType: "issue", ProjectID: target.ID, IssueID: &targetID,
	})
	if err != nil {
		return fmt.Errorf("create target move mapping: %w", err)
	}
	now := time.Date(2026, 7, 15, 21, 0, 0, 0, time.UTC)
	claimPrincipal := db.ClaimPrincipal{
		HolderInstanceUID: store.InstanceUID(), Holder: "move-worker", ClientKind: "agent",
	}
	if _, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: source.ID, IssueRef: moving.UID, Principal: claimPrincipal,
		ClaimKind: "hard", Now: now,
	}); err != nil {
		return fmt.Errorf("acquire move-surviving claim: %w", err)
	}
	if _, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: source.ID, IssueRef: moving.UID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(), Holder: "queued-worker", ClientKind: "agent",
		},
		ClaimKind: "hard", Now: now,
	}); err != nil {
		return fmt.Errorf("enqueue move-surviving claim: %w", err)
	}

	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: moving.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: moving.Revision + 10, Actor: "mover",
	})
	var conflict *db.RevisionConflictError
	assert.ErrorAs(t, err, &conflict)
	unchanged, err := store.IssueByID(ctx, moving.ID)
	if err != nil {
		return fmt.Errorf("load issue after rejected move: %w", err)
	}
	assert.Equal(t, source.ID, unchanged.ProjectID)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: moving.ID, FromProjectID: source.ID, ToProjectID: source.ID,
		IfMatchRev: moving.Revision, Actor: "mover",
	})
	assert.Error(t, err)
	recurrence, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: source.ID, Actor: "mover", Rule: "FREQ=WEEKLY", DTStart: "2026-07-20", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "pinned move"},
	})
	if err != nil {
		return fmt.Errorf("create recurrence for move guard: %w", err)
	}
	materialized, err := store.MaterializeNext(ctx, recurrence.ID, "2026-07-20", "mover")
	if err != nil {
		return fmt.Errorf("materialize recurrence for move guard: %w", err)
	}
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: materialized.NewIssueID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: 1, Actor: "mover",
	})
	var pinned *db.RecurrencePinnedError
	assert.ErrorAs(t, err, &pinned)

	moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: moving.ID, FromProjectID: source.ID, ToProjectID: target.ID,
		IfMatchRev: moving.Revision, Actor: "mover",
	})
	if err != nil {
		return fmt.Errorf("move issue project: %w", err)
	}
	assert.Equal(t, target.ID, moved.Issue.ProjectID)
	assert.Equal(t, "xd4ex", moved.NewShortID)
	assert.Equal(t, moving.Revision+1, moved.NewRevision)
	assert.Equal(t, moved.NewShortID, moved.Issue.ShortID)
	assert.NotZero(t, moved.EventID)
	preservedLink, err := store.LinkByEndpoints(ctx, moving.ID, peer.ID, "blocks")
	if err != nil {
		return fmt.Errorf("load link after move: %w", err)
	}
	assert.Equal(t, link.ID, preservedLink.ID)
	targetMapping, err := store.ImportMappingBySource(ctx, target.ID, "tracker", "issue", "collision")
	if err != nil {
		return fmt.Errorf("load target collision mapping after move: %w", err)
	}
	assert.Equal(t, &targetID, targetMapping.IssueID)
	_, err = store.ImportMappingBySource(ctx, source.ID, "tracker", "issue", "collision")
	assert.ErrorIs(t, err, db.ErrNotFound)
	sourceLiveClaims, err := store.CountLiveClaims(ctx, source.ID)
	if err != nil {
		return err
	}
	targetLiveClaims, err := store.CountLiveClaims(ctx, target.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, sourceLiveClaims)
	assert.Equal(t, int64(1), targetLiveClaims)
	claimStatus, err := store.ClaimStatusReadOnly(ctx, target.ID, moving.UID, now.Add(time.Minute))
	if err != nil {
		return err
	}
	assert.True(t, claimStatus.Held)
	sourcePendingClaims, err := store.CountPendingClaims(ctx, source.ID)
	if err != nil {
		return err
	}
	targetPendingClaims, err := store.CountPendingClaims(ctx, target.ID)
	if err != nil {
		return err
	}
	assert.Zero(t, sourcePendingClaims)
	assert.Equal(t, int64(1), targetPendingClaims)
	pending, err := store.ListPendingClaimRequestsForIssue(ctx, target.ID, moving.UID, 10)
	if err != nil {
		return err
	}
	require.Len(t, pending, 1)
	assert.Equal(t, target.ID, pending[0].ProjectID)

	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: target.ID, Limit: 100})
	if err != nil {
		return fmt.Errorf("list target events after move: %w", err)
	}
	var moveEvent *db.Event
	for index := range events {
		if events[index].ID == moved.EventID {
			moveEvent = &events[index]
		}
	}
	require.NotNil(t, moveEvent)
	assert.Equal(t, "issue.moved", moveEvent.Type)
	assert.Equal(t, "mover", moveEvent.Actor)
	assert.Equal(t, target.ID, moveEvent.ProjectID)
	assert.Equal(t, target.UID, moveEvent.ProjectUID)
	assert.Equal(t, &moving.UID, moveEvent.IssueUID)
	var payload struct {
		IssueUID    string `json:"issue_uid"`
		FromProject string `json:"from_project_uid"`
		FromShortID string `json:"from_short_id"`
		ToProject   string `json:"to_project_uid"`
		ToShortID   string `json:"to_short_id"`
		UpdatedAt   string `json:"updated_at"`
	}
	require.NoError(t, json.Unmarshal([]byte(moveEvent.Payload), &payload))
	assert.Equal(t, moving.UID, payload.IssueUID)
	assert.Equal(t, source.UID, payload.FromProject)
	assert.Equal(t, moving.ShortID, payload.FromShortID)
	assert.Equal(t, target.UID, payload.ToProject)
	assert.Equal(t, moved.NewShortID, payload.ToShortID)
	assert.NotEmpty(t, payload.UpdatedAt)
	return nil
}

func checkProjectMerge(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	source, err := store.CreateProject(ctx, "merge-source")
	if err != nil {
		return fmt.Errorf("create merge source: %w", err)
	}
	target, err := store.CreateProject(ctx, "merge-target")
	if err != nil {
		return fmt.Errorf("create merge target: %w", err)
	}
	targetIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: target.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "target issue", Author: "merger",
	})
	if err != nil {
		return fmt.Errorf("create merge target issue: %w", err)
	}
	sourceIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "source issue", Author: "merger",
	})
	if err != nil {
		return fmt.Errorf("create merge source issue: %w", err)
	}
	sourcePeer, err := createFixtureIssue(ctx, store, source.ID, "source peer", "merger", nil)
	if err != nil {
		return err
	}
	purgedSource, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: source.ID, UID: "01HZNQ7VFPK2XGD8R5MABXD4EX", Title: "purged source", Author: "merger",
	})
	if err != nil {
		return err
	}
	assert.Equal(t, "xd4ex", purgedSource.ShortID)
	if _, err := store.PurgeIssue(ctx, purgedSource.ID, "merger", nil); err != nil {
		return fmt.Errorf("purge source issue before merge: %w", err)
	}
	recurrence, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: source.ID, Actor: "merger", Rule: "FREQ=WEEKLY", DTStart: "2026-07-20", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "merged recurrence"},
	})
	if err != nil {
		return fmt.Errorf("create source recurrence before merge: %w", err)
	}
	link, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: sourceIssue.ID, ToIssueID: sourcePeer.ID, Type: "blocks", Author: "merger",
	})
	if err != nil {
		return fmt.Errorf("create merge-surviving link: %w", err)
	}
	alias, err := store.AttachAlias(ctx, source.ID, "https://example.invalid/source.git", "git")
	if err != nil {
		return fmt.Errorf("attach source merge alias: %w", err)
	}
	sourceIssueID := sourceIssue.ID
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "tracker", ExternalID: "source-issue", ObjectType: "issue",
		ProjectID: source.ID, IssueID: &sourceIssueID,
	})
	if err != nil {
		return fmt.Errorf("create merge import mapping: %w", err)
	}

	mergedName := "merged-project"
	merged, err := store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: source.ID, TargetProjectID: target.ID, TargetName: &mergedName,
	})
	if err != nil {
		return fmt.Errorf("merge projects: %w", err)
	}
	assert.Equal(t, source.ID, merged.Source.ID)
	assert.Equal(t, target.ID, merged.Target.ID)
	assert.Equal(t, mergedName, merged.Target.Name)
	assert.Equal(t, int64(2), merged.IssuesMoved)
	assert.Equal(t, int64(1), merged.AliasesMoved)
	assert.Equal(t, int64(3), merged.EventsMoved)
	assert.Equal(t, int64(1), merged.PurgeLogsMoved)
	require.Len(t, merged.ShortIDExtensions, 1)
	assert.Equal(t, sourceIssue.UID, merged.ShortIDExtensions[0].UID)
	assert.Equal(t, "d4ex", merged.ShortIDExtensions[0].PreMergeShortID)
	assert.Equal(t, "bxd4ex", merged.ShortIDExtensions[0].PostMergeShortID)
	kept, err := store.IssueByShortID(ctx, target.ID, "d4ex", db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("load target issue after merge: %w", err)
	}
	assert.Equal(t, targetIssue.UID, kept.UID)
	moved, err := store.IssueByShortID(ctx, target.ID, "bxd4ex", db.IncludeDeletedNo)
	if err != nil {
		return fmt.Errorf("load extended source issue after merge: %w", err)
	}
	assert.Equal(t, sourceIssue.UID, moved.UID)
	preservedLinks, err := store.LinksByIssue(ctx, sourceIssue.ID)
	if err != nil {
		return fmt.Errorf("list links after project merge: %w", err)
	}
	require.Len(t, preservedLinks, 1)
	assert.Equal(t, link.ID, preservedLinks[0].ID)
	mapping, err := store.ImportMappingBySource(ctx, target.ID, "tracker", "issue", "source-issue")
	if err != nil {
		return fmt.Errorf("load moved merge mapping: %w", err)
	}
	assert.Equal(t, target.ID, mapping.ProjectID)
	aliases, err := store.ProjectAliases(ctx, target.ID)
	if err != nil {
		return fmt.Errorf("list aliases after project merge: %w", err)
	}
	require.Len(t, aliases, 1)
	assert.Equal(t, alias.ID, aliases[0].ID)
	assert.Equal(t, target.ID, aliases[0].ProjectID)
	recurrences, err := store.ListRecurrencesByProject(ctx, target.ID)
	if err != nil {
		return fmt.Errorf("list recurrences after project merge: %w", err)
	}
	require.Len(t, recurrences, 1)
	assert.Equal(t, recurrence.ID, recurrences[0].ID)
	assert.Equal(t, target.ID, recurrences[0].ProjectID)
	_, err = store.ProjectByID(ctx, source.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: target.ID, Limit: 100})
	if err != nil {
		return fmt.Errorf("list events after project merge: %w", err)
	}
	assert.Len(t, events, 4)
	for _, event := range events {
		if _, _, err := db.ValidateRemoteEventContentHash(remoteEventFromStored(event)); err != nil {
			return fmt.Errorf("validate merged event %s for federation: %w", event.UID, err)
		}
	}

	collisionSource, err := store.CreateProject(ctx, "merge-collision-source")
	if err != nil {
		return err
	}
	collisionTarget, err := store.CreateProject(ctx, "merge-collision-target")
	if err != nil {
		return err
	}
	collisionSourceIssue, err := createFixtureIssue(ctx, store, collisionSource.ID, "source mapping", "merger", nil)
	if err != nil {
		return err
	}
	collisionTargetIssue, err := createFixtureIssue(ctx, store, collisionTarget.ID, "target mapping", "merger", nil)
	if err != nil {
		return err
	}
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "tracker", ExternalID: "same", ObjectType: "issue",
		ProjectID: collisionSource.ID, IssueID: &collisionSourceIssue.ID,
	})
	if err != nil {
		return err
	}
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "tracker", ExternalID: "same", ObjectType: "issue",
		ProjectID: collisionTarget.ID, IssueID: &collisionTargetIssue.ID,
	})
	if err != nil {
		return err
	}
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: collisionSource.ID, TargetProjectID: collisionTarget.ID,
	})
	assert.ErrorIs(t, err, db.ErrProjectMergeImportMappingCollision)
	_, err = store.ProjectByID(ctx, collisionSource.ID)
	assert.NoError(t, err)

	archivedSource, err := store.CreateProject(ctx, "merge-archived-source")
	if err != nil {
		return err
	}
	archiveTarget, err := store.CreateProject(ctx, "merge-archive-target")
	if err != nil {
		return err
	}
	if _, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: archivedSource.ID, Actor: "merger"}); err != nil {
		return fmt.Errorf("archive merge source: %w", err)
	}
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: archivedSource.ID, TargetProjectID: archiveTarget.ID,
	})
	assert.ErrorIs(t, err, db.ErrProjectMergeArchivedSource)
	archivedTarget, err := store.CreateProject(ctx, "merge-archived-target")
	if err != nil {
		return err
	}
	liveSource, err := store.CreateProject(ctx, "merge-live-source")
	if err != nil {
		return err
	}
	if _, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: archivedTarget.ID, Actor: "merger"}); err != nil {
		return fmt.Errorf("archive merge target: %w", err)
	}
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: liveSource.ID, TargetProjectID: archivedTarget.ID,
	})
	assert.ErrorIs(t, err, db.ErrProjectMergeArchivedTarget)

	syncSource, err := store.CreateProject(ctx, "merge-sync-source")
	if err != nil {
		return err
	}
	syncTarget, err := store.CreateProject(ctx, "merge-sync-target")
	if err != nil {
		return err
	}
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID: syncSource.ID, Provider: "github", SourceKey: "github:merge/source",
		RemoteID: "merge/source", DisplayName: "merge/source", Config: json.RawMessage(`{}`), IntervalSeconds: 60,
	})
	if err != nil {
		return fmt.Errorf("create merge-blocking sync binding: %w", err)
	}
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: syncSource.ID, TargetProjectID: syncTarget.ID,
	})
	assert.ErrorIs(t, err, db.ErrProjectMergeIssueSyncBinding)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: syncTarget.ID, TargetProjectID: syncTarget.ID,
	})
	assert.ErrorIs(t, err, db.ErrProjectMergeSameProject)
	system, err := store.SystemProject(ctx)
	if err != nil {
		return fmt.Errorf("load system project for merge guards: %w", err)
	}
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: system.ID, TargetProjectID: syncTarget.ID,
	})
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.MergeProjects(ctx, db.MergeProjectsParams{
		SourceProjectID: syncTarget.ID, TargetProjectID: system.ID,
	})
	assert.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkActiveProjectionExports(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "export-project")
	if err != nil {
		return err
	}
	otherProject, err := store.CreateProject(ctx, "export-other")
	if err != nil {
		return err
	}
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "exported issue", Author: "exporter",
	})
	if err != nil {
		return err
	}
	deleted, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "deleted export issue", Author: "exporter",
	})
	if err != nil {
		return err
	}
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: otherProject.ID, Title: "outside export", Author: "exporter",
	})
	if err != nil {
		return err
	}
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "exporter", Body: "exported comment",
	})
	if err != nil {
		return err
	}
	if _, err := store.AddLabel(ctx, issue.ID, "exported", "exporter"); err != nil {
		return err
	}
	link, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: issue.ID, ToIssueID: deleted.ID, Type: "blocks", Author: "exporter",
	})
	if err != nil {
		return err
	}
	alias, err := store.AttachAlias(ctx, project.ID, "local:///export", "local")
	if err != nil {
		return err
	}
	recurrence, err := store.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: project.ID, Actor: "exporter", Rule: "FREQ=WEEKLY", DTStart: "2026-07-20",
		Timezone: "UTC", Template: db.RecurrenceTemplate{Title: "export recurrence"},
	})
	if err != nil {
		return err
	}
	issueID := issue.ID
	mapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "export", ExternalID: "issue-1", ObjectType: "issue",
		ProjectID: project.ID, IssueID: &issueID,
	})
	if err != nil {
		return err
	}
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID: project.ID, Provider: "github", SourceKey: "github:export/repo",
		RemoteID: "export/repo", DisplayName: "export/repo", Config: json.RawMessage(`{}`),
		IntervalSeconds: 60,
	})
	if err != nil {
		return err
	}
	if _, _, _, err := store.SoftDeleteIssue(ctx, deleted.ID, "exporter"); err != nil {
		return err
	}

	filter := db.ExportFilter{ProjectID: &project.ID}
	meta, err := collectExport(store.ExportMeta(ctx))
	if err != nil {
		return fmt.Errorf("export meta: %w", err)
	}
	assert.NotEmpty(t, meta)
	projects, err := collectExport(store.ExportProjects(ctx, filter))
	if err != nil {
		return fmt.Errorf("export projects: %w", err)
	}
	require.Len(t, projects, 1)
	assert.Equal(t, project.UID, projects[0].UID)
	issues, err := collectExport(store.ExportIssues(ctx, filter))
	if err != nil {
		return fmt.Errorf("export issues: %w", err)
	}
	require.Len(t, issues, 1)
	assert.Equal(t, issue.UID, issues[0].UID)
	comments, err := collectExport(store.ExportComments(ctx, filter))
	if err != nil {
		return fmt.Errorf("export comments: %w", err)
	}
	require.Len(t, comments, 1)
	assert.Equal(t, comment.UID, comments[0].UID)
	labels, err := collectExport(store.ExportIssueLabels(ctx, filter))
	if err != nil {
		return fmt.Errorf("export labels: %w", err)
	}
	require.Len(t, labels, 1)
	assert.Equal(t, "exported", labels[0].Label)
	links, err := collectExport(store.ExportLinks(ctx, filter))
	if err != nil {
		return fmt.Errorf("export links: %w", err)
	}
	assert.Empty(t, links)
	aliases, err := collectExport(store.ExportProjectAliases(ctx, filter))
	if err != nil {
		return fmt.Errorf("export aliases: %w", err)
	}
	require.Len(t, aliases, 1)
	assert.Equal(t, alias.ID, aliases[0].ID)
	recurrences, err := collectExport(store.ExportRecurrences(ctx, filter))
	if err != nil {
		return fmt.Errorf("export recurrences: %w", err)
	}
	require.Len(t, recurrences, 1)
	assert.Equal(t, recurrence.UID, recurrences[0].UID)
	mappings, err := collectExport(store.ExportImportMappings(ctx, filter))
	if err != nil {
		return fmt.Errorf("export mappings: %w", err)
	}
	require.Len(t, mappings, 1)
	assert.Equal(t, mapping.ID, mappings[0].ID)
	bindings, err := collectExport(store.ExportIssueSyncBindings(ctx, filter))
	if err != nil {
		return fmt.Errorf("export sync bindings: %w", err)
	}
	require.Len(t, bindings, 1)
	assert.Equal(t, binding.ID, bindings[0].ID)
	assert.True(t, bindings[0].Enabled)
	statuses, err := collectExport(store.ExportIssueSyncStatus(ctx, filter))
	if err != nil {
		return fmt.Errorf("export sync status: %w", err)
	}
	require.Len(t, statuses, 1)
	assert.Equal(t, binding.ID, statuses[0].BindingID)
	events, err := collectExport(store.ExportEvents(ctx, filter))
	if err != nil {
		return fmt.Errorf("export events: %w", err)
	}
	assert.NotEmpty(t, events)
	for _, event := range events {
		assert.Equal(t, project.ID, event.ProjectID)
		assert.NotEqual(t, &deleted.ID, event.IssueID)
		assert.NotEmpty(t, event.ContentHash)
	}

	withDeleted := db.ExportFilter{ProjectID: &project.ID, IncludeDeleted: true}
	issues, err = collectExport(store.ExportIssues(ctx, withDeleted))
	if err != nil {
		return err
	}
	assert.Len(t, issues, 2)
	links, err = collectExport(store.ExportLinks(ctx, withDeleted))
	if err != nil {
		return err
	}
	require.Len(t, links, 1)
	assert.Equal(t, link.ID, links[0].ID)
	return nil
}
