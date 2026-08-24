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

type externalRootSafetyFixture struct {
	project db.Project
	issue   db.Issue
	binding db.ExternalRootBinding
	token   string
	now     time.Time
}

type metadataUpdatePayload struct {
	Diff map[string]struct {
		From json.RawMessage `json:"from"`
		To   json.RawMessage `json:"to"`
	} `json:"diff"`
	RevisionNew int64  `json:"revision_new"`
	UpdatedAt   string `json:"updated_at"`
}

func newExternalRootSafetyFixture(
	t *testing.T,
	store db.Storage,
	suffix string,
) externalRootSafetyFixture {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "external-root-safety-"+suffix)
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "External root safety", Author: "tester",
	})
	require.NoError(t, err)
	binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-" + suffix,
		ExternalAccountKey: "opaque-account-key", Actor: "tester",
		ReceiveCommentsAfter: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	token := "claim-" + suffix
	binding, claimed, err := store.ClaimExternalRootBinding(ctx, binding.ID, token, now, now.Add(-time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	return externalRootSafetyFixture{project: project, issue: issue, binding: binding, token: token, now: now}
}

func checkExternalRootSafetyInvariants(t *testing.T, store db.Storage, backend Backend) error {
	t.Helper()
	ctx := context.Background()

	t.Run("external root storage rejects NUL identities and bodies", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-nul-validation")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "External root", Author: "tester",
		})
		require.NoError(t, err)
		now := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
		for _, mutate := range []func(*db.CreateExternalRootBindingParams){
			func(params *db.CreateExternalRootBindingParams) { params.ConnectorInstance = "notes\x00connector" },
			func(params *db.CreateExternalRootBindingParams) { params.ExternalRootKey = "root\x00key" },
			func(params *db.CreateExternalRootBindingParams) { params.ExternalAccountKey = "account\x00key" },
		} {
			params := db.CreateExternalRootBindingParams{
				ProjectID: project.ID, IssueID: issue.ID, ConnectorInstance: "notes",
				ExternalRootKey: "root", ExternalAccountKey: "account", Actor: "tester",
				ReceiveCommentsAfter: now,
			}
			mutate(&params)
			_, _, err := store.CreateExternalRootBinding(ctx, params)
			assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		}

		fixture := newExternalRootSafetyFixture(t, store, "nul-projection")
		for _, mutate := range []func(*db.ExternalRootProjectionParams){
			func(params *db.ExternalRootProjectionParams) { params.ExternalRevision = "revision\x00value" },
			func(params *db.ExternalRootProjectionParams) { params.Body = "body\x00value" },
			func(params *db.ExternalRootProjectionParams) { params.ExternalActorID = "actor\x00id" },
		} {
			params := db.ExternalRootProjectionParams{
				BindingID: fixture.binding.ID, ClaimToken: fixture.token,
				Title: "External root", Body: "Body", ExternalRevision: "revision",
				ExternalActorID: "actor", ExternalActorName: "Contributor",
				ExternalUpdatedAt: fixture.now, ExternalObservedAt: fixture.now,
				IntegrationActor: "connector:notes",
			}
			mutate(&params)
			_, _, _, err := store.ApplyExternalRootProjection(ctx, params)
			assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		}
		for _, mutate := range []func(*db.ExternalCommentProjectionParams){
			func(params *db.ExternalCommentProjectionParams) { params.ExternalID = "comment\x00id" },
			func(params *db.ExternalCommentProjectionParams) { params.Body = "body\x00value" },
			func(params *db.ExternalCommentProjectionParams) { params.ExternalActorID = "actor\x00id" },
		} {
			params := db.ExternalCommentProjectionParams{
				BindingID: fixture.binding.ID, ClaimToken: fixture.token,
				ExternalID: "comment", ExternalRevision: "revision", Body: "Body",
				ExternalActorID: "actor", ExternalActorName: "Contributor",
				ExternalCreatedAt: fixture.now, ExternalUpdatedAt: fixture.now,
				IntegrationActor: "connector:notes",
			}
			mutate(&params)
			_, _, _, err := store.UpsertExternalCommentProjection(ctx, params)
			assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		}
	})

	t.Run("external root storage rejects impossible projection timestamps", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "timestamp-ordering")
		_, _, _, err := store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Original projection", Body: "Body", ExternalRevision: "revision-one",
			ExternalUpdatedAt: fixture.now, ExternalObservedAt: fixture.now,
			IntegrationActor: "connector:notes",
		})
		require.NoError(t, err)

		_, _, _, err = store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Rewritten projection", Body: "Body", ExternalRevision: "revision-two",
			ExternalUpdatedAt: fixture.now.Add(time.Minute), ExternalObservedAt: fixture.now,
			IntegrationActor: "connector:notes",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		projected, readErr := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, "Original projection", projected.Title,
			"rejected root projection must not mutate the projected issue")

		_, _, _, err = store.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			ExternalID: "comment-one", ExternalRevision: "comment-revision", Body: "Body",
			ExternalActorID: "actor", ExternalActorName: "Contributor",
			ExternalCreatedAt: fixture.now, ExternalUpdatedAt: fixture.now.Add(-time.Minute),
			IntegrationActor: "connector:notes",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		comments, readErr := store.CommentsByIssue(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Empty(t, comments, "rejected comment projection must not insert a comment")
	})

	t.Run("live claims fence field mappings and operator lifecycle actions", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-live-claim-fence")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Claimed root", Author: "tester",
		})
		require.NoError(t, err)
		mapping, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
			ConnectorInstance: "claimed-notes", KataField: "scheduled_on",
			ExternalFieldID: "start-original", ExternalFieldName: "Start",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-original",
		})
		require.NoError(t, err)
		now := time.Now().UTC().Truncate(time.Millisecond)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID,
			ConnectorInstance: "claimed-notes", ExternalRootKey: "claimed-root",
			ExternalAccountKey: "opaque-account-key", Actor: "tester", ReceiveCommentsAfter: now,
		})
		require.NoError(t, err)
		claimed, acquired, err := store.ClaimExternalRootBinding(
			ctx, binding.ID, "live-claim", now, now.Add(-5*time.Minute),
		)
		require.NoError(t, err)
		require.True(t, acquired)

		_, remapErr := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
			ConnectorInstance: "claimed-notes", KataField: "scheduled_on",
			ExternalFieldID: "start-replacement", ExternalFieldName: "Replacement start",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-replacement",
		})
		assert.ErrorIs(t, remapErr, db.ErrExternalRootClaimActive)
		_, unmapErr := store.UnmapExternalField(ctx, "claimed-notes", "scheduled_on")
		assert.ErrorIs(t, unmapErr, db.ErrExternalRootClaimActive)
		_, _, pauseErr := store.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: binding.ID, Actor: "tester", Reason: "operator_pause",
		})
		assert.ErrorIs(t, pauseErr, db.ErrExternalRootClaimActive)
		_, _, unbindErr := store.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: binding.ID, Actor: "tester",
		})
		assert.ErrorIs(t, unbindErr, db.ErrExternalRootClaimActive)

		retained, readErr := store.ExternalRootBindingByID(ctx, binding.ID)
		require.NoError(t, readErr)
		assert.True(t, retained.Active)
		assert.True(t, retained.Enabled)
		assert.Equal(t, claimed.ClaimToken, retained.ClaimToken)
		mappings, readErr := store.ListExternalFieldMappings(ctx, "claimed-notes")
		require.NoError(t, readErr)
		require.Len(t, mappings, 1)
		assert.Equal(t, mapping.ID, mappings[0].ID)
		assert.True(t, mappings[0].Active)
		assert.Equal(t, "start-original", mappings[0].ExternalFieldID)
	})

	t.Run("field mapping changes invalidate stale worker claims", func(t *testing.T) {
		for _, action := range []string{"remap", "unmap"} {
			t.Run(action, func(t *testing.T) {
				connectorInstance := "stale-claim-" + action
				project, err := store.CreateProject(ctx, "external-root-stale-claim-"+action)
				require.NoError(t, err)
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: project.ID, Title: "Stale claimed root", Author: "tester",
				})
				require.NoError(t, err)
				_, err = store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
					ConnectorInstance: connectorInstance, KataField: "scheduled_on",
					ExternalFieldID: "start-original", ExternalFieldName: "Start",
					AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
					SchemaRevision: "schema-original",
				})
				require.NoError(t, err)
				frontier := time.Now().UTC().Truncate(time.Millisecond)
				binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
					ProjectID: project.ID, IssueID: issue.ID,
					ConnectorInstance: connectorInstance, ExternalRootKey: "root-" + action,
					ExternalAccountKey: "opaque-account-key", Actor: "tester",
					ReceiveCommentsAfter: frontier,
				})
				require.NoError(t, err)
				staleAt := frontier.Add(-2 * db.ExternalRootClaimStaleAfter)
				_, acquired, err := store.ClaimExternalRootBinding(
					ctx, binding.ID, "stale-worker", staleAt, staleAt.Add(-time.Minute),
				)
				require.NoError(t, err)
				require.True(t, acquired)

				switch action {
				case "remap":
					_, err = store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
						ConnectorInstance: connectorInstance, KataField: "scheduled_on",
						ExternalFieldID: "start-replacement", ExternalFieldName: "Replacement start",
						AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
						SchemaRevision: "schema-replacement",
					})
				case "unmap":
					_, err = store.UnmapExternalField(ctx, connectorInstance, "scheduled_on")
				}
				require.NoError(t, err)
				retained, err := store.ExternalRootBindingByID(ctx, binding.ID)
				require.NoError(t, err)
				assert.Empty(t, retained.ClaimToken)
				assert.Nil(t, retained.ClaimStartedAt)
				_, err = store.RenewExternalRootClaim(ctx, binding.ID, "stale-worker", frontier)
				assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
			})
		}
	})

	t.Run("read-only federated spokes reject binding and projection writes", func(t *testing.T) {
		hub, err := store.CreateProject(ctx, "external-root-federation-hub")
		require.NoError(t, err)
		createProject, err := store.CreateProject(ctx, "external-root-federation-create")
		require.NoError(t, err)
		createIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: createProject.ID, Title: "Read-only binding target", Author: "tester",
		})
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: createProject.ID, Role: db.FederationRoleSpoke,
			HubURL: "https://hub.example", HubProjectID: hub.ID, HubProjectUID: hub.UID, Enabled: true,
		})
		require.NoError(t, err)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: createProject.ID, IssueID: createIssue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "read-only-create-root",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
		})
		assert.ErrorIs(t, err, db.ErrFederatedReadOnly)
		_, mappingErr := store.ImportMappingBySource(
			ctx, createProject.ID, "connector:notes", "issue", "read-only-create-root",
		)
		assert.ErrorIs(t, mappingErr, db.ErrNotFound)

		fixture := newExternalRootSafetyFixture(t, store, "federation-projection")
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: fixture.project.ID, Role: db.FederationRoleSpoke,
			HubURL: "https://hub.example", HubProjectID: hub.ID, HubProjectUID: hub.UID, Enabled: true,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootFederationConflict)
		_, readErr := store.FederationBindingByProject(ctx, fixture.project.ID)
		assert.ErrorIs(t, readErr, db.ErrNotFound)
		paused, _, err := store.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Actor: "tester", Reason: "operator_pause",
		})
		require.NoError(t, err)
		assert.False(t, paused.Enabled)
	})

	t.Run("bind-time comment revisions form an identity frontier", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-comment-frontier")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Comment frontier", Author: "tester",
		})
		require.NoError(t, err)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "comment-frontier-root",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter:       time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
			UseCommentIdentityFrontier: true,
			InitialCommentRevisions: []db.ExternalCommentRevision{
				{ExternalID: "provider-comment-one", Revision: "revision-one"},
				{ExternalID: "provider-comment-two", Revision: "revision-two"},
			},
		})
		require.NoError(t, err)
		mappings, err := store.ImportMappingsByProjectSource(
			ctx, project.ID, db.ExternalRootCommentRevisionMappingSource(binding),
		)
		require.NoError(t, err)
		require.Len(t, mappings, 3)
		actual := make(map[string]bool, len(mappings))
		for _, mapping := range mappings {
			assert.Equal(t, db.ExternalRevisionAnchorObjectType, mapping.ObjectType)
			require.NotNil(t, mapping.IssueID)
			assert.Equal(t, issue.ID, *mapping.IssueID)
			assert.Nil(t, mapping.CommentID)
			actual[mapping.ExternalID] = true
		}
		assert.True(t, actual[db.ExternalCommentFrontierExternalID])
		assert.True(t, actual[db.ExternalCommentRevisionMappingExternalID("provider-comment-one", "revision-one")])
		assert.True(t, actual[db.ExternalCommentRevisionMappingExternalID("provider-comment-two", "revision-two")])
	})

	t.Run("soft delete requires an external root to be unbound first", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "soft-delete")
		_, event, changed, err := store.SoftDeleteIssue(ctx, fixture.issue.ID, "tester")
		assert.ErrorIs(t, err, db.ErrExternalRootContentOwned)
		assert.False(t, changed)
		assert.Nil(t, event)
		issue, readErr := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Nil(t, issue.DeletedAt)
		binding, err := store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, err)
		assert.True(t, binding.Active)
		assert.True(t, binding.Enabled)
		assert.Equal(t, fixture.token, binding.ClaimToken)
		assert.Nil(t, binding.UnboundAt)
	})

	t.Run("a claimed closed external root fences reopening", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-reopen-fence")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Closed root", Author: "tester",
		})
		require.NoError(t, err)
		issue, _, _, err = store.CloseIssue(ctx, issue.ID, "done", "tester", "Verified complete.", nil)
		require.NoError(t, err)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-reopen-fence",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err)
		now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
		_, acquired, err := store.ClaimExternalRootBinding(ctx, binding.ID, "completion-claim", now, now.Add(-time.Minute))
		require.NoError(t, err)
		require.True(t, acquired)

		_, _, changed, err := store.ReopenIssue(ctx, issue.ID, "operator")
		assert.ErrorIs(t, err, db.ErrExternalRootClaimActive)
		assert.False(t, changed)
		retained, readErr := store.IssueByID(ctx, issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, "closed", retained.Status)

		_, err = store.ReleaseExternalRootClaim(ctx, binding.ID, "completion-claim")
		require.NoError(t, err)
		reopened, _, changed, err := store.ReopenIssue(ctx, issue.ID, "operator")
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, "open", reopened.Status)
	})

	t.Run("an external root cannot be rebound to a different issue", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-rebind-safety")
		require.NoError(t, err)
		first, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Original root issue", Author: "tester",
		})
		require.NoError(t, err)
		second, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Different root issue", Author: "tester",
		})
		require.NoError(t, err)
		at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: first.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-rebind-safety",
			ExternalAccountKey: "opaque-account-key", Actor: "tester", ReceiveCommentsAfter: at,
		})
		require.NoError(t, err)
		_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
			Source: "connector:notes", ExternalID: "root-rebind-safety", ObjectType: "issue",
			ProjectID: project.ID, IssueID: &second.ID,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootAlreadyBound)
		retainedAfterRetarget, readErr := store.ExternalRootBindingByID(ctx, binding.ID)
		require.NoError(t, readErr)
		assert.Equal(t, first.ID, retainedAfterRetarget.IssueID)
		_, _, err = store.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: binding.ID, Actor: "tester",
		})
		require.NoError(t, err)
		_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
			Source: "connector:notes", ExternalID: "root-rebind-safety", ObjectType: "issue",
			ProjectID: project.ID, IssueID: &second.ID,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootAlreadyBound)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: second.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-rebind-safety",
			ExternalAccountKey: "opaque-account-key", Actor: "tester", ReceiveCommentsAfter: at,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootAlreadyBound)
		otherProject, err := store.CreateProject(ctx, "external-root-rebind-other-project")
		require.NoError(t, err)
		otherIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: otherProject.ID, Title: "Cross-project root target", Author: "tester",
		})
		require.NoError(t, err)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: otherProject.ID, IssueID: otherIssue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-rebind-safety",
			ExternalAccountKey: "opaque-account-key", Actor: "tester", ReceiveCommentsAfter: at,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootAlreadyBound)
		mapping, readErr := store.ImportMappingBySource(
			ctx, project.ID, "connector:notes", "issue", "root-rebind-safety",
		)
		require.NoError(t, readErr)
		require.NotNil(t, mapping.IssueID)
		assert.Equal(t, first.ID, *mapping.IssueID, "a rejected rebind must preserve the restorable mapping")
		historical, readErr := store.ExternalRootBindingByID(ctx, binding.ID)
		require.NoError(t, readErr)
		assert.Equal(t, mapping.ID, historical.RootMappingID)
		assert.Equal(t, first.ID, historical.IssueID)

		records, exportErr := CollectImportRecords(ctx, store, db.ExportFilter{
			ProjectID: &project.ID, IncludeDeleted: true,
		})
		require.NoError(t, exportErr)
		restored := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, restored.Close()) })
		require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{}))
		var restoredBinding db.ExternalRootBindingExport
		for candidate, candidateErr := range restored.ExportExternalRootBindings(
			ctx, db.ExportFilter{IncludeDeleted: true},
		) {
			require.NoError(t, candidateErr)
			if candidate.UID == binding.UID {
				restoredBinding = candidate
			}
		}
		require.Equal(t, binding.UID, restoredBinding.UID)
		assert.Equal(t, first.UID, restoredBinding.IssueUID)
		restoredIssue, restoreErr := restored.IssueByUID(ctx, first.UID, db.IncludeDeletedNo)
		require.NoError(t, restoreErr)
		restoredProject, restoreErr := restored.ProjectByUID(ctx, project.UID)
		require.NoError(t, restoreErr)
		restoredMapping, restoreErr := restored.ImportMappingBySource(
			ctx, restoredProject.ID, "connector:notes", "issue", "root-rebind-safety",
		)
		require.NoError(t, restoreErr)
		require.NotNil(t, restoredMapping.IssueID)
		assert.Equal(t, restoredIssue.ID, *restoredMapping.IssueID)
	})

	t.Run("archiving pauses bridges and restoration requires explicit resume", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "archive-pause")
		pausedIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: fixture.project.ID, Title: "Already paused bridge", Author: "tester",
		})
		require.NoError(t, err)
		pausedBinding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: fixture.project.ID, IssueID: pausedIssue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-already-paused",
			ExternalAccountKey: "opaque-account-key", Actor: "tester", ReceiveCommentsAfter: fixture.now,
		})
		require.NoError(t, err)
		pausedBinding, _, err = store.PauseExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: pausedBinding.ID, Actor: "operator", Reason: "operator_pause",
		})
		require.NoError(t, err)
		archived, _, err := store.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: fixture.project.ID, Actor: "operator", Force: true,
		})
		require.NoError(t, err)
		require.NotNil(t, archived.DeletedAt)
		binding, err := store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, err)
		assert.False(t, binding.Enabled)
		assert.Equal(t, "project_archived", binding.PausedReason)
		assert.Empty(t, binding.ClaimToken)
		assert.Nil(t, binding.ClaimStartedAt)
		pausedBinding, err = store.ExternalRootBindingByID(ctx, pausedBinding.ID)
		require.NoError(t, err)
		assert.False(t, pausedBinding.Enabled)
		assert.Equal(t, "operator_pause", pausedBinding.PausedReason)

		_, _, changed, err := store.RestoreProject(ctx, fixture.project.ID, "operator")
		require.NoError(t, err)
		require.True(t, changed)
		binding, err = store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, err)
		assert.False(t, binding.Enabled)
		due, err := store.ListDueExternalRootBindings(ctx, fixture.now.Add(time.Hour), fixture.now, 10)
		require.NoError(t, err)
		for _, candidate := range due {
			assert.NotEqual(t, binding.ID, candidate.ID)
		}
		resumed, _, err := store.ResumeExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: binding.ID, Actor: "operator",
		})
		require.NoError(t, err)
		assert.True(t, resumed.Enabled)
		assert.Empty(t, resumed.PausedReason)
	})

	t.Run("archiving rejects a fresh external root claim", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "archive-live-claim")
		now := time.Now().UTC()
		_, err := store.RenewExternalRootClaim(ctx, fixture.binding.ID, fixture.token, now)
		require.NoError(t, err)

		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: fixture.project.ID, Actor: "operator", Force: true,
		})

		assert.ErrorIs(t, err, db.ErrExternalRootClaimActive)
		project, readErr := store.ProjectByID(ctx, fixture.project.ID)
		require.NoError(t, readErr)
		assert.Nil(t, project.DeletedAt)
		binding, readErr := store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, readErr)
		assert.True(t, binding.Enabled)
		assert.Equal(t, fixture.token, binding.ClaimToken)
		_, releaseErr := store.ReleaseExternalRootClaim(ctx, fixture.binding.ID, fixture.token)
		require.NoError(t, releaseErr)
	})

	t.Run("creation requires synchronization frontiers", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-safety-frontiers")
		require.NoError(t, err)
		inbound := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
		cases := []struct {
			name            string
			receiveAfter    time.Time
			publish         bool
			publishAfter    *time.Time
			localFrontier   bool
			initialClaim    string
			initialClaimAt  time.Time
			externalRootKey string
		}{
			{name: "missing inbound", externalRootKey: "root-missing-inbound"},
			{name: "missing outbound", receiveAfter: inbound, publish: true, externalRootKey: "root-missing-outbound"},
			{name: "ambiguous outbound", receiveAfter: inbound, publish: true, publishAfter: &inbound, localFrontier: true, externalRootKey: "root-ambiguous-outbound"},
			{name: "local frontier without publishing", receiveAfter: inbound, localFrontier: true, externalRootKey: "root-disabled-local-outbound"},
			{name: "claim token without timestamp", receiveAfter: inbound, initialClaim: "reserved-claim", externalRootKey: "root-claim-without-time"},
			{name: "claim timestamp without token", receiveAfter: inbound, initialClaimAt: inbound, externalRootKey: "root-claim-without-token"},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
					ProjectID: project.ID, Title: "Frontier validation", Author: "tester",
				})
				require.NoError(t, err)
				_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
					ProjectID: project.ID, IssueID: issue.ID,
					ConnectorInstance: "notes", ExternalRootKey: test.externalRootKey,
					ExternalAccountKey: "opaque-account-key", Actor: "tester",
					ReceiveCommentsAfter: test.receiveAfter, PublishComments: test.publish,
					PublishCommentsAfter: test.publishAfter, UseLocalPublishFrontier: test.localFrontier,
					InitialClaimToken: test.initialClaim, InitialClaimStartedAt: test.initialClaimAt,
				})
				assert.ErrorIs(t, err, db.ErrExternalRootValidation)
				_, mappingErr := store.ImportMappingBySource(
					ctx, project.ID, "connector:notes", "issue", test.externalRootKey,
				)
				assert.ErrorIs(t, mappingErr, db.ErrNotFound)
			})
		}

		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Explicit frontiers", Author: "tester",
		})
		require.NoError(t, err)
		outbound := inbound.Add(time.Hour)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-explicit-frontiers",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: inbound, PublishComments: true, PublishCommentsAfter: &outbound,
		})
		require.NoError(t, err)
		assert.Equal(t, &inbound, binding.ReceiveCommentsAfter)
		assert.Equal(t, &outbound, binding.PublishCommentsAfter)

		localIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Local publish frontier", Author: "tester",
		})
		require.NoError(t, err)
		oldComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localIssue.ID, Author: "tester", Body: "Existing local comment",
		})
		require.NoError(t, err)
		require.NotNil(t, backend.InstallExternalRootClock)
		restoreClock := backend.InstallExternalRootClock(store, func() time.Time {
			return oldComment.CreatedAt
		})
		t.Cleanup(restoreClock)
		localBinding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: localIssue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-local-frontier",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: inbound, PublishComments: true, UseLocalPublishFrontier: true,
		})
		require.NoError(t, err)
		require.NotNil(t, localBinding.PublishCommentsAfter)
		assert.True(t, localBinding.PublishCommentsAfter.IsZero(),
			"local publish frontier is marker-governed and records the zero time")
		markers, err := store.ImportCommentMappingsByIssue(ctx, localIssue.ID)
		require.NoError(t, err)
		marked := false
		for _, mapping := range markers {
			if mapping.CommentID != nil && *mapping.CommentID == oldComment.ID &&
				mapping.Source == db.ExternalRootSkippedCommentMappingSource("notes") {
				marked = true
			}
		}
		assert.True(t, marked, "pre-binding local comment must carry a durable skip marker")

		// A comment committed after the binding must carry no skip marker even
		// when its timestamp does not exceed the pre-binding comment's, so it
		// stays eligible for outbound publication.
		require.NotNil(t, backend.BackdateCommentCreated)
		postBinding, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: localIssue.ID, Author: "tester", Body: "Committed after binding",
		})
		require.NoError(t, err)
		require.NoError(t, backend.BackdateCommentCreated(ctx, store, postBinding.ID, oldComment.CreatedAt.Add(-time.Hour)))
		markers, err = store.ImportCommentMappingsByIssue(ctx, localIssue.ID)
		require.NoError(t, err)
		for _, mapping := range markers {
			if mapping.CommentID != nil && *mapping.CommentID == postBinding.ID {
				t.Fatalf("post-binding comment %d must not be marked skipped: %+v", postBinding.ID, mapping)
			}
		}
	})

	t.Run("creation reserves an initial claim atomically", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-initial-claim")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "Initial claim", Author: "tester",
		})
		require.NoError(t, err)
		claimedAt := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: project.ID, IssueID: issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-initial-claim",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: claimedAt,
			InitialClaimToken:    "reserved-claim", InitialClaimStartedAt: claimedAt,
		})
		require.NoError(t, err)
		assert.Equal(t, "reserved-claim", binding.ClaimToken)
		require.NotNil(t, binding.ClaimStartedAt)
		assert.Equal(t, claimedAt, *binding.ClaimStartedAt)

		competing, acquired, err := store.ClaimExternalRootBinding(
			ctx, binding.ID, "competing-claim", claimedAt.Add(time.Second), claimedAt.Add(-time.Minute),
		)
		require.NoError(t, err)
		assert.False(t, acquired)
		assert.Equal(t, "reserved-claim", competing.ClaimToken)
		assert.Equal(t, &claimedAt, competing.ClaimStartedAt)
	})

	t.Run("pending comment identity and action contract", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "comments")
		comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: fixture.issue.ID, Author: "tester", Body: "Pending comment",
		})
		require.NoError(t, err)
		otherComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: fixture.issue.ID, Author: "tester", Body: "Other comment",
		})
		require.NoError(t, err)
		otherIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: fixture.project.ID, Title: "Other issue", Author: "tester",
		})
		require.NoError(t, err)
		wrongIssueComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: otherIssue.ID, Author: "tester", Body: "Wrong issue comment",
		})
		require.NoError(t, err)

		setPending := func(commentUID string) error {
			_, err := store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
				BindingID: fixture.binding.ID, ClaimToken: fixture.token,
				CommentUID: commentUID, At: fixture.now.Add(time.Minute),
			})
			return err
		}
		clearPending := func(action string, mapping *db.ImportMappingParams) error {
			externalRevision := ""
			if action == "published" || action == "adopt" {
				externalRevision = "pending-external-revision"
			}
			_, _, err := store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
				BindingID: fixture.binding.ID, ClaimToken: fixture.token, CommentUID: comment.UID,
				ExpectedBody: comment.Body, Action: action, Actor: "tester", At: fixture.now.Add(2 * time.Minute), Mapping: mapping,
				ExternalRevision: externalRevision,
			})
			return err
		}
		assertRejectedSet := func(commentUID string) {
			t.Helper()
			err := setPending(commentUID)
			assert.ErrorIs(t, err, db.ErrNotFound)
			if err == nil {
				_, _, cleanupErr := store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
					BindingID: fixture.binding.ID, ClaimToken: fixture.token, CommentUID: commentUID,
					Action: "skip", Actor: "tester", At: fixture.now.Add(2 * time.Minute),
				})
				require.NoError(t, cleanupErr)
			}
		}
		assertRejectedClear := func(action string, mapping *db.ImportMappingParams) {
			t.Helper()
			err := clearPending(action, mapping)
			assert.ErrorIs(t, err, db.ErrExternalRootValidation)
			if err == nil {
				require.NoError(t, setPending(comment.UID))
			}
		}

		assertRejectedSet("missing-comment-uid")
		assertRejectedSet(wrongIssueComment.UID)
		require.NoError(t, setPending(comment.UID))

		assertRejectedClear("published", nil)
		assertRejectedClear("adopt", nil)
		issueID := fixture.issue.ID
		commentID := comment.ID
		exactMapping := func(externalID string) *db.ImportMappingParams {
			return &db.ImportMappingParams{
				Source: db.ExternalRootPublishedCommentMappingSource(fixture.binding), ExternalID: externalID, ObjectType: "comment",
				ProjectID: fixture.project.ID, IssueID: &issueID, CommentID: &commentID,
			}
		}
		assertRejectedClear("retry", exactMapping("retry-comment"))
		assertRejectedClear("skip", exactMapping("skip-comment"))
		assertRejectedClear("skip", nil)
		wrongDirection := exactMapping("wrong-direction-comment")
		wrongDirection.Source = db.ExternalRootCommentMappingSource(fixture.binding)
		assertRejectedClear("published", wrongDirection)
		otherCommentID := otherComment.ID
		wrongMapping := exactMapping("wrong-comment")
		wrongMapping.CommentID = &otherCommentID
		assertRejectedClear("published", wrongMapping)
		retryAt := fixture.now.Add(2 * time.Minute)
		retried, event, err := store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token, CommentUID: comment.UID,
			Action: "retry", Actor: "tester", At: retryAt,
		})
		require.NoError(t, err)
		assert.Equal(t, comment.UID, retried.PendingCommentUID)
		require.NotNil(t, retried.PendingCommentStartedAt)
		assert.Equal(t, retryAt, *retried.PendingCommentStartedAt)
		assert.Equal(t, "issue.external_comment_resolved", event.Type)
		require.NoError(t, clearPending("published", exactMapping("published-comment")))
	})

	t.Run("pending skip writes durable suppression mapping atomically", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "comment-skip")
		comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: fixture.issue.ID, Author: "tester", Body: "Skip this comment",
		})
		require.NoError(t, err)
		_, err = store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			CommentUID: comment.UID, At: fixture.now.Add(time.Minute),
		})
		require.NoError(t, err)
		issueID, commentID := fixture.issue.ID, comment.ID
		suppression := db.ImportMappingParams{
			Source: "connector-skip:notes", ExternalID: comment.UID, ObjectType: "comment",
			ProjectID: fixture.project.ID, IssueID: &issueID, CommentID: &commentID,
		}

		cleared, event, err := store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token, CommentUID: comment.UID,
			Action: "skip", Actor: "tester", At: fixture.now.Add(2 * time.Minute), Mapping: &suppression,
		})
		require.NoError(t, err)
		assert.Empty(t, cleared.PendingCommentUID)
		assert.Nil(t, cleared.PendingCommentStartedAt)
		assert.Equal(t, "issue.external_comment_resolved", event.Type)
		stored, err := store.ImportMappingBySource(
			ctx, fixture.project.ID, "connector-skip:notes", "comment", comment.UID,
		)
		require.NoError(t, err)
		require.NotNil(t, stored.CommentID)
		assert.Equal(t, comment.ID, *stored.CommentID)
	})

	t.Run("pending adopt does not steal an existing external mapping", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "comment-adopt-conflict")
		pending, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: fixture.issue.ID, Author: "tester", Body: "Pending adoption",
		})
		require.NoError(t, err)
		other, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: fixture.issue.ID, Author: "tester", Body: "Already mapped",
		})
		require.NoError(t, err)
		_, err = store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			CommentUID: pending.UID, At: fixture.now.Add(time.Minute),
		})
		require.NoError(t, err)
		issueID, otherID := fixture.issue.ID, other.ID
		_, err = store.UpsertImportMapping(ctx, db.ImportMappingParams{
			Source: db.ExternalRootCommentMappingSource(fixture.binding), ExternalID: "already-mapped-comment", ObjectType: "comment",
			ProjectID: fixture.project.ID, IssueID: &issueID, CommentID: &otherID,
		})
		require.NoError(t, err)
		pendingID := pending.ID
		adoption := db.ImportMappingParams{
			Source: db.ExternalRootPublishedCommentMappingSource(fixture.binding), ExternalID: "already-mapped-comment", ObjectType: "comment",
			ProjectID: fixture.project.ID, IssueID: &issueID, CommentID: &pendingID,
		}

		_, _, err = store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token, CommentUID: pending.UID,
			ExpectedBody: pending.Body, Action: "adopt", Actor: "tester", At: fixture.now.Add(2 * time.Minute), Mapping: &adoption,
			ExternalRevision: "adopted-external-revision",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		retained, readErr := store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, readErr)
		assert.Equal(t, pending.UID, retained.PendingCommentUID)
		mapping, readErr := store.ImportMappingBySource(
			ctx, fixture.project.ID, db.ExternalRootCommentMappingSource(fixture.binding), "comment", "already-mapped-comment",
		)
		require.NoError(t, readErr)
		require.NotNil(t, mapping.CommentID)
		assert.Equal(t, other.ID, *mapping.CommentID)
	})

	t.Run("unbind preserves pending uncertainty until resolution", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "unbind")
		comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: fixture.issue.ID, Author: "tester", Body: "Pending comment",
		})
		require.NoError(t, err)
		_, err = store.SetPendingExternalComment(ctx, db.SetPendingExternalCommentParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			CommentUID: comment.UID, At: fixture.now.Add(time.Minute),
		})
		require.NoError(t, err)

		_, _, unbindErr := store.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: fixture.binding.ID, Actor: "tester",
		})
		assert.ErrorIs(t, unbindErr, db.ErrExternalCommentPending)
		_, _, rebindErr := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: fixture.project.ID, IssueID: fixture.issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-unbind-replacement",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: fixture.now,
		})
		assert.ErrorIs(t, rebindErr, db.ErrExternalRootIssueAlreadyBound)
		if unbindErr == nil {
			return
		}

		issueID, commentID := fixture.issue.ID, comment.ID
		suppression := db.ImportMappingParams{
			Source: "connector-skip:notes", ExternalID: comment.UID, ObjectType: "comment",
			ProjectID: fixture.project.ID, IssueID: &issueID, CommentID: &commentID,
		}
		_, _, err = store.ClearPendingExternalComment(ctx, db.ClearPendingExternalCommentParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token, CommentUID: comment.UID,
			Action: "skip", Actor: "tester", At: fixture.now.Add(2 * time.Minute), Mapping: &suppression,
		})
		require.NoError(t, err)
		_, _, err = store.UnbindExternalRootBinding(ctx, db.ExternalRootActionParams{
			BindingID: fixture.binding.ID, Actor: "tester",
		})
		require.NoError(t, err)
		_, _, err = store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: fixture.project.ID, IssueID: fixture.issue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "root-unbind",
			ExternalAccountKey: "opaque-account-key", Actor: "tester",
			ReceiveCommentsAfter: fixture.now,
		})
		require.NoError(t, err)
	})

	t.Run("release requires a nonempty claim token", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "release")
		_, err := store.ReleaseExternalRootClaim(ctx, fixture.binding.ID, "   ")
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		_, err = store.ReleaseExternalRootClaim(ctx, fixture.binding.ID, fixture.token)
		require.NoError(t, err)
		_, err = store.ReleaseExternalRootClaim(ctx, fixture.binding.ID, "")
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)
	})

	t.Run("checkpoint validation preserves the active claim", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			suffix string
			run    func(externalRootSafetyFixture) error
		}{
			{
				name:   "success requires external state",
				suffix: "success-state",
				run: func(fixture externalRootSafetyFixture) error {
					_, err := store.RecordExternalRootSuccess(ctx, db.ExternalRootSuccessParams{
						BindingID: fixture.binding.ID, ClaimToken: fixture.token,
						At: fixture.now, NextAttemptAt: fixture.now.Add(time.Minute),
						ExternalRevision: "revision-1",
					})
					return err
				},
			},
			{
				name:   "success requires external revision",
				suffix: "success-revision",
				run: func(fixture externalRootSafetyFixture) error {
					_, err := store.RecordExternalRootSuccess(ctx, db.ExternalRootSuccessParams{
						BindingID: fixture.binding.ID, ClaimToken: fixture.token,
						At: fixture.now, NextAttemptAt: fixture.now.Add(time.Minute),
						ExternalState: "open",
					})
					return err
				},
			},
			{
				name:   "success rejects unknown external state",
				suffix: "success-state-enum",
				run: func(fixture externalRootSafetyFixture) error {
					_, err := store.RecordExternalRootSuccess(ctx, db.ExternalRootSuccessParams{
						BindingID: fixture.binding.ID, ClaimToken: fixture.token,
						At: fixture.now, NextAttemptAt: fixture.now.Add(time.Minute),
						ExternalState: "pending", ExternalRevision: "revision-1",
					})
					return err
				},
			},
			{
				name:   "error rejects unknown external state",
				suffix: "error-state-enum",
				run: func(fixture externalRootSafetyFixture) error {
					_, err := store.RecordExternalRootError(ctx, db.ExternalRootErrorParams{
						BindingID: fixture.binding.ID, ClaimToken: fixture.token,
						At: fixture.now, NextAttemptAt: fixture.now.Add(time.Minute), Error: "request failed",
						ExternalState: "pending", ExternalRevision: "revision-1",
					})
					return err
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newExternalRootSafetyFixture(t, store, "checkpoint-"+test.suffix)
				err := test.run(fixture)
				assert.ErrorIs(t, err, db.ErrExternalRootValidation)
				persisted, readErr := store.ExternalRootBindingByID(ctx, fixture.binding.ID)
				require.NoError(t, readErr)
				assert.Equal(t, fixture.token, persisted.ClaimToken)
				assert.Empty(t, persisted.LastExternalState)
				assert.Empty(t, persisted.LastExternalRevision)
			})
		}
	})

	t.Run("field projection is native idempotent and claim guarded", func(t *testing.T) {
		mapping, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
			ConnectorInstance: "notes", KataField: "scheduled_on",
			ExternalFieldID: "start-projection", ExternalFieldName: "Start",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-projection",
		})
		require.NoError(t, err)
		fixture := newExternalRootSafetyFixture(t, store, "field-projection")
		patch := map[string]json.RawMessage{
			"scheduled_on": json.RawMessage(`"2026-08-21T09:30"`),
			"timezone":     json.RawMessage(`"America/Los_Angeles"`),
		}
		nativeIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: fixture.project.ID, Title: "Native metadata parity", Author: "tester",
		})
		require.NoError(t, err)
		native, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: nativeIssue.ID, Actor: "connector:notes", Patch: patch,
		})
		require.NoError(t, err)
		params := db.ExternalFieldProjectionParams{
			BindingID: fixture.binding.ID, MappingID: mapping.ID, ClaimToken: fixture.token,
			KataField: "scheduled_on", IntegrationActor: "connector:notes",
			Patch: patch, ExpectedIssueRevision: fixture.issue.Revision,
		}
		projected, event, changed, err := store.ApplyExternalFieldProjection(ctx, params)
		require.NoError(t, err)
		assert.True(t, changed)
		require.NotNil(t, event)
		assert.Equal(t, "issue.metadata_updated", event.Type)
		assert.Equal(t, "connector:notes", event.Actor)
		var metadata map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(projected.Metadata), &metadata))
		assert.JSONEq(t, `"2026-08-21T09:30"`, string(metadata["scheduled_on"]))
		assert.JSONEq(t, `"America/Los_Angeles"`, string(metadata["timezone"]))
		nativePayload := requireCanonicalMetadataUpdatePayload(t, native.Event, native.Issue)
		externalPayload := requireCanonicalMetadataUpdatePayload(t, *event, projected)
		nativePayload.RevisionNew, externalPayload.RevisionNew = 0, 0
		nativePayload.UpdatedAt = ""
		externalPayload.UpdatedAt = ""
		assert.Equal(t, nativePayload, externalPayload)

		params.ExpectedIssueRevision = projected.Revision
		repeated, repeatedEvent, repeatedChanged, err := store.ApplyExternalFieldProjection(ctx, params)
		require.NoError(t, err)
		assert.False(t, repeatedChanged)
		assert.Nil(t, repeatedEvent)
		assert.Equal(t, projected.Revision, repeated.Revision)

		params.ClaimToken = "wrong-claim"
		params.Patch["scheduled_on"] = json.RawMessage(`"2026-08-22"`)
		_, _, _, err = store.ApplyExternalFieldProjection(ctx, params)
		assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
		afterRejected, readErr := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, projected.Revision, afterRejected.Revision)
		assert.JSONEq(t, string(projected.Metadata), string(afterRejected.Metadata))
		afterEvents, readErr := store.EventsAfter(ctx, db.EventsAfterParams{
			AfterID: event.ID, ProjectID: fixture.project.ID, Limit: 10,
		})
		require.NoError(t, readErr)
		assert.Empty(t, afterEvents)

		params.ClaimToken = fixture.token
		params.ExpectedIssueRevision = projected.Revision
		local, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
			IssueID: fixture.issue.ID, Actor: "tester", Patch: map[string]json.RawMessage{
				"scheduled_on": json.RawMessage(`"2026-08-23"`),
			},
		})
		require.NoError(t, err)
		params.Patch["scheduled_on"] = json.RawMessage(`"2026-08-24"`)
		_, _, _, err = store.ApplyExternalFieldProjection(ctx, params)
		var revisionConflict *db.RevisionConflictError
		assert.ErrorAs(t, err, &revisionConflict)
		if err == nil {
			return
		}
		assert.Equal(t, local.Issue.Revision, revisionConflict.CurrentRevision)
		afterStale, readErr := store.IssueByUID(ctx, fixture.issue.UID, db.IncludeDeletedYes)
		require.NoError(t, readErr)
		assert.Equal(t, local.Issue.Revision, afterStale.Revision)
		assert.JSONEq(t, string(local.Issue.Metadata), string(afterStale.Metadata))
	})

	t.Run("archived projects reject claimed root and field projections", func(t *testing.T) {
		mapping, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
			ConnectorInstance: "notes", KataField: "scheduled_on",
			ExternalFieldID: "start-archived", ExternalFieldName: "Start",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-archived",
		})
		require.NoError(t, err)
		fixture := newExternalRootSafetyFixture(t, store, "archived-projection")
		before, err := store.IssueByUID(ctx, fixture.issue.UID, db.IncludeDeletedYes)
		require.NoError(t, err)
		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: fixture.project.ID, Actor: "tester", Force: true,
		})
		require.NoError(t, err)

		_, _, _, err = store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Archived overwrite", Body: "Archived overwrite",
			ExternalRevision: "archived-revision", ExternalUpdatedAt: fixture.now,
			ExternalObservedAt: fixture.now, IntegrationActor: "connector:notes",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
		_, _, _, err = store.ApplyExternalFieldProjection(ctx, db.ExternalFieldProjectionParams{
			BindingID: fixture.binding.ID, MappingID: mapping.ID, ClaimToken: fixture.token,
			KataField: "scheduled_on", ExpectedIssueRevision: before.Revision,
			Patch:            map[string]json.RawMessage{"scheduled_on": json.RawMessage(`"2026-08-24"`)},
			IntegrationActor: "connector:notes",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)

		after, readErr := store.IssueByUID(ctx, fixture.issue.UID, db.IncludeDeletedYes)
		require.NoError(t, readErr)
		assert.Equal(t, before.Title, after.Title)
		assert.Equal(t, before.Body, after.Body)
		assert.Equal(t, before.Revision, after.Revision)
		assert.JSONEq(t, string(before.Metadata), string(after.Metadata))
	})

	t.Run("field conflict requires explicit resolution", func(t *testing.T) {
		mapping, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
			ConnectorInstance: "notes", KataField: "scheduled_on",
			ExternalFieldID: "start-safety", ExternalFieldName: "Start",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-safety",
		})
		require.NoError(t, err)
		fixture := newExternalRootSafetyFixture(t, store, "field-conflict")
		_, _, err = store.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
			BindingID: fixture.binding.ID, MappingID: mapping.ID, ClaimToken: fixture.token,
			Baseline:     json.RawMessage(`"2026-08-20"`),
			ConflictKata: json.RawMessage(`"2026-08-21"`), ConflictExternal: json.RawMessage(`"2026-08-22"`),
			Conflicted: true, At: fixture.now.Add(time.Minute), Actor: "tester",
		})
		require.NoError(t, err)
		_, _, clearErr := store.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
			BindingID: fixture.binding.ID, MappingID: mapping.ID, ClaimToken: fixture.token,
			Baseline:   json.RawMessage(`"2026-08-23"`),
			Conflicted: false, At: fixture.now.Add(2 * time.Minute), Actor: "tester",
		})
		assert.ErrorIs(t, clearErr, db.ErrExternalRootValidation)
		states, err := store.ExternalFieldStates(ctx, fixture.binding.ID)
		require.NoError(t, err)
		require.Len(t, states, 1)
		assert.True(t, states[0].Conflicted)
		if clearErr == nil {
			return
		}
		resolved, event, err := store.ResolveExternalFieldConflict(ctx, db.ResolveExternalFieldConflictParams{
			BindingID: fixture.binding.ID, MappingID: mapping.ID, ClaimToken: fixture.token,
			Baseline: json.RawMessage(`"2026-08-23"`), Actor: "tester", At: fixture.now.Add(3 * time.Minute),
		})
		require.NoError(t, err)
		assert.False(t, resolved.Conflicted)
		assert.Equal(t, "issue.external_field_resolved", event.Type)
	})

	t.Run("inbound projections are native idempotent and claim guarded", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "inbound-projections")
		integrationActor := "connector:notes"
		providerCreatedAt := fixture.now.Add(time.Minute + 123456*time.Nanosecond)
		providerUpdatedAt := fixture.now.Add(2 * time.Minute)
		for _, invalidTitle := range []string{" \t\n", "external\x00title"} {
			_, _, _, err := store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
				BindingID: fixture.binding.ID, ClaimToken: fixture.token,
				Title: invalidTitle, Body: "must not be stored", ExternalRevision: "invalid-title",
				ExternalUpdatedAt: providerCreatedAt, ExternalObservedAt: providerCreatedAt,
				IntegrationActor: integrationActor,
			})
			assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		}
		unchanged, err := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, err)
		assert.Equal(t, fixture.issue.Title, unchanged.Title)

		title, body := "External projection", "External root body"
		projected, rootEvent, changed, err := store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: title, Body: body, ExternalRevision: "root-revision-2",
			ExternalActorID: "actor-1", ExternalActorName: "Reviewer",
			ExternalUpdatedAt: providerUpdatedAt, ExternalObservedAt: providerUpdatedAt,
			IntegrationActor: integrationActor,
		})
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, title, projected.Title)
		assert.Equal(t, body, projected.Body)
		require.NotNil(t, rootEvent)
		assert.Equal(t, "issue.updated", rootEvent.Type)
		assert.Equal(t, integrationActor, rootEvent.Actor)

		staleTitle := "Stale external projection"
		_, _, _, err = store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: staleTitle, Body: "Stale external root body", ExternalRevision: "root-revision-1",
			ExternalUpdatedAt: providerCreatedAt, ExternalObservedAt: providerUpdatedAt.Add(time.Minute),
			IntegrationActor: integrationActor,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		afterStale, readErr := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, title, afterStale.Title)
		rootMapping, readErr := store.ImportMappingBySource(
			ctx, fixture.project.ID, "connector:notes", "issue", fixture.binding.ExternalRootKey,
		)
		require.NoError(t, readErr)
		require.NotNil(t, rootMapping.SourceUpdatedAt)
		assert.True(t, rootMapping.SourceUpdatedAt.Equal(providerUpdatedAt))
		equalTimestampRoot, equalTimestampRootEvent, equalTimestampRootChanged, err := store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Same-time revised root", Body: body, ExternalRevision: "root-revision-3",
			ExternalUpdatedAt: providerUpdatedAt, ExternalObservedAt: providerUpdatedAt.Add(time.Minute),
			IntegrationActor: integrationActor,
		})
		require.NoError(t, err)
		assert.True(t, equalTimestampRootChanged)
		require.NotNil(t, equalTimestampRootEvent)
		assert.Equal(t, "Same-time revised root", equalTimestampRoot.Title)
		afterEqualTimestamp, readErr := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, "Same-time revised root", afterEqualTimestamp.Title)
		_, _, _, err = store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Same-revision corruption", Body: body, ExternalRevision: "root-revision-3",
			ExternalUpdatedAt: providerUpdatedAt.Add(time.Minute), ExternalObservedAt: providerUpdatedAt.Add(2 * time.Minute),
			IntegrationActor: integrationActor,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)

		commentParams := db.ExternalCommentProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			ExternalID: "comment-1", ExternalRevision: providerCreatedAt.Format(time.RFC3339Nano),
			Body: "External comment", ExternalActorID: "actor-2", ExternalActorName: "Contributor",
			ExternalCreatedAt: providerCreatedAt, ExternalUpdatedAt: providerCreatedAt,
			IntegrationActor: integrationActor,
		}
		comment, commentEvent, created, err := store.UpsertExternalCommentProjection(ctx, commentParams)
		require.NoError(t, err)
		assert.True(t, created)
		assert.Equal(t, integrationActor, comment.Author)
		assert.Equal(t, providerCreatedAt, comment.CreatedAt)
		require.NotNil(t, commentEvent)
		assert.Equal(t, "issue.commented", commentEvent.Type)
		assert.Equal(t, integrationActor, commentEvent.Actor)
		var commentPayload struct {
			Author string `json:"author"`
			Source struct {
				ActorID   string `json:"actor_id"`
				ActorName string `json:"actor_name"`
			} `json:"source"`
		}
		require.NoError(t, json.Unmarshal([]byte(commentEvent.Payload), &commentPayload))
		assert.Equal(t, integrationActor, commentPayload.Author)
		assert.Equal(t, commentEvent.Actor, commentPayload.Author)
		assert.Equal(t, "actor-2", commentPayload.Source.ActorID)
		assert.Equal(t, "Contributor", commentPayload.Source.ActorName)

		duplicate, duplicateEvent, duplicateChanged, err := store.UpsertExternalCommentProjection(ctx, commentParams)
		require.NoError(t, err)
		assert.False(t, duplicateChanged)
		assert.Nil(t, duplicateEvent)
		assert.Equal(t, comment.ID, duplicate.ID)
		sameTimeConflict := commentParams
		sameTimeConflict.Body = "Same-time revised comment"
		sameTimeConflict.ExternalRevision = "different-revision"
		sameTimeEdited, sameTimeEvent, sameTimeChanged, err := store.UpsertExternalCommentProjection(ctx, sameTimeConflict)
		require.NoError(t, err)
		assert.True(t, sameTimeChanged)
		require.NotNil(t, sameTimeEvent)
		assert.Equal(t, "Same-time revised comment", sameTimeEdited.Body)
		changedComment, readErr := store.CommentsByIssue(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		require.Len(t, changedComment, 1)
		assert.Equal(t, "Same-time revised comment", changedComment[0].Body)
		sameRevisionConflict := sameTimeConflict
		sameRevisionConflict.Body = "Same-revision corruption"
		sameRevisionConflict.ExternalUpdatedAt = providerUpdatedAt.Add(time.Minute)
		_, _, _, err = store.UpsertExternalCommentProjection(ctx, sameRevisionConflict)
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)

		commentParams.Body = "Corrected external comment"
		commentParams.ExternalUpdatedAt = providerUpdatedAt
		commentParams.ExternalRevision = "third-revision"
		edited, editEvent, editChanged, err := store.UpsertExternalCommentProjection(ctx, commentParams)
		require.NoError(t, err)
		assert.True(t, editChanged)
		assert.Equal(t, "Corrected external comment", edited.Body)
		require.NotNil(t, editEvent)
		assert.Equal(t, "issue.comment_edited", editEvent.Type)

		_, _, _, err = store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: "wrong-claim",
			Title: "Rejected title", Body: body, ExternalRevision: "root-revision-3",
			ExternalUpdatedAt: providerUpdatedAt, ExternalObservedAt: providerUpdatedAt,
			IntegrationActor: integrationActor,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
		commentParams.ClaimToken = "wrong-claim"
		commentParams.ExternalID = "rejected-comment"
		_, _, _, err = store.UpsertExternalCommentProjection(ctx, commentParams)
		assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
		_, _, _, err = store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Rejected actor", Body: body, ExternalRevision: "root-revision-4",
			ExternalUpdatedAt: providerUpdatedAt, ExternalObservedAt: providerUpdatedAt,
			IntegrationActor: "connector:different-instance",
		})
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)

		afterRejected, err := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, err)
		assert.Equal(t, "Same-time revised root", afterRejected.Title)
		_, err = store.ImportMappingBySource(
			ctx, fixture.project.ID, db.ExternalRootCommentMappingSource(fixture.binding), "comment", "rejected-comment",
		)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("inbound projection frontiers preserve sub-millisecond precision", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "inbound-sub-millisecond-frontier")
		integrationActor := "connector:notes"
		newer := time.Date(2026, 8, 20, 10, 0, 0, 123900000, time.UTC)
		older := newer.Add(-100 * time.Microsecond)

		_, _, changed, err := store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Newer root", Body: "Newer root body", ExternalRevision: "root-newer",
			ExternalUpdatedAt: newer, ExternalObservedAt: newer,
			IntegrationActor: integrationActor,
		})
		require.NoError(t, err)
		require.True(t, changed)
		_, _, _, err = store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			Title: "Older root", Body: "Older root body", ExternalRevision: "root-older",
			ExternalUpdatedAt: older, ExternalObservedAt: newer.Add(time.Second),
			IntegrationActor: integrationActor,
		})
		assert.ErrorIs(t, err, db.ErrExternalRootValidation)
		root, readErr := store.IssueByID(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		assert.Equal(t, "Newer root", root.Title)

		commentCreated := newer.Add(-100 * time.Microsecond)
		commentParams := db.ExternalCommentProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			ExternalID: "sub-millisecond-comment", ExternalRevision: "comment-newer",
			Body: "Newer comment", ExternalActorID: "actor", ExternalActorName: "Contributor",
			ExternalCreatedAt: commentCreated, ExternalUpdatedAt: newer,
			IntegrationActor: integrationActor,
		}
		_, _, created, err := store.UpsertExternalCommentProjection(ctx, commentParams)
		require.NoError(t, err)
		require.True(t, created)
		commentParams.Body = "Older comment"
		commentParams.ExternalRevision = "comment-older"
		commentParams.ExternalUpdatedAt = newer.Add(-50 * time.Microsecond)
		_, staleEvent, staleChanged, err := store.UpsertExternalCommentProjection(ctx, commentParams)
		require.NoError(t, err)
		assert.False(t, staleChanged)
		assert.Nil(t, staleEvent)
		comments, readErr := store.CommentsByIssue(ctx, fixture.issue.ID)
		require.NoError(t, readErr)
		require.Len(t, comments, 1)
		assert.Equal(t, "Newer comment", comments[0].Body)
	})

	t.Run("comment projections reject mappings to another issue's comment", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "cross-issue-comment-mapping")
		other, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: fixture.project.ID, Title: "Other issue", Author: "tester",
		})
		require.NoError(t, err)
		foreign, _, err := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: other.ID, Author: "tester", Body: "Foreign comment body",
		})
		require.NoError(t, err)

		for _, test := range []struct {
			name       string
			externalID string
			lifecycle  bool
		}{
			{name: "ordinary comment", externalID: "cross-issue-comment"},
			{name: "lifecycle request", externalID: "lifecycle:cross-issue", lifecycle: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				issueID, commentID := fixture.issue.ID, foreign.ID
				source := db.ExternalRootCommentMappingSource(fixture.binding)
				if test.lifecycle {
					source = db.ExternalRootLifecycleMappingSource(fixture.binding)
				}
				_, err := store.UpsertImportMapping(ctx, db.ImportMappingParams{
					Source:     source,
					ExternalID: test.externalID, ObjectType: "comment",
					ProjectID: fixture.project.ID, IssueID: &issueID, CommentID: &commentID,
				})
				require.NoError(t, err)
				params := db.ExternalCommentProjectionParams{
					BindingID: fixture.binding.ID, ClaimToken: fixture.token,
					ExternalID: test.externalID, ExternalRevision: "revision-two",
					Body: "Must not overwrite another issue", LifecycleState: "complete",
					ExternalCreatedAt: fixture.now.Add(time.Minute),
					ExternalUpdatedAt: fixture.now.Add(time.Minute),
					IntegrationActor:  "connector:notes",
				}
				if test.lifecycle {
					_, _, _, err = store.EnsureExternalRootLifecycleRequest(ctx, params)
				} else {
					_, _, _, err = store.UpsertExternalCommentProjection(ctx, params)
				}
				assert.ErrorIs(t, err, db.ErrExternalRootValidation)
			})
		}
		comments, err := store.CommentsByIssue(ctx, other.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		assert.Equal(t, "Foreign comment body", comments[0].Body)
	})

	t.Run("comment identities are scoped to their external root binding", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "external-root-comment-identity")
		require.NoError(t, err)
		observedAt := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
		type projectedComment struct {
			binding db.ExternalRootBinding
			comment db.Comment
		}
		projected := make([]projectedComment, 0, 2)
		for index := 1; index <= 2; index++ {
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
				ProjectID: project.ID, Title: fmt.Sprintf("Bound root %d", index), Author: "tester",
			})
			require.NoError(t, err)
			binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
				ProjectID: project.ID, IssueID: issue.ID,
				ConnectorInstance: "notes", ExternalRootKey: fmt.Sprintf("root-%d", index),
				ExternalAccountKey: "opaque-account-key", Actor: "tester",
				ReceiveCommentsAfter: observedAt.Add(-time.Hour),
			})
			require.NoError(t, err)
			claimToken := fmt.Sprintf("comment-scope-claim-%d", index)
			binding, claimed, err := store.ClaimExternalRootBinding(
				ctx, binding.ID, claimToken, observedAt, observedAt.Add(-time.Minute),
			)
			require.NoError(t, err)
			require.True(t, claimed)
			comment, event, created, err := store.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{
				BindingID: binding.ID, ClaimToken: claimToken,
				ExternalID: "root-local-comment-id", ExternalRevision: fmt.Sprintf("revision-%d", index),
				Body:            fmt.Sprintf("Comment from root %d", index),
				ExternalActorID: "actor", ExternalActorName: "Contributor",
				ExternalCreatedAt: observedAt, ExternalUpdatedAt: observedAt,
				IntegrationActor: "connector:notes",
			})
			require.NoError(t, err)
			assert.True(t, created)
			require.NotNil(t, event)
			projected = append(projected, projectedComment{binding: binding, comment: comment})
		}

		require.Len(t, projected, 2)
		assert.NotEqual(t, projected[0].comment.ID, projected[1].comment.ID)
		for _, item := range projected {
			mapping, err := store.ImportMappingBySource(
				ctx, project.ID, db.ExternalRootCommentMappingSource(item.binding),
				"comment", "root-local-comment-id",
			)
			require.NoError(t, err)
			require.NotNil(t, mapping.IssueID)
			require.NotNil(t, mapping.CommentID)
			assert.Equal(t, item.binding.IssueID, *mapping.IssueID)
			assert.Equal(t, item.comment.ID, *mapping.CommentID)
		}
	})

	t.Run("push-enabled spoke projects external comments with bound actor", func(t *testing.T) {
		spokeProject, err := store.CreateProject(ctx, "external-root-comment-spoke")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: spokeProject.ID, Role: db.FederationRoleSpoke,
			HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: spokeProject.UID,
			PushEnabled: true, Actor: "bound-agent", Enabled: true,
		})
		require.NoError(t, err)
		spokeIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: spokeProject.ID, Title: "Projected comment", Author: "requesting-agent",
		})
		require.NoError(t, err)
		hub := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, hub.Close()) })
		hubProject, err := hub.CreateProjectWithUID(ctx, "external-root-comment-hub", spokeProject.UID)
		require.NoError(t, err)
		hubIssue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: hubProject.ID, UID: spokeIssue.UID, Title: spokeIssue.Title, Author: "bound-agent",
		})
		require.NoError(t, err)
		_, err = hub.EnableProjectFederation(ctx, hubProject.ID, "operator")
		require.NoError(t, err)

		observedAt := time.Date(2026, 8, 20, 11, 0, 0, 123456789, time.UTC)
		binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: spokeProject.ID, IssueID: spokeIssue.ID,
			ConnectorInstance: "notes", ExternalRootKey: "push-comment-root",
			ExternalAccountKey: "opaque-account-key", Actor: "bound-agent",
			ReceiveCommentsAfter: observedAt.Add(-time.Hour),
		})
		require.NoError(t, err)
		binding, claimed, err := store.ClaimExternalRootBinding(
			ctx, binding.ID, "push-comment-claim", observedAt, observedAt.Add(-time.Minute),
		)
		require.NoError(t, err)
		require.True(t, claimed)
		projected, event, created, err := store.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{
			BindingID: binding.ID, ClaimToken: binding.ClaimToken,
			ExternalID: "provider-comment", ExternalRevision: "provider-revision",
			Body: "Provider comment", ExternalActorID: "provider-actor",
			ExternalActorName: "Provider Contributor",
			ExternalCreatedAt: observedAt, ExternalUpdatedAt: observedAt,
			IntegrationActor: "connector:notes",
		})
		require.NoError(t, err)
		require.True(t, created)
		require.NotNil(t, event)
		assert.Equal(t, "bound-agent", projected.Author)
		assert.Equal(t, "bound-agent", event.Actor)
		var payload struct {
			Author string `json:"author"`
			Source struct {
				ActorID   string `json:"actor_id"`
				ActorName string `json:"actor_name"`
			} `json:"source"`
		}
		require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
		assert.Equal(t, "bound-agent", payload.Author)
		assert.Equal(t, "provider-actor", payload.Source.ActorID)
		assert.Equal(t, "Provider Contributor", payload.Source.ActorName)

		ingested, err := hub.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: hubProject.ID, SpokeInstanceUID: store.InstanceUID(), BoundActor: "bound-agent",
			Events: []db.FederationIngestEvent{{SourceEventID: event.ID, Event: remoteEventFromStored(*event)}},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, ingested.Accepted)
		hubComments, err := hub.CommentsByIssue(ctx, hubIssue.ID)
		require.NoError(t, err)
		require.Len(t, hubComments, 1)
		assert.Equal(t, "bound-agent", hubComments[0].Author)
		assert.Equal(t, "Provider comment", hubComments[0].Body)
	})

	t.Run("push-enabled spoke projects external root field and audit events with bound actor", func(t *testing.T) {
		const (
			boundActor       = "bound-agent"
			connector        = "push-projection-notes"
			integrationActor = "connector:" + connector
		)
		spokeProject, err := store.CreateProject(ctx, "external-root-projection-spoke")
		require.NoError(t, err)
		_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: spokeProject.ID, Role: db.FederationRoleSpoke,
			HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: spokeProject.UID,
			PushEnabled: true, Actor: boundActor, Enabled: true,
		})
		require.NoError(t, err)
		spokeIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: spokeProject.ID, Title: "Projected root", Author: "requesting-agent",
		})
		require.NoError(t, err)

		hub := backend.Open(t)
		t.Cleanup(func() { require.NoError(t, hub.Close()) })
		hubProject, err := hub.CreateProjectWithUID(ctx, "external-root-projection-hub", spokeProject.UID)
		require.NoError(t, err)
		hubIssue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: hubProject.ID, UID: spokeIssue.UID, Title: spokeIssue.Title, Author: boundActor,
		})
		require.NoError(t, err)
		_, err = hub.EnableProjectFederation(ctx, hubProject.ID, "operator")
		require.NoError(t, err)

		observedAt := time.Date(2026, 8, 20, 12, 0, 0, 123456789, time.UTC)
		binding, bindingEvent, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
			ProjectID: spokeProject.ID, IssueID: spokeIssue.ID,
			ConnectorInstance: connector, ExternalRootKey: "push-projection-root",
			ExternalAccountKey: "opaque-account-key", Actor: integrationActor,
			ReceiveCommentsAfter: observedAt.Add(-time.Hour),
		})
		require.NoError(t, err)
		assert.Equal(t, boundActor, bindingEvent.Actor)
		var auditPayload db.ExternalRootAuditPayload
		require.NoError(t, json.Unmarshal([]byte(bindingEvent.Payload), &auditPayload))
		assert.Equal(t, connector, auditPayload.ConnectorInstance)
		assert.Equal(t, integrationActor, auditPayload.Actor)
		mapping, err := store.UpsertExternalFieldMapping(ctx, db.ExternalFieldMappingParams{
			ConnectorInstance: connector, KataField: "scheduled_on",
			ExternalFieldID: "start", ExternalFieldName: "Start",
			AcceptedKinds: []string{"date"}, Nullable: true, Writable: true,
			SchemaRevision: "schema-projection",
		})
		require.NoError(t, err)

		binding, claimed, err := store.ClaimExternalRootBinding(
			ctx, binding.ID, "push-projection-claim", observedAt, observedAt.Add(-time.Minute),
		)
		require.NoError(t, err)
		require.True(t, claimed)
		projected, rootEvent, changed, err := store.ApplyExternalRootProjection(ctx, db.ExternalRootProjectionParams{
			BindingID: binding.ID, ClaimToken: binding.ClaimToken,
			Title: "Provider title", Body: "Provider body", ExternalRevision: "provider-root-revision",
			ExternalActorID: "provider-actor", ExternalActorName: "Provider Contributor",
			ExternalUpdatedAt: observedAt, ExternalObservedAt: observedAt,
			IntegrationActor: integrationActor,
		})
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, rootEvent)
		assert.Equal(t, boundActor, rootEvent.Actor)
		var rootPayload struct {
			Source db.ExternalProjectionSource `json:"source"`
		}
		require.NoError(t, json.Unmarshal([]byte(rootEvent.Payload), &rootPayload))
		assert.Equal(t, connector, rootPayload.Source.ConnectorInstance)
		assert.Equal(t, "provider-actor", rootPayload.Source.ActorID)
		assert.Equal(t, "Provider Contributor", rootPayload.Source.ActorName)

		projected, fieldEvent, changed, err := store.ApplyExternalFieldProjection(ctx, db.ExternalFieldProjectionParams{
			BindingID: binding.ID, MappingID: mapping.ID, ClaimToken: binding.ClaimToken,
			KataField: "scheduled_on", ExpectedIssueRevision: projected.Revision,
			Patch: map[string]json.RawMessage{
				"scheduled_on": json.RawMessage(`"2026-08-24"`),
			},
			IntegrationActor: integrationActor,
		})
		require.NoError(t, err)
		require.True(t, changed)
		require.NotNil(t, fieldEvent)
		assert.Equal(t, boundActor, fieldEvent.Actor)

		_, auditEvent, err := store.UpsertExternalFieldState(ctx, db.ExternalFieldStateParams{
			BindingID: binding.ID, MappingID: mapping.ID, ClaimToken: binding.ClaimToken,
			Baseline:     json.RawMessage(`"2026-08-23"`),
			ConflictKata: json.RawMessage(`"2026-08-24"`), ConflictExternal: json.RawMessage(`"2026-08-25"`),
			Conflicted: true, At: observedAt.Add(time.Minute), Actor: integrationActor,
		})
		require.NoError(t, err)
		require.NotNil(t, auditEvent)
		assert.Equal(t, boundActor, auditEvent.Actor)
		require.NoError(t, json.Unmarshal([]byte(auditEvent.Payload), &auditPayload))
		assert.Equal(t, connector, auditPayload.ConnectorInstance)
		assert.Equal(t, integrationActor, auditPayload.Actor)

		events := []db.Event{bindingEvent, *rootEvent, *fieldEvent, *auditEvent}
		transport := make([]db.FederationIngestEvent, 0, len(events))
		for _, event := range events {
			transport = append(transport, db.FederationIngestEvent{
				SourceEventID: event.ID, Event: remoteEventFromStored(event),
			})
		}
		ingested, err := hub.IngestFederationEvents(ctx, db.FederationIngestParams{
			ProjectID: hubProject.ID, SpokeInstanceUID: store.InstanceUID(), BoundActor: boundActor,
			Events: transport,
		})
		require.NoError(t, err)
		assert.Equal(t, len(transport), ingested.Accepted)
		hubIssue, err = hub.IssueByID(ctx, hubIssue.ID)
		require.NoError(t, err)
		assert.Equal(t, "Provider title", hubIssue.Title)
		assert.Equal(t, "Provider body", hubIssue.Body)
		var metadata map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(hubIssue.Metadata), &metadata))
		assert.JSONEq(t, `"2026-08-24"`, string(metadata["scheduled_on"]))
	})

	t.Run("lifecycle request and review label are atomic deduplicated and claim guarded", func(t *testing.T) {
		fixture := newExternalRootSafetyFixture(t, store, "lifecycle-request")
		integrationActor := "connector:notes"
		providerTime := fixture.now.Add(time.Minute + 654321*time.Nanosecond)
		params := db.ExternalCommentProjectionParams{
			BindingID: fixture.binding.ID, ClaimToken: fixture.token,
			ExternalID: "lifecycle:complete:revision-1", ExternalRevision: "revision-1", LifecycleState: "complete",
			Body: "External completion requested", ExternalActorID: "actor-3", ExternalActorName: "local-operator",
			ExternalCreatedAt: providerTime, ExternalUpdatedAt: providerTime,
			IntegrationActor: integrationActor,
		}
		providerParams := params
		providerParams.ExternalRevision = "provider-comment-revision-1"
		providerParams.LifecycleState = ""
		providerParams.Body = "Provider comment using a lifecycle-shaped ID"
		providerComment, _, providerCreated, err := store.UpsertExternalCommentProjection(ctx, providerParams)
		require.NoError(t, err)
		require.True(t, providerCreated)

		comment, events, created, err := store.EnsureExternalRootLifecycleRequest(ctx, params)
		require.NoError(t, err)
		assert.True(t, created)
		assert.NotEqual(t, providerComment.ID, comment.ID)
		persisted, err := store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, err)
		assert.Equal(t, "complete", persisted.LastExternalState)
		assert.Equal(t, "revision-1", persisted.LastExternalRevision)
		assert.Equal(t, integrationActor, comment.Author)
		assert.Equal(t, providerTime, comment.CreatedAt)
		require.Len(t, events, 2)
		assert.Equal(t, "issue.commented", events[0].Type)
		assert.Equal(t, integrationActor, events[0].Actor)
		assert.Equal(t, "issue.labeled", events[1].Type)
		assert.Equal(t, integrationActor, events[1].Actor)
		var payload struct {
			Author string `json:"author"`
			Source struct {
				ActorID   string `json:"actor_id"`
				ActorName string `json:"actor_name"`
			} `json:"source"`
		}
		require.NoError(t, json.Unmarshal([]byte(events[0].Payload), &payload))
		assert.Equal(t, integrationActor, payload.Author)
		assert.Equal(t, "actor-3", payload.Source.ActorID)
		assert.Equal(t, "local-operator", payload.Source.ActorName)
		hasReview, err := store.HasLabel(ctx, fixture.issue.ID, "needs-review")
		require.NoError(t, err)
		assert.True(t, hasReview)

		_, err = store.RemoveLabelAndEvent(ctx, fixture.issue.ID, db.LabelEventParams{
			EventType: "issue.unlabeled", Label: "needs-review", Actor: "verifier",
		})
		require.NoError(t, err)
		duplicate, duplicateEvents, duplicateCreated, err := store.EnsureExternalRootLifecycleRequest(ctx, params)
		require.NoError(t, err)
		assert.False(t, duplicateCreated)
		assert.Empty(t, duplicateEvents)
		assert.Equal(t, comment.ID, duplicate.ID)
		hasReview, err = store.HasLabel(ctx, fixture.issue.ID, "needs-review")
		require.NoError(t, err)
		assert.False(t, hasReview)

		newer := params
		newer.ExternalID = "lifecycle:open:revision-2"
		newer.ExternalRevision = "revision-2"
		newer.LifecycleState = "open"
		newer.ExternalCreatedAt = providerTime.Add(time.Minute)
		newer.ExternalUpdatedAt = providerTime.Add(time.Minute)
		_, _, newerCreated, err := store.EnsureExternalRootLifecycleRequest(ctx, newer)
		require.NoError(t, err)
		require.True(t, newerCreated)
		_, staleEvents, staleCreated, err := store.EnsureExternalRootLifecycleRequest(ctx, params)
		require.NoError(t, err)
		assert.False(t, staleCreated)
		assert.Empty(t, staleEvents)
		persisted, err = store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, err)
		assert.Equal(t, "open", persisted.LastExternalState)
		assert.Equal(t, "revision-2", persisted.LastExternalRevision)

		params.ClaimToken = "wrong-claim"
		params.ExternalID = "lifecycle:complete:revision-3"
		params.ExternalRevision = "revision-3"
		params.LifecycleState = "complete"
		_, _, _, err = store.EnsureExternalRootLifecycleRequest(ctx, params)
		assert.ErrorIs(t, err, db.ErrExternalRootClaimLost)
		persisted, err = store.ExternalRootBindingByID(ctx, fixture.binding.ID)
		require.NoError(t, err)
		assert.Equal(t, "open", persisted.LastExternalState)
		assert.Equal(t, "revision-2", persisted.LastExternalRevision)
		_, err = store.ImportMappingBySource(
			ctx, fixture.project.ID, db.ExternalRootLifecycleMappingSource(fixture.binding), "comment", "lifecycle:complete:revision-3",
		)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	if t.Failed() {
		return fmt.Errorf("external root safety invariant checks failed")
	}
	return nil
}

