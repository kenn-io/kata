package dbtest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkExternalImportLifecycle(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "external-import-project")
	if err != nil {
		return err
	}
	older := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	closedReason := "done"
	priority := int64(2)
	owner := "maintainer"

	created, createdEvents, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{
			{
				ExternalID: "first", Title: "First imported issue", Body: "initial body",
				Author: "alice", Owner: &owner, Priority: &priority, Status: "open",
				CreatedAt: older, UpdatedAt: older, Labels: []string{"managed", "replace-me"},
				Comments: []db.ImportComment{{
					ExternalID: "comment-1", Author: "bob", Body: "first comment", CreatedAt: older,
				}},
				Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "second"}},
			},
			{
				ExternalID: "second", Title: "Second imported issue", Body: "closed body",
				Author: "carol", Status: "closed", ClosedReason: &closedReason,
				CreatedAt: older, UpdatedAt: newer, ClosedAt: &newer,
			},
		},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, "tracker", created.Source)
	assert.Equal(t, 2, created.Created)
	assert.Zero(t, created.Updated)
	assert.Zero(t, created.Unchanged)
	assert.Equal(t, 1, created.Comments)
	assert.Equal(t, 1, created.Links)
	require.Len(t, created.Items, 2)
	assert.Equal(t, "created", created.Items[0].Status)
	assert.Equal(t, "created", created.Items[1].Status)
	assert.NotEmpty(t, createdEvents)

	firstMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "first")
	if err != nil {
		return err
	}
	secondMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "second")
	if err != nil {
		return err
	}
	require.NotNil(t, firstMapping.IssueID)
	require.NotNil(t, secondMapping.IssueID)
	first, err := store.IssueByID(ctx, *firstMapping.IssueID)
	if err != nil {
		return err
	}
	second, err := store.IssueByID(ctx, *secondMapping.IssueID)
	if err != nil {
		return err
	}
	assert.Equal(t, "First imported issue", first.Title)
	assert.Equal(t, "initial body", first.Body)
	assert.Equal(t, "alice", first.Author)
	assert.Equal(t, owner, pointerString(first.Owner))
	assert.Equal(t, priority, pointerInt64(first.Priority))
	assert.True(t, first.CreatedAt.Equal(older))
	assert.True(t, first.UpdatedAt.Equal(older))
	assert.Equal(t, "closed", second.Status)
	assert.Equal(t, closedReason, pointerString(second.ClosedReason))
	require.NotNil(t, second.ClosedAt)
	assert.True(t, second.ClosedAt.Equal(newer))

	comments, err := store.CommentsByIssue(ctx, first.ID)
	if err != nil {
		return err
	}
	require.Len(t, comments, 1)
	assert.Equal(t, "bob", comments[0].Author)
	assert.Equal(t, "first comment", comments[0].Body)
	assert.True(t, comments[0].CreatedAt.Equal(older))
	commentMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "comment", "comment-1")
	if err != nil {
		return err
	}
	require.NotNil(t, commentMapping.CommentID)
	assert.Equal(t, comments[0].ID, *commentMapping.CommentID)

	labels, err := store.LabelsByIssue(ctx, first.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, []string{"managed", "replace-me"}, importLabelNames(labels))
	link, err := store.LinkByEndpoints(ctx, first.ID, second.ID, "blocks")
	if err != nil {
		return err
	}
	linkMapping, err := store.ImportMappingBySource(
		ctx, project.ID, "tracker", "link", "first:blocks:second",
	)
	if err != nil {
		return err
	}
	require.NotNil(t, linkMapping.LinkID)
	assert.Equal(t, link.ID, *linkMapping.LinkID)

	if _, err := store.AddLabel(ctx, first.ID, "local", "local-author"); err != nil {
		return err
	}
	localLink, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: first.ID, ToIssueID: second.ID, Type: "related", Author: "local-author",
	})
	if err != nil {
		return err
	}

	updatedAt := newer.Add(time.Hour)
	updated, updatedEvents, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{
			{
				ExternalID: "first", Title: "First updated", Body: "updated body",
				Author: "alice", Status: "closed", ClosedReason: &closedReason,
				CreatedAt: older, UpdatedAt: updatedAt, ClosedAt: &updatedAt,
				Labels: []string{"managed", "replacement"},
				Comments: []db.ImportComment{
					{ExternalID: "comment-1", Author: "bob", Body: "first comment", CreatedAt: older},
					{ExternalID: "comment-2", Author: "dana", Body: "later comment", CreatedAt: newer},
				},
			},
			{
				ExternalID: "second", Title: "Second imported issue", Body: "closed body",
				Author: "carol", Status: "closed", ClosedReason: &closedReason,
				CreatedAt: older, UpdatedAt: newer, ClosedAt: &newer,
			},
		},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, updated.Updated)
	assert.Equal(t, 1, updated.Unchanged)
	assert.Equal(t, 1, updated.Comments)
	assert.Zero(t, updated.Links)
	assert.NotEmpty(t, updatedEvents)
	first, err = store.IssueByID(ctx, first.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, "First updated", first.Title)
	assert.Equal(t, "updated body", first.Body)
	assert.Equal(t, "closed", first.Status)
	assert.True(t, first.UpdatedAt.Equal(updatedAt))
	labels, err = store.LabelsByIssue(ctx, first.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, []string{"local", "managed", "replacement"}, importLabelNames(labels))
	_, err = store.LinkByID(ctx, link.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.LinkByID(ctx, localLink.ID)
	assert.NoError(t, err)
	_, err = store.ImportMappingBySource(ctx, project.ID, "tracker", "link", "first:blocks:second")
	assert.ErrorIs(t, err, db.ErrNotFound)

	stale, staleEvents, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "first", Title: "stale title", Body: "stale body", Author: "alice",
			Status: "open", CreatedAt: older, UpdatedAt: older,
			Comments: []db.ImportComment{{
				ExternalID: "comment-3", Author: "erin", Body: "merged from stale issue", CreatedAt: older,
			}},
		}},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, stale.Unchanged)
	assert.Equal(t, 1, stale.Comments)
	require.Len(t, staleEvents, 1)
	assert.Equal(t, "issue.commented", staleEvents[0].Type)
	first, err = store.IssueByID(ctx, first.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, "First updated", first.Title)
	comments, err = store.CommentsByIssue(ctx, first.ID)
	if err != nil {
		return err
	}
	assert.Equal(t, []string{"first comment", "merged from stale issue", "later comment"}, importCommentBodies(comments))

	repeated, repeatedEvents, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "first", Title: "stale title", Body: "stale body", Author: "alice",
			Status: "open", CreatedAt: older, UpdatedAt: older,
			Comments: []db.ImportComment{{
				ExternalID: "comment-3", Author: "erin", Body: "merged from stale issue", CreatedAt: older,
			}},
		}},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, repeated.Unchanged)
	assert.Zero(t, repeated.Comments)
	assert.Empty(t, repeatedEvents)

	if err := checkImportTransactionRollback(ctx, t, store); err != nil {
		return err
	}
	if err := checkImportIssueSyncGuard(ctx, t, store); err != nil {
		return err
	}
	return checkImportFederationActor(ctx, t, store)
}

