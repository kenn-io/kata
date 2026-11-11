package federation

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
)

func TestSyncFederationMoveOutWithStandalonePeerLink(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "archived"}[archived], func(t *testing.T) {
			ctx := t.Context()
			hub, p := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, p[:1])
			if archived {
				c, e := spoke.env.DB.CreateProjectWithUID(ctx, "archived-target", p[1].UID)
				require.NoError(t, e)
				_, _, e = spoke.env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: c.ID, Actor: "worker"})
				require.NoError(t, e)
			}
			a, _, e := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p[0].ID, Title: "moving issue", Author: "worker"})
			require.NoError(t, e)
			require.NoError(t, spoke.sync(t, 0))
			local, e := spoke.env.DB.IssueByUID(ctx, a.UID, db.IncludeDeletedNo)
			require.NoError(t, e)
			standalone, e := spoke.env.DB.CreateProject(ctx, "standalone-project")
			require.NoError(t, e)
			peer, _, e := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: standalone.ID, Title: "standalone peer", Author: "worker"})
			require.NoError(t, e)
			_, ev, e := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: local.ID, ToIssueID: peer.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: peer.ID, FromShortID: peer.ShortID, FromUID: peer.UID, ToShortID: local.ShortID, ToUID: local.UID, Actor: "worker"})
			require.NoError(t, e)
			links, e := spoke.env.DB.LinksByIssue(ctx, peer.ID)
			require.NoError(t, e)
			require.Len(t, links, 1)
			t.Logf("before: link=%d event=%s peer_project=%d source_project=%d", len(links), ev.UID, peer.ProjectID, local.ProjectID)
			a, e = hub.DB.IssueByUID(ctx, a.UID, db.IncludeDeletedNo)
			require.NoError(t, e)
			moved, e := hub.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: a.ID, FromProjectID: p[0].ID, ToProjectID: p[1].ID, IfMatchRev: a.Revision, Actor: "worker"})
			require.NoError(t, e)
			before, e := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, e)
			syncErr := spoke.sync(t, 0)
			after, e := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, e)
			row, rowErr := spoke.env.DB.IssueByUID(ctx, a.UID, db.IncludeDeletedNo)
			peerRow, peerErr := spoke.env.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
			links, e = spoke.env.DB.LinksByIssue(ctx, peer.ID)
			require.NoError(t, e)
			history, e := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{IssueUID: a.UID, Limit: 100})
			require.NoError(t, e)
			retained := false
			for _, v := range history {
				if v.UID == moved.Event.UID && v.ContentHash == moved.Event.ContentHash {
					retained = true
				}
			}
			target, targetErr := spoke.env.DB.ProjectByUID(ctx, p[1].UID)
			t.Logf("sync=%v cursor_before=%d cursor_after=%d move_event_id=%d move_retained=%t", syncErr, before.PullCursorEventID, after.PullCursorEventID, moved.Event.ID, retained)
			t.Logf("moving_row_error=%v moving_project=%d peer_error=%v peer_project=%d links=%d target_error=%v target_archived=%t", rowErr, row.ProjectID, peerErr, peerRow.ProjectID, len(links), targetErr, targetErr == nil && target.DeletedAt != nil)
			require.NoError(t, peerErr)
			require.NoError(t, syncErr, "outbound pull should complete")
			require.NoError(t, rowErr)
			require.Len(t, links, 1)
			require.True(t, retained)
			require.GreaterOrEqual(t, after.PullCursorEventID, moved.Event.ID)
		})
	}
}