func requireCanonicalMetadataUpdatePayload(
	t *testing.T,
	event db.Event,
	issue db.Issue,
) metadataUpdatePayload {
	t.Helper()
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &envelope))
	assert.ElementsMatch(t, []string{"diff", "revision_new", "updated_at"}, mapKeys(envelope))
	var payload metadataUpdatePayload
	require.NoError(t, json.Unmarshal([]byte(event.Payload), &payload))
	assert.Equal(t, issue.Revision, payload.RevisionNew)
	updatedAt, err := time.Parse(time.RFC3339Nano, payload.UpdatedAt)
	require.NoError(t, err)
	assert.Equal(t, issue.UpdatedAt.UTC(), updatedAt.UTC())
	require.Len(t, payload.Diff, 2)
	for key, want := range map[string]string{
		"scheduled_on": `"2026-08-21T09:30"`,
		"timezone":     `"America/Los_Angeles"`,
	} {
		diff, ok := payload.Diff[key]
		require.True(t, ok, "missing diff for %s", key)
		assert.JSONEq(t, "null", string(diff.From))
		assert.JSONEq(t, want, string(diff.To))
		var entry map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(envelopeDiffEntry(t, envelope["diff"], key), &entry))
		assert.ElementsMatch(t, []string{"from", "to"}, mapKeys(entry))
	}
	return payload
}

func envelopeDiffEntry(t *testing.T, raw json.RawMessage, key string) []byte {
	t.Helper()
	var entries map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &entries))
	entry, ok := entries[key]
	require.True(t, ok, "missing raw diff for %s", key)
	return entry
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