func checkExternalImportEdgeCases(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := context.Background()
	if err := checkImportPresentationAndLegacyKeys(ctx, t, store); err != nil {
		return err
	}
	if err := checkImportCorrectedCreationTime(ctx, t, store); err != nil {
		return err
	}
	if err := checkImportLocalLinkOwnership(ctx, t, store); err != nil {
		return err
	}
	if err := checkImportParentConflict(ctx, t, store); err != nil {
		return err
	}
	return checkImportValidationAndWriteGate(ctx, t, store)
}

func checkImportPresentationAndLegacyKeys(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-presentation")
	if err != nil {
		return err
	}
	createdAt := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	sourceAt := createdAt.Add(time.Hour)
	initial := db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "legacy-issue", Title: "Original title", Body: "body", Author: "alice",
			Status: "open", CreatedAt: createdAt, UpdatedAt: sourceAt,
			Comments: []db.ImportComment{{
				ExternalID: "legacy-comment", Author: "bob", Body: "legacy note", CreatedAt: sourceAt,
			}},
		}},
	}
	if _, _, err := store.ImportBatch(ctx, initial); err != nil {
		return err
	}
	corrected := initial
	corrected.Items = []db.ImportItem{{
		ExternalID: "legacy-issue", Title: "[External 1] Original title", Body: "body",
		Author: "alice", Status: "open", CreatedAt: createdAt, UpdatedAt: sourceAt,
	}}
	result, events, err := store.ImportBatch(ctx, corrected)
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Updated)
	require.Len(t, events, 1)
	assert.Equal(t, "issue.updated", events[0].Type)
	payload := db.PayloadMap(json.RawMessage(events[0].Payload))
	title, _ := db.StringValue(payload["title"])
	oldTitle, _ := db.StringValue(payload["old_title"])
	assert.Equal(t, "[External 1] Original title", title)
	assert.Equal(t, "Original title", oldTitle)

	legacyMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "legacy-issue")
	if err != nil {
		return err
	}
	require.NotNil(t, legacyMapping.IssueID)
	localTitle := "Locally edited title"
	if _, _, _, err := store.EditIssue(ctx, db.EditIssueParams{
		IssueID: *legacyMapping.IssueID, Title: &localTitle, Actor: "local-author",
	}); err != nil {
		return err
	}
	corrected.Items[0].Title = "[External 1] Another title"
	result, events, err = store.ImportBatch(ctx, corrected)
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Unchanged)
	assert.Empty(t, events)
	issue, err := store.IssueByID(ctx, *legacyMapping.IssueID)
	if err != nil {
		return err
	}
	assert.Equal(t, localTitle, issue.Title)

	canonical := initial
	canonical.Items = []db.ImportItem{{
		ExternalID: "canonical-issue", LegacyExternalIDs: []string{"legacy-issue"},
		Title: localTitle, Body: "body", Author: "alice", Status: "open",
		CreatedAt: createdAt, UpdatedAt: sourceAt,
		Comments: []db.ImportComment{{
			ExternalID: "canonical-comment", LegacyExternalIDs: []string{"legacy-comment"},
			Author: "bob", Body: "legacy note", CreatedAt: sourceAt,
		}},
	}}
	result, _, err = store.ImportBatch(ctx, canonical)
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Unchanged)
	assert.Zero(t, result.Comments)
	canonicalMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "canonical-issue")
	if err != nil {
		return err
	}
	require.NotNil(t, canonicalMapping.IssueID)
	assert.Equal(t, *legacyMapping.IssueID, *canonicalMapping.IssueID)
	_, err = store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "legacy-issue")
	assert.ErrorIs(t, err, db.ErrNotFound)
	comments, err := store.CommentsByIssue(ctx, *canonicalMapping.IssueID)
	if err != nil {
		return err
	}
	assert.Len(t, comments, 1)
	_, err = store.ImportMappingBySource(ctx, project.ID, "tracker", "comment", "legacy-comment")
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.ImportMappingBySource(ctx, project.ID, "tracker", "comment", "canonical-comment")
	assert.NoError(t, err)
	return nil
}

