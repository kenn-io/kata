package dbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkIssueSyncLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "issue-sync-project")
	if err != nil {
		return fmt.Errorf("create issue sync project: %w", err)
	}
	params := db.UpsertIssueSyncBindingParams{
		ProjectID: project.ID, Provider: "github", SourceKey: "github:owner/repo", RemoteID: "owner/repo",
		DisplayName: "owner/repo", Config: json.RawMessage(`{"labels":["bug"]}`), IntervalSeconds: 60,
	}
	binding, err := store.UpsertIssueSyncBinding(ctx, params)
	if err != nil {
		return fmt.Errorf("upsert issue sync binding: %w", err)
	}
	assert.True(t, binding.Enabled)
	assert.Equal(t, params.ProjectID, binding.ProjectID)
	assert.Equal(t, params.Provider, binding.Provider)
	assert.Equal(t, params.SourceKey, binding.SourceKey)
	assert.Equal(t, params.RemoteID, binding.RemoteID)
	assert.JSONEq(t, string(params.Config), string(binding.Config))
	byProject, err := store.IssueSyncBindingByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("issue sync binding by project: %w", err)
	}
	assert.Equal(t, binding.ID, byProject.ID)
	byID, err := store.IssueSyncBindingByID(ctx, binding.ID)
	if err != nil {
		return fmt.Errorf("issue sync binding by ID: %w", err)
	}
	assert.Equal(t, binding.ProjectID, byID.ProjectID)
	status, err := store.IssueSyncStatusByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("initial issue sync status: %w", err)
	}
	assert.Equal(t, binding.ID, status.BindingID)
	assert.Nil(t, status.SyncStartedAt)

	started := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	due, err := store.ListDueIssueSyncBindings(ctx, "github", started, started.Add(-time.Hour), 10)
	if err != nil {
		return fmt.Errorf("list initially due issue sync binding: %w", err)
	}
	require.Len(t, due, 1)
	assert.Equal(t, binding.ID, due[0].ID)
	claimedBinding, claimed, err := store.ClaimIssueSyncBinding(
		ctx, binding.ID, "github", started, started.Add(-time.Hour),
	)
	if err != nil {
		return fmt.Errorf("claim issue sync binding: %w", err)
	}
	assert.True(t, claimed)
	assert.Equal(t, binding.ID, claimedBinding.ID)
	_, claimed, err = store.ClaimIssueSyncBinding(
		ctx, binding.ID, "github", started.Add(time.Minute), started.Add(-time.Hour),
	)
	if err != nil {
		return fmt.Errorf("repeat issue sync claim: %w", err)
	}
	assert.False(t, claimed)
	status, err = store.IssueSyncStatusByProject(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("claimed issue sync status: %w", err)
	}
	assert.Equal(t, &started, status.SyncStartedAt)
	assert.Equal(t, &started, status.LastAttemptAt)

	_, err = store.RecordIssueSyncError(ctx, db.IssueSyncErrorParams{
		BindingID: binding.ID, StartedAt: started.Add(time.Second), At: started.Add(time.Minute), Error: "stale",
	})
	assert.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	failedAt := started.Add(2 * time.Minute)
	status, err = store.RecordIssueSyncError(ctx, db.IssueSyncErrorParams{
		BindingID: binding.ID, StartedAt: started, At: failedAt, Error: "rate limited",
	})
	if err != nil {
		return fmt.Errorf("record issue sync error: %w", err)
	}
	assert.Nil(t, status.SyncStartedAt)
	assert.Equal(t, &failedAt, status.LastErrorAt)
	assert.Equal(t, "rate limited", status.LastError)
	due, err = store.ListDueIssueSyncBindings(ctx, "github", started.Add(30*time.Second), started.Add(-time.Hour), 10)
	if err != nil {
		return fmt.Errorf("list issue sync binding before interval: %w", err)
	}
	assert.Empty(t, due)
	due, err = store.ListDueIssueSyncBindings(ctx, "github", started.Add(61*time.Second), started.Add(-time.Hour), 10)
	if err != nil {
		return fmt.Errorf("list issue sync binding after interval: %w", err)
	}
	require.Len(t, due, 1)

	secondStarted := started.Add(2 * time.Hour)
	_, claimed, err = store.ClaimIssueSyncBinding(
		ctx, binding.ID, "github", secondStarted, secondStarted.Add(-time.Hour),
	)
	if err != nil {
		return fmt.Errorf("claim issue sync binding again: %w", err)
	}
	assert.True(t, claimed)
	succeededAt := secondStarted.Add(time.Minute)
	cursorAt := succeededAt.Add(-time.Minute)
	status, err = store.RecordIssueSyncSuccess(ctx, db.IssueSyncSuccessParams{
		BindingID: binding.ID, StartedAt: secondStarted, At: succeededAt, CursorAt: cursorAt,
		LastCreated: 2, LastUpdated: 3, LastUnchanged: 4, LastComments: 5,
	})
	if err != nil {
		return fmt.Errorf("record issue sync success: %w", err)
	}
	assert.Nil(t, status.SyncStartedAt)
	assert.Equal(t, &succeededAt, status.LastSuccessAt)
	assert.Nil(t, status.LastErrorAt)
	assert.Empty(t, status.LastError)
	assert.Equal(t, 2, status.LastCreated)
	assert.Equal(t, 3, status.LastUpdated)
	assert.Equal(t, 4, status.LastUnchanged)
	assert.Equal(t, 5, status.LastComments)
	byID, err = store.IssueSyncBindingByID(ctx, binding.ID)
	if err != nil {
		return fmt.Errorf("binding after issue sync success: %w", err)
	}
	assert.Equal(t, &cursorAt, byID.LastCursorAt)

	refreshed, err := store.RefreshIssueSyncBinding(ctx, db.IssueSyncBindingUpdateParams{
		BindingID: binding.ID, DisplayName: "owner/repo renamed", Config: json.RawMessage(`{"state":"open"}`),
	})
	if err != nil {
		return fmt.Errorf("refresh issue sync binding: %w", err)
	}
	assert.Equal(t, "owner/repo renamed", refreshed.DisplayName)
	assert.JSONEq(t, `{"state":"open"}`, string(refreshed.Config))
	assert.Equal(t, params.RemoteID, refreshed.RemoteID)

	disabled, err := store.DisableIssueSyncBinding(ctx, project.ID)
	if err != nil {
		return fmt.Errorf("disable issue sync binding: %w", err)
	}
	assert.False(t, disabled.Enabled)
	_, claimed, err = store.ClaimIssueSyncBinding(
		ctx, binding.ID, "github", secondStarted.Add(time.Hour), secondStarted,
	)
	if err != nil {
		return fmt.Errorf("claim disabled issue sync binding: %w", err)
	}
	assert.False(t, claimed)

	params.DisplayName = "owner/repo enabled"
	params.Config = json.RawMessage(`{"labels":["enhancement"]}`)
	reenabled, err := store.UpsertIssueSyncBinding(ctx, params)
	if err != nil {
		return fmt.Errorf("re-enable issue sync binding: %w", err)
	}
	assert.True(t, reenabled.Enabled)
	assert.Nil(t, reenabled.LastCursorAt, "config changes reset the import cursor")
	conflicting := params
	conflicting.RemoteID = "owner/other"
	conflicting.SourceKey = "github:owner/other"
	_, err = store.UpsertIssueSyncBinding(ctx, conflicting)
	assert.ErrorIs(t, err, db.ErrIssueSyncProjectAlreadyBound)
	_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{ProjectID: project.ID})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	return nil
}

