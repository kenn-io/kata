package federation

import (
	"path/filepath"
	"testing"

	"encoding/json/jsontext"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestRejoinRetainedHistory(t *testing.T) {
	t.Run("unknown issue", checkRejoinPublishesNewEventsAfterOldActor)
	t.Run("known issue", checkRejoinDoesNotSnapshotKnownIssue)
	t.Run("unknown link peer", checkRejoinPublishesRetainedLinkPeer)
	t.Run("known link peer", checkRejoinLinkToKnownRetainedPeerAfterInterruptedRetire)
}

func rejoinWithActor(t *testing.T, spoke moveDeliverySpoke, hubURL string, hubProject db.Project, hubStore db.Storage, capabilities string) {
	t.Helper()
	ctx := t.Context()
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hubStore.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &hubProject.ID, Capabilities: capabilities, Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hubURL, HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[hubProject.UID] = config.FederationCredential{HubURL: hubURL, HubProjectID: hubProject.ID, Token: grant.Token, Actor: "new-actor"}
}

func checkRejoinPublishesNewEventsAfterOldActor(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	oldIssue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "new task", Author: "new-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.env.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: oldIssue.ID, Actor: "new-actor", Patch: map[string]jsontext.Value{"area": jsontext.Value(`"api"`)}})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: oldIssue.ID, Author: "new-actor", Body: "new comment"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	published, err := hub.DB.IssueByUID(ctx, oldIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.JSONEq(t, `{"area":"api"}`, string(published.Metadata))
	comments, err := hub.DB.CommentsByIssue(ctx, published.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "new comment", comments[0].Body)
}

func checkRejoinDoesNotSnapshotKnownIssue(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "known task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "new comment"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	snapshots, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projects[0].ID, IssueUID: issue.UID, Types: []string{"issue.snapshot"}, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, snapshots)
	published, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, published.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	binding.Actor = "old-actor"
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, wrong, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "wrong actor task", Author: "old-actor"})
	require.NoError(t, err)
	require.Equal(t, "old-actor", wrong.Actor)
	require.Greater(t, wrong.ID, binding.RetainedThroughEventID)
	binding.Actor = "new-actor"
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	err = spoke.sync(t, 0)
	require.ErrorContains(t, err, "does not match bound actor")
	_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
}

func checkRejoinPublishesRetainedLinkPeer(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	known, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "known", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	retained, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained", Author: "old-actor"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: retained.ID, Author: "old-actor", Body: "old comment"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: retained.ID, ToIssueID: known.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: retained.ID, Actor: "old-actor", FromUID: retained.UID, ToUID: known.UID, FromShortID: retained.ShortID, ToShortID: known.ShortID})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push")
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: known.ID, ToIssueID: retained.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: known.ID, Actor: "new-actor", FromUID: known.UID, ToUID: retained.UID, FromShortID: known.ShortID, ToShortID: retained.ShortID})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later", Author: "new-actor"})
	require.NoError(t, err)
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "after_retained_snapshot_publish=interrupt")
	require.ErrorContains(t, pushRejoin(t, spoke), "after_retained_snapshot_publish")
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	spoke = reopenRejoinSpoke(t, spoke)
	require.NoError(t, pushRejoin(t, spoke))
	remote, err := hub.DB.IssueByUID(ctx, known.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := hub.DB.LinksByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, links, 2)
	var newLinkFound bool
	for _, link := range links {
		require.Equal(t, "new-actor", link.Author)
		newLinkFound = newLinkFound || (link.FromIssueUID == known.UID && link.ToIssueUID == retained.UID && link.Type == "blocks")
	}
	require.True(t, newLinkFound)
	remoteRetained, err := hub.DB.IssueByUID(ctx, retained.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "new-actor", remoteRetained.Author)
	comments, err := hub.DB.CommentsByIssue(ctx, remoteRetained.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "new-actor", comments[0].Author)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assertNoRejoinPushPending(t, spoke)
}

func pushRejoin(t *testing.T, spoke moveDeliverySpoke) error {
	t.Helper()
	ctx := t.Context()
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	if err != nil {
		return err
	}
	creds := spoke.credentials[spoke.projects[0].UID]
	client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
	if err != nil {
		return err
	}
	return pushDirectSpoke(ctx, spoke.env.DB, &binding, creds.HubProjectID, client, nil, nil, spoke.credentials, clientpkg.Opts{})
}

func reopenRejoinSpoke(t *testing.T, spoke moveDeliverySpoke) moveDeliverySpoke {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(spoke.env.Home, "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	env := *spoke.env
	env.DB = store
	spoke.env = &env
	return spoke
}

func assertNoRejoinPushPending(t *testing.T, spoke moveDeliverySpoke) {
	t.Helper()
	binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
	require.NoError(t, err)
	pending, err := spoke.env.DB.PendingFederationPushEvents(t.Context(), binding.ProjectID, spoke.env.DB.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func checkRejoinLinkToKnownRetainedPeerAfterInterruptedRetire(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	from, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "from", Author: "old-actor"})
	require.NoError(t, err)
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "peer", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push")
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: from.ID, ToIssueID: peer.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: from.ID, Actor: "new-actor", FromUID: from.UID, ToUID: peer.UID, FromShortID: from.ShortID, ToShortID: peer.ShortID})
	require.NoError(t, err)
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_retained_snapshot_retire=interrupt")
	require.ErrorContains(t, pushRejoin(t, spoke), "before_retained_snapshot_retire")
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	spoke = reopenRejoinSpoke(t, spoke)
	require.NoError(t, pushRejoin(t, spoke))
	remote, err := hub.DB.IssueByUID(ctx, from.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := hub.DB.LinksByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, peer.UID, links[0].ToIssueUID)
	snapshots, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: spoke.projects[0].ID, IssueUID: peer.UID, Types: []string{"issue.snapshot"}, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, snapshots)
	_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound)
	assertNoRejoinPushPending(t, spoke)
}
