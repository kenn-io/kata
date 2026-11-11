package federation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
)

func TestSyncMoveOutPreservesConnectorComment(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			hub, spoke := nativeStore(t, "sqlite"), nativeStore(t, backend)
			server := nativeHTTP(t, hub)
			source, err := hub.CreateProject(ctx, "source-project")
			require.NoError(t, err)
			target, err := hub.CreateProject(ctx, "target-project")
			require.NoError(t, err)
			for _, p := range []db.Project{source, target} {
				_, err = hub.EnableProjectFederation(ctx, p.ID, "worker")
				require.NoError(t, err)
			}
			grant, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &source.ID, Capabilities: "push,pull", Actor: "worker"})
			require.NoError(t, err)
			project, err := spoke.CreateProjectWithUID(ctx, "spoke-project", source.UID)
			require.NoError(t, err)
			_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: server.URL, HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
			require.NoError(t, err)
			credential := config.FederationCredential{HubURL: server.URL, HubProjectID: source.ID, Token: grant.Token, Actor: "worker"}
			sync := func() error {
				binding, err := spoke.FederationBindingByProject(ctx, project.ID)
				if err != nil {
					return err
				}
				return federation.SyncFederationOnce(ctx, spoke, binding, credential)
			}
			issue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Moving issue", Author: "worker"})
			require.NoError(t, err)
			require.NoError(t, sync())
			local, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			now := time.Now().UTC()
			root, _, err := spoke.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{ProjectID: project.ID, IssueID: local.ID, ConnectorInstance: "notes", ExternalRootKey: "root", ExternalAccountKey: "account", Actor: "worker", ReceiveCommentsAfter: now})
			require.NoError(t, err)
			root, claimed, err := spoke.ClaimExternalRootBinding(ctx, root.ID, "worker-token", now, now.Add(-db.ExternalRootClaimStaleAfter))
			require.NoError(t, err)
			require.True(t, claimed)
			comment, _, changed, err := spoke.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{BindingID: root.ID, ClaimToken: root.ClaimToken, ExternalID: "comment", ExternalRevision: "one", Body: "External comment", ExternalActorID: "person", ExternalActorName: "Person", ExternalCreatedAt: now, ExternalUpdatedAt: now, IntegrationActor: "connector:notes"})
			require.NoError(t, err)
			require.True(t, changed)
			require.NoError(t, sync())
			issue, err = hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, err = spoke.ProjectByUID(ctx, target.UID)
			require.ErrorIs(t, err, db.ErrNotFound)
			require.NoError(t, sync(), "outbound move must not discard connector-owned content")
			binding, err := spoke.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			require.GreaterOrEqual(t, binding.PullCursorEventID, moved.Event.ID)
			comments, err := spoke.CommentsByIssue(ctx, local.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, comment.UID, comments[0].UID)
			require.Equal(t, comment.Body, comments[0].Body)
			mapping, err := spoke.ImportMappingBySource(ctx, project.ID, db.ExternalRootCommentMappingSource(root), "comment", "comment")
			require.NoError(t, err)
			require.NotNil(t, mapping.CommentID)
			require.Equal(t, comment.ID, *mapping.CommentID)
			_, _, _, err = spoke.EditComment(ctx, db.EditCommentParams{IssueID: local.ID, CommentUID: comment.UID, Body: "Forbidden edit", Actor: "worker"})
			require.ErrorIs(t, err, db.ErrExternalCommentContentOwned)
		})
	}
}