func checkImportCorrectedCreationTime(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-created-at")
	if err != nil {
		return err
	}
	realCreated := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	realClosed := realCreated.Add(24 * time.Hour)
	synthetic := realCreated.Add(9 * 24 * time.Hour)
	newer := synthetic.Add(5 * 24 * time.Hour)
	if _, _, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "created-at", Title: "Open", Author: "alice", Status: "open",
			CreatedAt: synthetic, UpdatedAt: synthetic,
		}},
	}); err != nil {
		return err
	}
	reason := "done"
	result, events, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "created-at", Title: "Closed", Author: "alice", Status: "closed",
			ClosedReason: &reason, CreatedAt: realCreated, UpdatedAt: newer, ClosedAt: &realClosed,
		}},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Updated)
	mapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "created-at")
	if err != nil {
		return err
	}
	require.NotNil(t, mapping.IssueID)
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	if err != nil {
		return err
	}
	assert.True(t, issue.CreatedAt.Equal(realCreated))
	require.NotNil(t, issue.ClosedAt)
	assert.True(t, issue.ClosedAt.Equal(realClosed))
	require.NotEmpty(t, events)
	payload := db.PayloadMap(json.RawMessage(events[0].Payload))
	createdValue, _ := db.StringValue(payload["created_at"])
	assert.Equal(t, "2026-07-01T10:00:00.000Z", createdValue)
	return nil
}

func checkImportLocalLinkOwnership(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-local-link")
	if err != nil {
		return err
	}
	firstAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	baseItems := []db.ImportItem{
		{ExternalID: "left", Title: "Left", Author: "alice", Status: "open", CreatedAt: firstAt, UpdatedAt: firstAt},
		{ExternalID: "right", Title: "Right", Author: "bob", Status: "open", CreatedAt: firstAt, UpdatedAt: firstAt},
	}
	if _, _, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent", Items: baseItems,
	}); err != nil {
		return err
	}
	leftMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "left")
	if err != nil {
		return err
	}
	rightMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "right")
	if err != nil {
		return err
	}
	require.NotNil(t, leftMapping.IssueID)
	require.NotNil(t, rightMapping.IssueID)
	localLink, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: *leftMapping.IssueID, ToIssueID: *rightMapping.IssueID,
		Type: "blocks", Author: "local-author",
	})
	if err != nil {
		return err
	}
	secondAt := firstAt.Add(time.Hour)
	baseItems[0].Title = "Left updated"
	baseItems[0].UpdatedAt = secondAt
	baseItems[0].Links = []db.ImportLink{{Type: "blocks", TargetExternalID: "right"}}
	result, _, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent", Items: baseItems,
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Updated)
	assert.Zero(t, result.Links)
	_, err = store.ImportMappingBySource(ctx, project.ID, "tracker", "link", "left:blocks:right")
	assert.ErrorIs(t, err, db.ErrNotFound)
	baseItems[0].Title = "Left updated again"
	baseItems[0].UpdatedAt = secondAt.Add(time.Hour)
	baseItems[0].Links = nil
	if _, _, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent", Items: baseItems,
	}); err != nil {
		return err
	}
	_, err = store.LinkByID(ctx, localLink.ID)
	assert.NoError(t, err)
	return nil
}

