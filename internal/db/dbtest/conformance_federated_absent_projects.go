package dbtest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A subtree rearranged on the hub and then moved to an unjoined project
// leaves obsolete parent links that must not make the final graph circular.
func checkFederatedParkedLinkGraph(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	source, err := hub.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := hub.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	for _, project := range []db.Project{source, target} {
		_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	uids, creations, moves := make([]string, 0, 3), make([]db.RemoteEvent, 0, 4), make([]db.RemoteEvent, 0, 3)
	for _, title := range []string{"W", "U", "V"} {
		issue, created, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: title, Author: "worker"})
		require.NoError(t, err)
		moved, err := hub.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
		require.NoError(t, err)
		uids, creations, moves = append(uids, issue.UID), append(creations, portableNativeEvent(created)), append(moves, portableNativeEvent(*moved.Event))
	}
	w, u, v := uids[0], uids[1], uids[2]
	base := moves[0].HLCPhysicalMS - 10000
	link := func(typ, from, to string, at int64) db.RemoteEvent {
		return newRemoteEvent(t, source, &from, typ, "worker", moves[0].OriginInstanceUID, base+at, []byte(fmt.Sprintf(`{"from_uid":%q,"to_uid":%q,"type":"parent"}`, from, to)))
	}
	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	route, err := spoke.CreateProjectWithUID(ctx, "source", source.UID)
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: route.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, append(slices.Clone(creations), link("issue.linked", u, v, 1)))
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	localU, err := spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, comment, err := spoke.CreateComment(ctx, db.CreateCommentParams{IssueID: localU.ID, Author: "worker", Body: "local comment"})
	require.NoError(t, err)
	_, later, err := spoke.CreateIssue(ctx, db.CreateIssueParams{ProjectID: route.ID, Title: "local work", Author: "worker"})
	require.NoError(t, err)
	// U->V becomes V->W->U before all three issues leave.
	_, err = spoke.InsertRemoteEvents(ctx, route.ID, append([]db.RemoteEvent{link("issue.unlinked", u, v, 2), link("issue.linked", v, w, 3), link("issue.linked", w, u, 4)}, moves...))
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, route.ID))
	local, err := spoke.IssueByUID(ctx, w, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := spoke.LinksByIssue(ctx, local.ID)
	require.NoError(t, err)
	var edges []string
	for _, link := range links {
		edges = append(edges, link.FromIssueUID+"->"+link.ToIssueUID)
	}
	require.ElementsMatch(t, []string{v + "->" + w, w + "->" + u}, edges)
	pending, err := spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	var ids []string
	for _, event := range pending {
		ids = append(ids, event.UID)
	}
	require.NotContains(t, ids, comment.UID)
	require.Contains(t, ids, later.UID)
	count, _, err := spoke.PendingFederationPushStats(ctx, route.ID, spoke.InstanceUID(), 0)
	require.NoError(t, err)
	require.EqualValues(t, len(pending), count)
	parked, err := spoke.ProjectByUID(ctx, target.UID)
	require.NoError(t, err)
	_, _, _, err = spoke.RestoreProject(ctx, parked.ID, "worker")
	require.NoError(t, err)
	other, err := spoke.CreateProject(ctx, "other")
	require.NoError(t, err)
	localU, err = spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localU.ID, FromProjectID: parked.ID, ToProjectID: other.ID, IfMatchRev: localU.Revision, Actor: "worker"})
	require.NoError(t, err)
	pending, err = spoke.PendingFederationPushEvents(ctx, route.ID, spoke.InstanceUID(), 0, 100)
	require.NoError(t, err)
	for _, event := range pending {
		require.NotEqual(t, comment.UID, event.UID)
	}
	localU, err = spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: localU.ID, FromProjectID: other.ID, ToProjectID: parked.ID, IfMatchRev: localU.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: parked.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: target.ID, HubProjectUID: target.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.AdvanceFederationPushCursor(ctx, route.ID, later.ID))
	require.NoError(t, spoke.ResetFederatedProject(ctx, route.ID, 0, 0))
	require.NoError(t, spoke.ResetFederatedProject(ctx, parked.ID, 0, 0))
	history, err := spoke.EventsByUIDs(ctx, route.ID, []string{comment.UID})
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, comment.ContentHash, history[0].ContentHash)
	_, err = spoke.InsertRemoteEvents(ctx, parked.ID, append(slices.Clone(creations), moves...))
	require.NoError(t, err)
	require.NoError(t, spoke.MaterializeFederatedProject(ctx, parked.ID))
	localU, err = spoke.IssueByUID(ctx, u, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := spoke.CommentsByIssue(ctx, localU.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "local comment", comments[0].Body)
	return nil
}
