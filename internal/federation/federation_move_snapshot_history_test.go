package federation

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationMovePreservesHistoricalPeerReferences(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		archived bool
	}{{"link", false}, {"reply", false}, {"link", true}, {"reply", true}} {
		name := tc.kind
		if tc.archived {
			name += "-archived"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "snapshot-source", "snapshot-target", "snapshot-peer")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moving discussion", Author: "worker"})
			require.NoError(t, err)
			peer, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[2].ID, Title: "Joined peer", Author: "worker"})
			require.NoError(t, err)
			if tc.kind == "link" {
				_, _, err = hub.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "worker"}, db.LinkEventParams{
					EventType: "issue.linked", EventIssueID: issue.ID,
					FromShortID: issue.ShortID, FromUID: issue.UID,
					ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "worker",
				})
				require.NoError(t, err)
			} else {
				comment, _, err := hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: peer.ID, Author: "worker", Body: "Peer finding"})
				require.NoError(t, err)
				_, _, err = hub.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Confirmed", ReplyToUID: comment.UID, ReplyKind: "confirm"})
				require.NoError(t, err)
			}
			for _, i := range []int{2, 0, 1} {
				require.NoError(t, spoke.sync(t, i))
			}
			local, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			localPeer, err := spoke.env.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			if tc.kind == "link" {
				_, err = spoke.env.DB.LinkByEndpoints(ctx, local.ID, localPeer.ID, "blocks")
				require.NoError(t, err, "the spoke must receive the historical link before moving")
			}
			if tc.archived {
				_, _, err = hub.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: projects[2].ID, Actor: "worker", Force: true})
				require.NoError(t, err)
			}
			spoke.move(t, local, 0, 1)
			require.NoError(t, spoke.sync(t, 1), "retained historical references must not block an authorized move")
			got, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, got.ProjectID)
			if tc.kind == "link" {
				link, err := hub.DB.LinkByEndpoints(ctx, got.ID, peer.ID, "blocks")
				require.NoError(t, err, "the moved issue must retain its historical relationship")
				require.Equal(t, issue.UID, link.FromIssueUID)
				require.Equal(t, peer.UID, link.ToIssueUID)
			} else {
				comments, err := hub.DB.CommentsByIssue(ctx, got.ID)
				require.NoError(t, err)
				require.Len(t, comments, 1)
				require.NotEmpty(t, comments[0].ReplyToUID)
			}
		})
	}
}