func checkImportParentConflict(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-parent-conflict")
	if err != nil {
		return err
	}
	at := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	items := []db.ImportItem{
		{ExternalID: "child", Title: "Child", Author: "alice", Status: "open", CreatedAt: at, UpdatedAt: at},
		{ExternalID: "source-parent", Title: "Source parent", Author: "bob", Status: "open", CreatedAt: at, UpdatedAt: at},
		{ExternalID: "local-parent", Title: "Local parent", Author: "carol", Status: "open", CreatedAt: at, UpdatedAt: at},
	}
	if _, _, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent", Items: items,
	}); err != nil {
		return err
	}
	childMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "child")
	if err != nil {
		return err
	}
	sourceParentMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "source-parent")
	if err != nil {
		return err
	}
	localParentMapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "local-parent")
	if err != nil {
		return err
	}
	require.NotNil(t, childMapping.IssueID)
	require.NotNil(t, sourceParentMapping.IssueID)
	require.NotNil(t, localParentMapping.IssueID)
	localParent, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: *childMapping.IssueID, ToIssueID: *localParentMapping.IssueID,
		Type: "parent", Author: "local-author",
	})
	if err != nil {
		return err
	}
	items[0].Title = "Child from source"
	items[0].UpdatedAt = at.Add(time.Hour)
	items[0].Links = []db.ImportLink{{Type: "parent", TargetExternalID: "source-parent"}}
	params := db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent", Items: items,
	}
	_, _, err = store.ImportBatch(ctx, params)
	assert.ErrorIs(t, err, db.ErrParentAlreadySet)
	child, err := store.IssueByID(ctx, *childMapping.IssueID)
	if err != nil {
		return err
	}
	assert.Equal(t, "Child", child.Title, "failed parent reconciliation must roll back issue updates")
	params.PreserveLocalParentConflicts = true
	result, _, err := store.ImportBatch(ctx, params)
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Updated)
	_, err = store.LinkByID(ctx, localParent.ID)
	assert.NoError(t, err)
	_, err = store.LinkByEndpoints(ctx, *childMapping.IssueID, *sourceParentMapping.IssueID, "parent")
	assert.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

func checkImportValidationAndWriteGate(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-validation")
	if err != nil {
		return err
	}
	at := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{ProjectID: project.ID, Actor: "import-agent"})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "invalid-title", Title: "before\x00after", Author: "alice",
			Status: "open", CreatedAt: at, UpdatedAt: at,
		}},
	})
	assert.ErrorIs(t, err, db.ErrImportValidation)
	issues, listErr := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID, Limit: 100})
	if listErr != nil {
		return listErr
	}
	assert.Empty(t, issues, "a rejected import must not persist its invalid issue")
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{
			{ExternalID: "one", Title: "One", Author: "alice", Status: "open", CreatedAt: at, UpdatedAt: at,
				Comments: []db.ImportComment{{ExternalID: "duplicate", Author: "alice", Body: "one", CreatedAt: at}}},
			{ExternalID: "two", Title: "Two", Author: "bob", Status: "open", CreatedAt: at, UpdatedAt: at,
				Comments: []db.ImportComment{{ExternalID: "duplicate", Author: "bob", Body: "two", CreatedAt: at}}},
		},
	})
	assert.ErrorIs(t, err, db.ErrImportValidation)

	readOnlyProject, err := store.CreateProject(ctx, "external-import-read-only")
	if err != nil {
		return err
	}
	hubUID, err := uid.New()
	if err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: readOnlyProject.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 51, HubProjectUID: hubUID,
		ReplayHorizonEventID: 1, Enabled: true,
	})
	if err != nil {
		return err
	}
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: readOnlyProject.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "blocked", Title: "Blocked", Author: "alice", Status: "open",
			CreatedAt: at, UpdatedAt: at,
		}},
	})
	assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
	return nil
}

