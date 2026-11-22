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

func TestRejoinPublishesNewEventsAfterOldActor(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	metadata, err := spoke.env.DB.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: spoke.projects[0].ID, Actor: "old-actor", Patch: map[string]jsontext.Value{"purpose": jsontext.Value(`"local"`)}})
	require.NoError(t, err)
	oldIssue, oldEvent, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull", Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor"}
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
	remoteIssue, remoteEvent, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "hub task", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{remoteEvent.UID})
	require.NoError(t, err)
	retained, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{oldEvent.UID})
	require.NoError(t, err)
	require.Equal(t, oldEvent.ContentHash, retained[0].ContentHash)
	require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, 0, 0))
	_, err = spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{remoteEvent.UID})
	require.ErrorIs(t, err, db.ErrNotFound)
	retained, err = spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{oldEvent.UID})
	require.NoError(t, err)
	require.Equal(t, oldEvent.ContentHash, retained[0].ContentHash)
	retainedMetadata, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{metadata.Event.UID})
	require.NoError(t, err)
	require.Equal(t, metadata.Event.ContentHash, retainedMetadata[0].ContentHash)
	require.NoError(t, spoke.env.DB.MaterializeFederatedProject(ctx, spoke.projects[0].ID))
	_, err = spoke.env.DB.IssueByUID(ctx, oldIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	later, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "later hub task", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.env.DB.IssueByUID(ctx, remoteIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

func TestRejoinDoesNotSnapshotKnownIssue(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "known task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull", Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor"}
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

	_, wrong, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "wrong actor task", Author: "new-actor"})
	require.NoError(t, err)
	hash, err := db.EventContentHash(db.EventHashInput{UID: wrong.UID, OriginInstanceUID: wrong.OriginInstanceUID, ProjectUID: wrong.ProjectUID, ProjectName: wrong.ProjectName, IssueUID: wrong.IssueUID, Type: wrong.Type, Actor: "old-actor", HLCPhysicalMS: wrong.HLCPhysicalMS, HLCCounter: wrong.HLCCounter, CreatedAt: wrong.CreatedAt.UTC().Format(db.EventTimestampFormat), Payload: jsontext.Value(wrong.Payload)})
	require.NoError(t, err)
	_, err = spoke.env.DB.ExecContext(ctx, `UPDATE events SET actor=?, content_hash=? WHERE uid=?`, "old-actor", hash, wrong.UID)
	require.NoError(t, err)
	err = spoke.sync(t, 0)
	require.ErrorContains(t, err, "does not match bound actor")
	_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
}

func TestRejoinPublishesRetainedLinkPeer(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	known, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "known", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	retained, oldEvent, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained", Author: "old-actor"})
	require.NoError(t, err)
	_, oldComment, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: retained.ID, Author: "old-actor", Body: "old comment"})
	require.NoError(t, err)
	_, oldLink, err := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: retained.ID, ToIssueID: known.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: retained.ID, Actor: "old-actor", FromUID: retained.UID, ToUID: known.UID, FromShortID: retained.ShortID, ToShortID: known.ShortID})
	require.NoError(t, err)
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push", Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor"}
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
	for _, original := range []db.Event{oldEvent, oldComment, oldLink} {
		old, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{original.UID})
		require.NoError(t, err)
		require.Equal(t, "old-actor", old[0].Actor)
		require.Equal(t, original.ContentHash, old[0].ContentHash)
	}
	localRetained, err := spoke.env.DB.IssueByUID(ctx, retained.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, "old-actor", localRetained.Author)
	localComments, err := spoke.env.DB.CommentsByIssue(ctx, localRetained.ID)
	require.NoError(t, err)
	require.Len(t, localComments, 1)
	require.Equal(t, "old-actor", localComments[0].Author)
	localLinks, err := spoke.env.DB.LinksByIssue(ctx, localRetained.ID)
	require.NoError(t, err)
	var oldLinkFound bool
	for _, link := range localLinks {
		if link.FromIssueUID == retained.UID && link.ToIssueUID == known.UID {
			oldLinkFound = true
			require.Equal(t, "new-actor", link.Author)
		}
	}
	require.True(t, oldLinkFound)
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

func TestRejoinLinkToKnownRetainedPeerAfterInterruptedRetire(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	from, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "from", Author: "old-actor"})
	require.NoError(t, err)
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "peer", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push", Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor"}
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: from.ID, ToIssueID: peer.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: from.ID, Actor: "new-actor", FromUID: from.UID, ToUID: peer.UID, FromShortID: from.ShortID, ToShortID: peer.ShortID})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: from.ID, Author: "new-actor", Body: "after link"})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later", Author: "new-actor"})
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
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	snapshots, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: spoke.projects[0].ID, IssueUID: peer.UID, Types: []string{"issue.snapshot"}, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, snapshots)
	_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound)
	assertNoRejoinPushPending(t, spoke)
}
