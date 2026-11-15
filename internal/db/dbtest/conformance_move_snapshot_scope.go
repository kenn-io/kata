package dbtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkMoveSnapshotProjectScope(t *testing.T, store db.Storage) error {
	for _, incoming := range []bool{false, true} {
		t.Run(fmt.Sprint("incoming=", incoming), func(t *testing.T) {
			ctx := t.Context()
			projects := make([]db.Project, 3)
			for i, name := range []string{"snapshot-source", "snapshot-target", "snapshot-hidden"} {
				var err error
				projects[i], err = store.CreateProject(ctx, fmt.Sprintf("%s-%t", name, incoming))
				require.NoError(t, err)
				_, err = store.EnableProjectFederation(ctx, projects[i].ID, "worker")
				require.NoError(t, err)
			}
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "moving issue", Author: "worker"})
			require.NoError(t, err)
			peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[2].ID, Title: "restricted peer", Author: "worker"})
			require.NoError(t, err)
			from, to := issue.ID, peer.ID
			if incoming {
				from, to = to, from
			}
			_, err = store.CreateLink(ctx, db.CreateLinkParams{FromIssueID: from, ToIssueID: to, Type: "blocks", Author: "worker"})
			require.NoError(t, err)
			moved, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			require.NotNil(t, moved.Event)
			require.Contains(t, moved.Event.Payload, peer.UID)
			require.Contains(t, moved.Event.Payload, peer.ShortID)
			query := db.EventsAfterParams{AfterID: moved.EventID - 1, ThroughID: moved.EventID, Limit: 1}
			scoped := db.WithAuthorizedProjects(ctx, []string{projects[0].UID, projects[1].UID})
			page, err := store.EventsAfter(scoped, query)
			require.NoError(t, err)
			require.Len(t, page, 1)
			require.Equal(t, db.ProjectScopeResetEventType, page[0].Type)
			require.Equal(t, moved.EventID, page[0].ID)
			require.Empty(t, page[0].UID)
			require.Empty(t, page[0].Actor)
			require.Nil(t, page[0].IssueUID)
			require.Nil(t, page[0].RelatedIssueUID)
			require.Empty(t, page[0].Payload)
			for _, authorized := range []struct {
				name string
				ctx  context.Context
			}{
				{name: "all linked projects", ctx: db.WithAuthorizedProjects(ctx, []string{projects[0].UID, projects[1].UID, projects[2].UID})},
				{name: "trusted federation stream", ctx: db.WithFederationEventStream(scoped)},
			} {
				t.Run(authorized.name, func(t *testing.T) {
					page, err := store.EventsAfter(authorized.ctx, query)
					require.NoError(t, err)
					require.Len(t, page, 1)
					require.Equal(t, "issue.moved", page[0].Type)
					require.Contains(t, page[0].Payload, peer.UID)
				})
			}
		})
	}
	return nil
}