func checkImportTransactionRollback(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-rollback")
	if err != nil {
		return err
	}
	at := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "import-agent",
		Items: []db.ImportItem{{
			ExternalID: "rollback", Title: "Must roll back", Author: "alice",
			Status: "open", CreatedAt: at, UpdatedAt: at,
			Links: []db.ImportLink{{Type: "blocks", TargetExternalID: "missing"}},
		}},
	})
	require.Error(t, err)
	issues, listErr := store.ListIssues(ctx, db.ListIssuesParams{ProjectID: project.ID, Limit: 100})
	if listErr != nil {
		return listErr
	}
	assert.Empty(t, issues)
	_, mappingErr := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "rollback")
	assert.ErrorIs(t, mappingErr, db.ErrNotFound)
	return nil
}

func checkImportIssueSyncGuard(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-guard")
	if err != nil {
		return err
	}
	binding, err := store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
		ProjectID: project.ID, Provider: "example", SourceKey: "example:guard",
		RemoteID: "guard", DisplayName: "Guarded import", Config: json.RawMessage(`{}`),
		IntervalSeconds: 60,
	})
	if err != nil {
		return err
	}
	started := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)
	_, claimed, err := store.ClaimIssueSyncBinding(ctx, binding.ID, "example", started, started.Add(-time.Hour))
	if err != nil {
		return err
	}
	require.True(t, claimed)
	at := started.Add(time.Minute)
	params := db.ImportBatchParams{
		ProjectID: project.ID, Source: "example:guard", Actor: "sync-agent",
		IssueSyncGuard: &db.IssueSyncImportGuard{
			BindingID: binding.ID, Provider: "example", StartedAt: started,
		},
		Items: []db.ImportItem{{
			ExternalID: "guarded", Title: "Guarded issue", Author: "sync-agent",
			Status: "open", CreatedAt: at, UpdatedAt: at,
		}},
	}
	result, _, err := store.ImportBatch(ctx, params)
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Created)
	params.IssueSyncGuard = &db.IssueSyncImportGuard{
		BindingID: binding.ID, Provider: "example", StartedAt: started.Add(time.Second),
	}
	_, _, err = store.ImportBatch(ctx, params)
	assert.ErrorIs(t, err, db.ErrIssueSyncAlreadyRunning)
	return nil
}

func checkImportFederationActor(ctx context.Context, t *testing.T, store db.Storage) error {
	project, err := store.CreateProject(ctx, "external-import-federated")
	if err != nil {
		return err
	}
	hubUID, err := uid.New()
	if err != nil {
		return err
	}
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example",
		HubProjectID: 41, HubProjectUID: hubUID, ReplayHorizonEventID: 1,
		PullCursorEventID: 0, PushEnabled: true, Actor: "bound-agent", Enabled: true,
	})
	if err != nil {
		return err
	}
	at := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	result, events, err := store.ImportBatch(ctx, db.ImportBatchParams{
		ProjectID: project.ID, Source: "tracker", Actor: "request-agent",
		Items: []db.ImportItem{{
			ExternalID: "federated", Title: "Federated import", Author: "source-author",
			Status: "open", CreatedAt: at, UpdatedAt: at,
			Comments: []db.ImportComment{{
				ExternalID: "federated-comment", Author: "source-commenter", Body: "note", CreatedAt: at,
			}},
		}},
	})
	if err != nil {
		return err
	}
	assert.Equal(t, 1, result.Created)
	mapping, err := store.ImportMappingBySource(ctx, project.ID, "tracker", "issue", "federated")
	if err != nil {
		return err
	}
	require.NotNil(t, mapping.IssueID)
	issue, err := store.IssueByID(ctx, *mapping.IssueID)
	if err != nil {
		return err
	}
	assert.Equal(t, "bound-agent", issue.Author)
	comments, err := store.CommentsByIssue(ctx, issue.ID)
	if err != nil {
		return err
	}
	require.Len(t, comments, 1)
	assert.Equal(t, "bound-agent", comments[0].Author)
	for _, event := range events {
		assert.Equal(t, "bound-agent", event.Actor)
	}
	return nil
}

func importLabelNames(labels []db.IssueLabel) []string {
	output := make([]string, len(labels))
	for index := range labels {
		output[index] = labels[index].Label
	}
	return output
}

func importCommentBodies(comments []db.Comment) []string {
	output := make([]string, len(comments))
	for index := range comments {
		output[index] = comments[index].Body
	}
	return output
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func pointerInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