func checkMetadataAndAtomicEdit(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	fixture, err := createIssueFixture(ctx, store, "metadata-atomic", "original", "author", nil)
	if err != nil {
		return err
	}

	issuePatch := map[string]json.RawMessage{
		"scheduled_on": json.RawMessage(`"2026-07-20"`),
		"custom":       json.RawMessage(`{"enabled":true}`),
	}
	patchedIssue, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: fixture.Issue.ID, IfMatchRev: db.IfMatch(fixture.Issue.Revision), Actor: "metadata-editor",
		Patch: issuePatch,
	})
	if err != nil {
		return fmt.Errorf("patch issue metadata: %w", err)
	}
	assert.True(t, patchedIssue.Changed)
	assert.Equal(t, fixture.Issue.Revision+1, patchedIssue.NewRevision)
	assert.Equal(t, "issue.metadata_updated", patchedIssue.Event.Type)
	assert.Equal(t, "metadata-editor", patchedIssue.Event.Actor)
	assert.JSONEq(t, `{"scheduled_on":"2026-07-20","custom":{"enabled":true}}`, string(patchedIssue.Issue.Metadata))
	var issuePayload struct {
		Diff map[string]struct {
			From json.RawMessage `json:"from"`
			To   json.RawMessage `json:"to"`
		} `json:"diff"`
		RevisionNew int64  `json:"revision_new"`
		UpdatedAt   string `json:"updated_at"`
	}
	require.NoError(t, json.Unmarshal([]byte(patchedIssue.Event.Payload), &issuePayload))
	assert.Equal(t, patchedIssue.NewRevision, issuePayload.RevisionNew)
	assert.NotEmpty(t, issuePayload.UpdatedAt)
	assert.JSONEq(t, `null`, string(issuePayload.Diff["scheduled_on"].From))
	assert.JSONEq(t, `"2026-07-20"`, string(issuePayload.Diff["scheduled_on"].To))

	noIssueChange, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: fixture.Issue.ID, IfMatchRev: db.IfMatch(patchedIssue.NewRevision), Actor: "metadata-editor",
		Patch: issuePatch,
	})
	if err != nil {
		return fmt.Errorf("repeat issue metadata patch: %w", err)
	}
	assert.False(t, noIssueChange.Changed)
	assert.Zero(t, noIssueChange.Event.ID)
	assert.Equal(t, patchedIssue.NewRevision, noIssueChange.NewRevision)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: fixture.Issue.ID, IfMatchRev: db.IfMatch(fixture.Issue.Revision), Actor: "metadata-editor",
		Patch: map[string]json.RawMessage{"custom": json.RawMessage(`null`)},
	})
	var issueConflict *db.RevisionConflictError
	assert.ErrorAs(t, err, &issueConflict)
	if issueConflict != nil {
		assert.Equal(t, patchedIssue.NewRevision, issueConflict.CurrentRevision)
	}
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: fixture.Issue.ID, Actor: "metadata-editor",
		Patch: map[string]json.RawMessage{"scheduled_on": json.RawMessage(`42`)},
	})
	assert.Error(t, err)
	clearedIssue, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: fixture.Issue.ID, Actor: "metadata-editor",
		Patch: map[string]json.RawMessage{"custom": json.RawMessage(`null`)},
	})
	if err != nil {
		return fmt.Errorf("clear issue metadata key: %w", err)
	}
	assert.True(t, clearedIssue.Changed)
	assert.JSONEq(t, `{"scheduled_on":"2026-07-20"}`, string(clearedIssue.Issue.Metadata))

	patchedProject, err := store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: fixture.Project.ID, IfMatchRev: db.IfMatch(fixture.Project.Revision), Actor: "metadata-editor",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"Platform"`)},
	})
	if err != nil {
		return fmt.Errorf("patch project metadata: %w", err)
	}
	assert.True(t, patchedProject.Changed)
	assert.Equal(t, fixture.Project.Revision+1, patchedProject.NewRevision)
	assert.Equal(t, "project.metadata_updated", patchedProject.Event.Type)
	assert.JSONEq(t, `{"area":"Platform"}`, string(patchedProject.Project.Metadata))
	noProjectChange, err := store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: fixture.Project.ID, IfMatchRev: db.IfMatch(patchedProject.NewRevision), Actor: "metadata-editor",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"Platform"`)},
	})
	if err != nil {
		return fmt.Errorf("repeat project metadata patch: %w", err)
	}
	assert.False(t, noProjectChange.Changed)
	assert.Zero(t, noProjectChange.Event.ID)
	_, err = store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: fixture.Project.ID, IfMatchRev: db.IfMatch(fixture.Project.Revision), Actor: "metadata-editor",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"Other"`)},
	})
	var projectConflict *db.RevisionConflictError
	assert.ErrorAs(t, err, &projectConflict)

	blocked, err := createFixtureIssue(ctx, store, fixture.Project.ID, "blocked", "author", nil)
	if err != nil {
		return err
	}
	related, err := createFixtureIssue(ctx, store, fixture.Project.ID, "related", "author", nil)
	if err != nil {
		return err
	}
	title := "atomically edited"
	owner := "alice"
	priority := int64(2)
	contentBefore, err := store.ListIssueContent(ctx, fixture.Project.ID, 100)
	if err != nil {
		return fmt.Errorf("list issue content before atomic edit: %w", err)
	}
	var originalContentRevision int64
	for _, content := range contentBefore {
		if content.ID == fixture.Issue.ID {
			originalContentRevision = content.ContentRevision
		}
	}
	atomicResult, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: fixture.Issue.ID, Actor: "atomic-editor", Title: &title, Owner: &owner,
		SetPriority: &priority, AddBlocks: []int64{blocked.ID}, AddRelated: []int64{related.ID},
	})
	if err != nil {
		return fmt.Errorf("atomic edit: %w", err)
	}
	assert.True(t, atomicResult.AnyChange)
	assert.Equal(t, title, atomicResult.Issue.Title)
	assert.Equal(t, &owner, atomicResult.Issue.Owner)
	assert.Equal(t, &priority, atomicResult.Issue.Priority)
	contentAfter, err := store.ListIssueContent(ctx, fixture.Project.ID, 100)
	if err != nil {
		return fmt.Errorf("list issue content after atomic edit: %w", err)
	}
	for _, content := range contentAfter {
		if content.ID == fixture.Issue.ID {
			assert.Equal(t, originalContentRevision+1, content.ContentRevision)
		}
	}
	require.Len(t, atomicResult.Events, 3)
	assert.Equal(t, []string{"issue.updated", "issue.priority_set", "issue.links_changed"}, []string{
		atomicResult.Events[0].Type, atomicResult.Events[1].Type, atomicResult.Events[2].Type,
	})
	require.Len(t, atomicResult.Changes.BlocksAdded, 1)
	assert.Equal(t, blocked.UID, atomicResult.Changes.BlocksAdded[0].UID)
	require.Len(t, atomicResult.Changes.RelatedAdded, 1)
	assert.Equal(t, related.UID, atomicResult.Changes.RelatedAdded[0].UID)
	_, err = store.LinkByEndpoints(ctx, fixture.Issue.ID, blocked.ID, "blocks")
	assert.NoError(t, err)
	relatedFrom, relatedTo := fixture.Issue.ID, related.ID
	if relatedFrom > relatedTo {
		relatedFrom, relatedTo = relatedTo, relatedFrom
	}
	_, err = store.LinkByEndpoints(ctx, relatedFrom, relatedTo, "related")
	assert.NoError(t, err)
	parent, err := createFixtureIssue(ctx, store, fixture.Project.ID, "parent", "author", nil)
	if err != nil {
		return err
	}
	parentResult, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: fixture.Issue.ID, Actor: "atomic-editor", SetParent: &parent.ID,
	})
	if err != nil {
		return fmt.Errorf("set parent atomically: %w", err)
	}
	require.Len(t, parentResult.Events, 1)
	assert.Equal(t, "issue.links_changed", parentResult.Events[0].Type)
	assert.Equal(t, &parent.ID, parentResult.Events[0].RelatedIssueID)
	assert.Equal(t, &parent.UID, parentResult.Events[0].RelatedIssueUID)
	require.NotNil(t, parentResult.Changes.ParentSet)
	assert.Equal(t, parent.UID, parentResult.Changes.ParentSet.UID)
	_, err = store.LinkByEndpoints(ctx, fixture.Issue.ID, parent.ID, "parent")
	assert.NoError(t, err)

	wrongParent := parent.ID + 1_000_000
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: fixture.Issue.ID, Actor: "atomic-editor", RemoveParent: &wrongParent,
	})
	assert.ErrorIs(t, err, db.ErrParentMismatch)
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: parent.ID, Actor: "atomic-editor", SetParent: &fixture.Issue.ID,
	})
	assert.ErrorIs(t, err, db.ErrParentCycle)

	noAtomicChange, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: fixture.Issue.ID, Actor: "atomic-editor", Title: &title, Owner: &owner,
		SetPriority: &priority, AddBlocks: []int64{blocked.ID}, AddRelated: []int64{related.ID},
	})
	if err != nil {
		return fmt.Errorf("repeat atomic edit: %w", err)
	}
	assert.False(t, noAtomicChange.AnyChange)
	assert.Empty(t, noAtomicChange.Events)

	clearOwner := ""
	clearResult, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: fixture.Issue.ID, Actor: "atomic-editor", Owner: &clearOwner, ClearPriority: true,
		RemoveParent: &parent.ID, RemoveBlocks: []int64{blocked.ID}, RemoveRelated: []int64{related.ID},
		AddBlockedBy: []int64{parent.ID},
	})
	if err != nil {
		return fmt.Errorf("clear and unlink atomically: %w", err)
	}
	assert.Nil(t, clearResult.Issue.Owner)
	assert.Nil(t, clearResult.Issue.Priority)
	require.Len(t, clearResult.Events, 3)
	assert.Equal(t, []string{"issue.updated", "issue.priority_cleared", "issue.links_changed"}, []string{
		clearResult.Events[0].Type, clearResult.Events[1].Type, clearResult.Events[2].Type,
	})
	require.NotNil(t, clearResult.Changes.ParentRemoved)
	require.Len(t, clearResult.Changes.BlocksRemoved, 1)
	require.Len(t, clearResult.Changes.RelatedRemoved, 1)
	require.Len(t, clearResult.Changes.BlockedByAdded, 1)
	_, err = store.LinkByEndpoints(ctx, parent.ID, fixture.Issue.ID, "blocks")
	assert.NoError(t, err)
	removeBlockedBy, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: fixture.Issue.ID, Actor: "atomic-editor", RemoveBlockedBy: []int64{parent.ID},
	})
	if err != nil {
		return fmt.Errorf("remove blocked-by atomically: %w", err)
	}
	require.Len(t, removeBlockedBy.Changes.BlockedByRemoved, 1)

	type concurrentAtomicResult struct {
		result db.EditIssueAtomicResult
		err    error
	}
	start := make(chan struct{})
	concurrent := make(chan concurrentAtomicResult, 2)
	for range 2 {
		go func() {
			<-start
			result, err := store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
				IssueID: fixture.Issue.ID, Actor: "atomic-editor", AddBlocks: []int64{blocked.ID},
			})
			concurrent <- concurrentAtomicResult{result: result, err: err}
		}()
	}
	close(start)
	changedCalls := 0
	for range 2 {
		call := <-concurrent
		if call.err != nil {
			return fmt.Errorf("concurrent idempotent atomic edit: %w", call.err)
		}
		if call.result.AnyChange {
			changedCalls++
		}
	}
	assert.Equal(t, 1, changedCalls)
	_, err = store.LinkByEndpoints(ctx, fixture.Issue.ID, blocked.ID, "blocks")
	assert.NoError(t, err)

	rollbackTitle := "must roll back"
	_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
		IssueID: fixture.Issue.ID, Actor: "atomic-editor", Title: &rollbackTitle,
		AddBlocks: []int64{fixture.Issue.ID + 1_000_000},
	})
	var missingTarget *db.LinkTargetNotFoundError
	assert.ErrorAs(t, err, &missingTarget)
	afterRollback, err := store.IssueByID(ctx, fixture.Issue.ID)
	if err != nil {
		return fmt.Errorf("load issue after rolled-back atomic edit: %w", err)
	}
	assert.Equal(t, title, afterRollback.Title)

	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: fixture.Project.ID, Limit: 100})
	if err != nil {
		return fmt.Errorf("list metadata and atomic events: %w", err)
	}
	var metadataEvents, rolledBackEvents int
	for _, event := range events {
		if event.Type == "issue.metadata_updated" {
			metadataEvents++
		}
		if event.Type == "issue.updated" && event.Actor == "atomic-editor" && strings.Contains(event.Payload, rollbackTitle) {
			rolledBackEvents++
		}
	}
	assert.Equal(t, 2, metadataEvents)
	assert.Zero(t, rolledBackEvents)
	return nil
}

func checkImportMappings(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	fixture, err := createIssueFixture(ctx, store, "mapping-project", "mapped issue", "importer", nil)
	if err != nil {
		return err
	}
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: fixture.Issue.ID, Author: "importer", Body: "mapped comment",
	})
	if err != nil {
		return fmt.Errorf("create mapped comment: %w", err)
	}
	if _, err := store.AddLabel(ctx, fixture.Issue.ID, "external", "importer"); err != nil {
		return fmt.Errorf("create mapped label: %w", err)
	}
	peer, err := createFixtureIssue(ctx, store, fixture.Project.ID, "mapped peer", "importer", nil)
	if err != nil {
		return err
	}
	link, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: fixture.Issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "importer",
	})
	if err != nil {
		return fmt.Errorf("create mapped link: %w", err)
	}
	sourceUpdated := time.Date(2026, 7, 15, 18, 30, 0, 123_000_000, time.UTC)
	issueID := fixture.Issue.ID
	commentID := comment.ID
	linkID := link.ID
	label := "external"
	params := []db.ImportMappingParams{
		{Source: "tracker", ExternalID: "issue-1", ObjectType: "issue", ProjectID: fixture.Project.ID, IssueID: &issueID, SourceUpdatedAt: &sourceUpdated},
		{Source: "tracker", ExternalID: "comment-1", ObjectType: "comment", ProjectID: fixture.Project.ID, IssueID: &issueID, CommentID: &commentID},
		{Source: "tracker", ExternalID: "label-1", ObjectType: "label", ProjectID: fixture.Project.ID, IssueID: &issueID, Label: &label},
		{Source: "tracker", ExternalID: "link-1", ObjectType: "link", ProjectID: fixture.Project.ID, IssueID: &issueID, LinkID: &linkID},
	}
	for _, input := range params {
		mapping, err := store.UpsertImportMapping(ctx, input)
		if err != nil {
			return fmt.Errorf("upsert %s import mapping: %w", input.ObjectType, err)
		}
		assert.NotZero(t, mapping.ID)
		assert.Equal(t, input.Source, mapping.Source)
		assert.Equal(t, input.ExternalID, mapping.ExternalID)
		assert.Equal(t, input.ObjectType, mapping.ObjectType)
		assert.Equal(t, input.ProjectID, mapping.ProjectID)
		assert.False(t, mapping.ImportedAt.IsZero())
	}

	issueMapping, err := store.ImportMappingBySource(ctx, fixture.Project.ID, "tracker", "issue", "issue-1")
	if err != nil {
		return fmt.Errorf("lookup issue import mapping: %w", err)
	}
	assert.Equal(t, &issueID, issueMapping.IssueID)
	require.NotNil(t, issueMapping.SourceUpdatedAt)
	assert.Equal(t, sourceUpdated, issueMapping.SourceUpdatedAt.UTC())
	firstImportedAt := issueMapping.ImportedAt

	newerSource := sourceUpdated.Add(time.Hour)
	updatedMapping, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "tracker", ExternalID: "issue-1", ObjectType: "issue", ProjectID: fixture.Project.ID,
		IssueID: &issueID, SourceUpdatedAt: &newerSource,
	})
	if err != nil {
		return fmt.Errorf("refresh issue import mapping: %w", err)
	}
	assert.Equal(t, issueMapping.ID, updatedMapping.ID)
	require.NotNil(t, updatedMapping.SourceUpdatedAt)
	assert.Equal(t, newerSource, updatedMapping.SourceUpdatedAt.UTC())
	assert.False(t, updatedMapping.ImportedAt.Before(firstImportedAt))

	listed, err := store.ImportMappingsByProjectSource(ctx, fixture.Project.ID, "tracker")
	if err != nil {
		return fmt.Errorf("list import mappings: %w", err)
	}
	require.Len(t, listed, 4)
	assert.Equal(t, []string{"issue-1", "comment-1", "label-1", "link-1"}, []string{
		listed[0].ExternalID, listed[1].ExternalID, listed[2].ExternalID, listed[3].ExternalID,
	})
	empty, err := store.ImportMappingsByProjectSource(ctx, fixture.Project.ID, "another-source")
	if err != nil {
		return fmt.Errorf("list absent import source: %w", err)
	}
	assert.Empty(t, empty)
	_, err = store.ImportMappingBySource(ctx, fixture.Project.ID, "tracker", "issue", "missing")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "tracker", ExternalID: "invalid", ObjectType: "comment", ProjectID: fixture.Project.ID,
		IssueID: &issueID,
	})
	assert.Error(t, err)
	return nil
}
