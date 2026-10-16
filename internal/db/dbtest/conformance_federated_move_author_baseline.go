package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFederatedMoveAuthorBaseline(t *testing.T, hub db.Storage, backend Backend) error {
	for _, cold := range []bool{false, true} {
		name := "hub-replay"
		if cold {
			name = "cold-replica"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			source, err := hub.CreateProject(ctx, name+"-author-source")
			require.NoError(t, err)
			target, err := hub.CreateProject(ctx, name+"-author-target")
			require.NoError(t, err)
			issue, creation, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "rewritten before enrollment", Author: "original-author"})
			require.NoError(t, err)
			_, _, err = hub.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "original-author", Body: "retained discussion"})
			require.NoError(t, err)
			rewrite, err := hub.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{ProjectID: source.ID, Actor: "worker", From: "original-author", To: "renamed-author"})
			require.NoError(t, err)
			require.True(t, rewrite.Changed)
			for _, project := range []db.Project{source, target} {
				_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
				require.NoError(t, err)
			}
			issue, err = hub.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			_, _, err = hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "unrelated source content", Author: "renamed-author"})
			require.NoError(t, err)

			read, err := hub.ReadFederation(db.WithAuthorizedProjects(ctx, []string{target.UID}), db.FederationReadParams{ProjectID: target.ID, IssueUID: issue.UID, Limit: 100, Features: db.SupportedEventFeatures})
			require.NoError(t, err)
			require.NotEmpty(t, read.Events)
			for _, event := range read.Events {
				if event.IssueUID == nil {
					t.Fatalf("issue-scoped history exposed project-wide %s", event.Type)
				}
				require.Equal(t, issue.UID, *event.IssueUID)
			}

			replay := hub
			replayTarget := target
			replaySource := source
			if cold {
				replay = backend.Open(t)
				t.Cleanup(func() { require.NoError(t, replay.Close()) })
				for _, project := range []db.Project{source, target} {
					local, err := replay.CreateProjectWithUID(ctx, project.Name, project.UID)
					require.NoError(t, err)
					_, err = replay.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
					require.NoError(t, err)
					if project.ID == target.ID {
						replayTarget = local
					} else {
						replaySource = local
					}
				}
				remote := make([]db.RemoteEvent, len(read.Events))
				for i, event := range read.Events {
					remote[i] = remoteMoveEvent(event)
				}
				_, err = replay.InsertRemoteEvents(ctx, replayTarget.ID, remote)
				require.NoError(t, err)
			}
			require.NoError(t, replay.MaterializeFederatedProject(ctx, replayTarget.ID))
			got, err := replay.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, replayTarget.ID, got.ProjectID)
			require.Equal(t, "renamed-author", got.Author, "source baseline preserves the rewrite, and later source rewrites do not follow the issue")
			comments, err := replay.CommentsByIssue(ctx, got.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "renamed-author", comments[0].Author)
			if cold {
				_, err = replay.LeaveFederationReplica(ctx, replaySource.ID)
				require.NoError(t, err)
				_, _, err = replay.CreateIssue(ctx, db.CreateIssueParams{ProjectID: replaySource.ID, Title: "remaining source issue", Author: "renamed-author"})
				require.NoError(t, err)
				_, err = replay.RewriteAuthorIdentity(ctx, db.RewriteAuthorIdentityParams{ProjectID: replaySource.ID, Actor: "worker", From: "renamed-author", To: "later-source-author"})
				require.NoError(t, err)
				require.NoError(t, replay.MaterializeFederatedProject(ctx, replayTarget.ID))
				got, err = replay.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, "renamed-author", got.Author, "former source rewrites must not follow the moved issue")
			}
			retained, err := hub.EventsByUIDs(ctx, source.ID, []string{creation.UID})
			require.NoError(t, err)
			require.Len(t, retained, 1)
			require.Equal(t, creation.ContentHash, retained[0].ContentHash)
		})
	}
	return nil
}
