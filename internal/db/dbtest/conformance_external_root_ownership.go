package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkExternalRootContentOwnership(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()

	t.Run("issue-sync managed content cannot be externally bound", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "issue-sync-owned-root")
		require.NoError(t, err)
		const source = "example-sync:managed-roots"
		_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
			ProjectID: project.ID, Provider: "example-sync", SourceKey: source,
			RemoteID: "managed-roots", DisplayName: "Managed roots",
			Config: []byte(`{}`), IntervalSeconds: 300,
		})
		require.NoError(t, err)
		createdAt := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
		item := db.ImportItem{
			ExternalID: "managed-root-1", Title: "Managed root", Body: "Managed body",
			Author: "sync-agent", Status: "open", CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
			ProjectID: project.ID, Source: source, Actor: "sync-agent", Items: []db.ImportItem{item},
		})
		require.NoError(t, err)
		mapping, err := store.ImportMappingBySource(ctx, project.ID, source, "issue", item.ExternalID)
		require.NoError(t, err)
		require.NotNil(t, mapping.IssueID)

		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: *mapping.IssueID,
			ConnectorInstance: "notes", ExternalRootKey: "managed-external-root",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: createdAt,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootIssueSyncConflict)
		_, readErr := store.ImportMappingBySource(
			ctx, project.ID, "connector:notes", "issue", "managed-external-root",
		)
		assert.ErrorIs(t, readErr, db.ErrNotFound)
	})

	t.Run("externally bound content cannot become issue-sync managed", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "late-issue-sync-owned-root")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Already bound root", Author: "tester",
		})
		require.NoError(t, err)
		issueID := issue.ID
		const source = "example-sync:late-managed-roots"
		_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
			Source: source, ExternalID: "managed-root-1", ObjectType: "issue",
			ProjectID: project.ID, IssueID: &issueID,
		})
		require.NoError(t, err)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "late-managed-external-root",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)

		_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
			ProjectID: project.ID, Provider: "example-sync", SourceKey: source,
			RemoteID: "late-managed-roots", DisplayName: "Late managed roots",
			Config: []byte(`{}`), IntervalSeconds: 300,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootIssueSyncConflict)
	})

	t.Run("issue-sync mapping cannot target externally bound content", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "post-bound-issue-sync-root")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Already externally bound", Author: "tester",
		})
		require.NoError(t, err)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "post-bound-external-root",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		const source = "example-sync:post-bound-roots"
		_, err = store.UpsertIssueSyncBinding(ctx, db.UpsertIssueSyncBindingParams{
			ProjectID: project.ID, Provider: "example-sync", SourceKey: source,
			RemoteID: "post-bound-roots", DisplayName: "Post-bound roots",
			Config: []byte(`{}`), IntervalSeconds: 300,
		})
		require.NoError(t, err)
		issueID := issue.ID

		_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
			Source: source, ExternalID: "managed-root-1", ObjectType: "issue",
			ProjectID: project.ID, IssueID: &issueID,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootIssueSyncConflict)
		_, readErr := store.ImportMappingBySource(ctx, project.ID, source, "issue", "managed-root-1")
		assert.ErrorIs(t, readErr, db.ErrNotFound)
	})

	t.Run("legacy import identity cannot rename a bound root mapping", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "legacy-root-rekey")
		item := db.ImportItem{
			ExternalID: "replacement-root-key", LegacyExternalIDs: []string{fixture.binding.ExternalRootKey},
			Title: fixture.issue.Title, Body: fixture.issue.Body, Author: fixture.issue.Author,
			Status: fixture.issue.Status, CreatedAt: fixture.issue.CreatedAt, UpdatedAt: fixture.issue.UpdatedAt,
		}
		_, _, err := store.ImportBatch(ctx, db.ImportBatchParams{
			ProjectID: fixture.project.ID, Source: "connector:notes", Actor: "import-agent",
			Items: []db.ImportItem{item},
		})
		assert.ErrorIs(t, err, db.ErrExternalRootAlreadyBound)
		retained, readErr := store.ImportMappingBySource(
			ctx, fixture.project.ID, "connector:notes", "issue", fixture.binding.ExternalRootKey,
		)
		require.NoError(t, readErr)
		assert.Equal(t, fixture.binding.RootMappingID, retained.ID)
		_, readErr = store.ImportMappingBySource(
			ctx, fixture.project.ID, "connector:notes", "issue", item.ExternalID,
		)
		assert.ErrorIs(t, readErr, db.ErrNotFound)
	})

	t.Run("local content is externally owned until unbind", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "content-ownership")
		before, err := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, err)
		beforeEventID, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		title, body := "Local replacement", "Local replacement body"

		_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{
			IssueID: fixture.issue.ID, Title: &title, Actor: "tester",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)
		_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{
			IssueID: fixture.issue.ID, Body: &body, Actor: "tester",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)

		priority := int64(2)
		_, err = store.EditIssueAtomic(ctx, db.EditIssueAtomicParams{
			IssueID: fixture.issue.ID, Actor: "tester", Title: &title, SetPriority: &priority,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)
		afterRejected, readErr := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, before.Title, afterRejected.Title)
		assert.Equal(t, before.Body, afterRejected.Body)
		assert.Equal(t, before.Owner, afterRejected.Owner)
		assert.Equal(t, before.Priority, afterRejected.Priority)
		assert.Equal(t, before.Revision, afterRejected.Revision)
		afterRejectedEventID, readErr := store.MaxEventID(ctx)
		require.NoError(t, readErr)
		assert.Equal(t, beforeEventID, afterRejectedEventID)

		owner := "operator"
		allowed, event, changed, err := store.EditIssue(ctx, db.EditIssueParams{
			IssueID: fixture.issue.ID, Title: &before.Title, Owner: &owner, Actor: "tester",
		})
		require.NoError(t, err)
		assert.True(t, changed)
		require.NotNil(t, event)
		assert.Equal(t, owner, *allowed.Owner)
		assert.Equal(t, before.Title, allowed.Title)

		_, _, err = store.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Actor: "tester", Reason: "operator_pause",
		})
		require.NoError(t, err)
		_, _, _, err = store.EditIssue(ctx, db.EditIssueParams{
			IssueID: fixture.issue.ID, Title: &title, Actor: "tester",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)

		_, _, err = store.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: fixture.binding.ID, Actor: "tester",
		})
		require.NoError(t, err)
		unbound, _, changed, err := store.EditIssue(ctx, db.EditIssueParams{
			IssueID: fixture.issue.ID, Title: &title, Actor: "tester",
		})
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, title, unbound.Title)
	})

	t.Run("inbound comments remain externally owned", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "comment-content-ownership")
		providerAt := time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC)
		projection := db.ExternalCommentProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			ExternalID: "provider-comment", ExternalRevision: "revision-one",
			Body: "Provider-authored comment", ExternalActorID: "provider-actor",
			ExternalActorName: "Contributor", ExternalCreatedAt: providerAt,
			ExternalUpdatedAt: providerAt, IntegrationActor: "connector:notes",
		}
		comment, event, changed, err := store.UpsertExternalCommentProjection(ctx, projection)
		require.NoError(t, err)
		assert.True(t, changed)
		require.NotNil(t, event)
		beforeEventID, err := store.MaxEventID(ctx)
		require.NoError(t, err)

		_, _, _, err = store.EditComment(ctx, db.EditCommentParams{
			IssueID: fixture.issue.ID, CommentUID: comment.UID,
			Body: "Locally edited comment", Actor: "tester",
		})
		assert.ErrorIs(t, err, db.ErrExternalCommentContentOwned)
		comments, err := store.CommentsByIssue(ctx, fixture.issue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		assert.Equal(t, projection.Body, comments[0].Body)
		afterEventID, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		assert.Equal(t, beforeEventID, afterEventID)

		reconciled, replayEvent, replayChanged, err := store.UpsertExternalCommentProjection(ctx, projection)
		require.NoError(t, err)
		assert.False(t, replayChanged)
		assert.Nil(t, replayEvent)
		assert.Equal(t, projection.Body, reconciled.Body)
	})

	t.Run("import content overwrite rejects the whole batch", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-import-ownership")
		require.NoError(t, err)
		createdAt := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
		initial := db.ImportItem{
			ExternalID: "imported-root", Title: "Imported root", Body: "Imported body",
			Author: "import-agent", Status: "open", CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
			ProjectID: project.ID, Source: "example-import", Actor: "import-agent",
			Items: []db.ImportItem{initial},
		})
		require.NoError(t, err)
		mapping, err := store.ImportMappingBySource(ctx, project.ID, "example-import", "issue", initial.ExternalID)
		require.NoError(t, err)
		require.NotNil(t, mapping.IssueID)
		issue, err := store.IssueByID(ctx, *mapping.IssueID)
		require.NoError(t, err)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "notes",
			ExternalRootKey: "root-import-ownership", ExternalAccountKey: "opaque-account-key",
			Actor: "tester", ReceiveCommentsAfter: createdAt,
		})
		require.NoError(t, err)
		before, err := store.IssueByID(ctx, issue.ID)
		require.NoError(t, err)
		beforeEventID, err := store.MaxEventID(ctx)
		require.NoError(t, err)
		priority := int64(2)
		overwrite := initial
		overwrite.Title = "Forbidden imported title"
		overwrite.Body = "Forbidden imported body"
		overwrite.Priority = &priority
		overwrite.UpdatedAt = createdAt.Add(time.Hour)

		_, _, err = store.ImportBatch(ctx, db.ImportBatchParams{
			ProjectID: project.ID, Source: "example-import", Actor: "import-agent",
			Items: []db.ImportItem{overwrite},
		})
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)
		afterRejected, readErr := store.IssueByID(ctx, issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, before.Title, afterRejected.Title)
		assert.Equal(t, before.Body, afterRejected.Body)
		assert.Equal(t, before.Priority, afterRejected.Priority)
		assert.Equal(t, before.Revision, afterRejected.Revision)
		afterEventID, readErr := store.MaxEventID(ctx)
		require.NoError(t, readErr)
		assert.Equal(t, beforeEventID, afterEventID)

		owner := "operator"
		nonContent := initial
		nonContent.Owner = &owner
		nonContent.Priority = &priority
		nonContent.UpdatedAt = createdAt.Add(2 * time.Hour)
		result, events, err := store.ImportBatch(ctx, db.ImportBatchParams{
			ProjectID: project.ID, Source: "example-import", Actor: "import-agent",
			Items: []db.ImportItem{nonContent},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, result.Updated)
		require.NotEmpty(t, events)
		allowed, readErr := store.IssueByID(ctx, issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, before.Title, allowed.Title)
		assert.Equal(t, before.Body, allowed.Body)
		assert.Equal(t, owner, *allowed.Owner)
		assert.Equal(t, priority, *allowed.Priority)
	})

	t.Run("federation content projection follows ingest rejection", func(t *testing.T) {
		hubProject, err := store.CreateProject(ctx, "external-root-federation-ownership")
		require.NoError(t, err)
		hubIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: hubProject.ID, Title: "Federated root", Body: "Federated body", Author: "tester",
		})
		require.NoError(t, err)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: hubProject.ID, IssueID: hubIssue.ID, ConnectorInstance: "notes",
			ExternalRootKey: "root-federation-ownership", ExternalAccountKey: "opaque-account-key",
			Actor: "tester", ReceiveCommentsAfter: time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		binding, claimed, err := store.ClaimExternalRootBinding(
			ctx, binding.ID, "claim-federation-ownership",
			time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 20, 8, 59, 0, 0, time.UTC),
		)
		require.NoError(t, err)
		require.True(t, claimed)
		providerAt := time.Date(2026, 8, 20, 9, 5, 0, 0, time.UTC)
		providerComment, _, changed, err := store.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{
			BindingID: binding.ID, ClaimToken: binding.ClaimToken,
			ExternalID: "provider-comment", ExternalRevision: "revision-one",
			Body: "Provider-authored comment", ExternalActorID: "provider-actor",
			ExternalActorName: "Contributor", ExternalCreatedAt: providerAt,
			ExternalUpdatedAt: providerAt, IntegrationActor: "connector:notes",
		})
		require.NoError(t, err)
		require.True(t, changed)
		_, err = store.EnableProjectFederation(ctx, hubProject.ID, "tester")
		require.NoError(t, err)

		spoke := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, spoke.Close()) })
		spokeProject, err := spoke.CreateProjectWithUID(ctx, "example-spoke-project", hubProject.UID)
		require.NoError(t, err)
		spokeIssue, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: spokeProject.ID, UID: hubIssue.UID, Title: hubIssue.Title,
			Body: hubIssue.Body, Author: "tester",
		})
		require.NoError(t, err)
		remoteTitle := "Forbidden federated title"
		_, remoteEvent, changed, err := spoke.EditIssue(ctx, db.EditIssueParams{
			IssueID: spokeIssue.ID, Title: &remoteTitle, Actor: "tester",
		})
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, remoteEvent)
		beforeEventID, err := store.MaxEventID(ctx)
		require.NoError(t, err)

		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "tester",
			Events: []db.FederationIngestEvent{{SourceEventID: remoteEvent.ID, Event: remoteEventFromStored(*remoteEvent)}},
		})
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)
		assert.ErrorIs(t, err, db.ErrFederationIngestValidation)
		retained, readErr := store.IssueByID(ctx, hubIssue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, hubIssue.Title, retained.Title)
		assert.Equal(t, hubIssue.Body, retained.Body)
		assert.Equal(t, hubIssue.Revision, retained.Revision)
		afterEventID, readErr := store.MaxEventID(ctx)
		require.NoError(t, readErr)
		assert.Equal(t, beforeEventID, afterEventID)

		remoteCommentEdit := newRemoteEvent(
			t, hubProject, &hubIssue.UID, "issue.comment_edited", "tester", spoke.InstanceUID(), 303,
			[]byte(`{"comment_uid":"`+providerComment.UID+`","body":"Forbidden federated comment","edited_at":"2026-08-20T09:10:00.000Z"}`),
		)
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "tester",
			Events: []db.FederationIngestEvent{{SourceEventID: 2, Event: remoteCommentEdit}},
		})
		assert.ErrorIs(t, err, db.ErrExternalCommentContentOwned)
		assert.ErrorIs(t, err, db.ErrFederationIngestValidation)
		comments, readErr := store.CommentsByIssue(ctx, hubIssue.ID)
		require.NoError(t, readErr)
		require.Len(t, comments, 1)
		assert.Equal(t, providerComment.Body, comments[0].Body)

		sameBodyCommentEdit := newRemoteEvent(
			t, hubProject, &hubIssue.UID, "issue.comment_edited", "tester", spoke.InstanceUID(), 304,
			[]byte(`{"comment_uid":"`+providerComment.UID+`","body":"`+providerComment.Body+`","edited_at":"2026-08-20T09:15:00.000Z"}`),
		)
		accepted, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "tester",
			Events: []db.FederationIngestEvent{{SourceEventID: 3, Event: sameBodyCommentEdit}},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, accepted.Accepted)
		comments, readErr = store.CommentsByIssue(ctx, hubIssue.ID)
		require.NoError(t, readErr)
		require.Len(t, comments, 1)
		assert.Equal(t, providerComment.Author, comments[0].Author)
		assert.Equal(t, providerComment.CreatedAt, comments[0].CreatedAt)
	})

	t.Run("federation delete projection cannot delete externally bound issue", func(t *testing.T) {
		hubProject, err := store.CreateProject(ctx, "external-root-federation-delete")
		require.NoError(t, err)
		hubIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: hubProject.ID, Title: "Federated root", Body: "Federated body", Author: "tester",
		})
		require.NoError(t, err)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: hubProject.ID, IssueID: hubIssue.ID, ConnectorInstance: "notes",
			ExternalRootKey: "root-federation-delete", ExternalAccountKey: "opaque-account-key",
			Actor: "tester", ReceiveCommentsAfter: time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, hubProject.ID, "tester")
		require.NoError(t, err)

		spoke := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, spoke.Close()) })
		spokeProject, err := spoke.CreateProjectWithUID(ctx, "example-delete-spoke", hubProject.UID)
		require.NoError(t, err)
		spokeIssue, _, err := spoke.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: spokeProject.ID, UID: hubIssue.UID, Title: hubIssue.Title,
			Body: hubIssue.Body, Author: "tester",
		})
		require.NoError(t, err)
		_, remoteEvent, changed, err := spoke.SoftDeleteIssue(ctx, spokeIssue.ID, "tester")
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, remoteEvent)
		beforeEventID, err := store.MaxEventID(ctx)
		require.NoError(t, err)

		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: hubProject.ID, SpokeInstanceUID: spoke.InstanceUID(), BoundActor: "tester",
			Events: []db.FederationIngestEvent{{SourceEventID: remoteEvent.ID, Event: remoteEventFromStored(*remoteEvent)}},
		})
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)
		assert.ErrorIs(t, err, db.ErrFederationIngestValidation)
		retained, readErr := store.IssueByID(ctx, hubIssue.ID)
		require.NoError(t, readErr)
		assert.Nil(t, retained.DeletedAt)
		assert.Equal(t, hubIssue.Revision, retained.Revision)
		afterEventID, readErr := store.MaxEventID(ctx)
		require.NoError(t, readErr)
		assert.Equal(t, beforeEventID, afterEventID)
	})

	if t.Failed() {
		return fmt.Errorf("external root content ownership checks failed")
	}
	return nil
}
