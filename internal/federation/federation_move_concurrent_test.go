package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationConcurrentMoveBranchesConverge(t *testing.T) {
	for _, order := range [][]int{{1, 2}, {2, 1}} {
		name := "middle-first"
		if order[0] == 2 {
			name = "target-first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "concurrent-source", "concurrent-middle", "concurrent-target")
			spokes := []moveDeliverySpoke{newMoveDeliverySpoke(t, hub, projects), newMoveDeliverySpoke(t, hub, projects)}
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Concurrent offline moves", Author: "worker"})
			require.NoError(t, err)
			for _, spoke := range spokes {
				for i := range projects {
					require.NoError(t, spoke.sync(t, i))
				}
			}
			var moves []db.FoldEvent
			for index, spoke := range spokes {
				local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				moved, err := spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: local.ID, FromProjectID: spoke.projects[0].ID, ToProjectID: spoke.projects[index+1].ID, IfMatchRev: local.Revision, Actor: "worker"})
				require.NoError(t, err)
				if moved.Event == nil {
					t.Fatal("missing move event")
				}
				event := moved.Event
				moves = append(moves, db.FoldEvent{UID: event.UID, OriginInstanceUID: event.OriginInstanceUID, ProjectUID: projects[index+1].UID, IssueUID: issue.UID, Type: event.Type, Actor: event.Actor, Payload: []byte(event.Payload), HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter})
			}
			winner := db.FoldEvents(moves).Issues[issue.UID].ProjectUID
			for _, destination := range order {
				require.NoError(t, spokes[destination-1].sync(t, destination), "a competing acknowledged move must not permanently block push before pull")
			}
			got, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			project, err := hub.DB.ProjectByID(ctx, got.ProjectID)
			require.NoError(t, err)
			require.Equal(t, winner, project.UID)
			for _, spoke := range spokes {
				for i := range projects {
					require.NoError(t, spoke.sync(t, i))
				}
				got, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				project, err := spoke.env.DB.ProjectByID(ctx, got.ProjectID)
				require.NoError(t, err)
				require.Equal(t, winner, project.UID, "both arrival orders must converge without operator retry")
			}
		})
	}
}
