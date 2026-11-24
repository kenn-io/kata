package federation

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestRetainedCommentHistoryIgnoresEditOnlySnapshotEntries(t *testing.T) {
	const commentUID = "01HZNQ7VFPK1XGD8R5MABCD4ED"
	payload := jsontext.Value(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EE","comments":[{"comment_uid":"` + commentUID + `","author":"earlier-actor","body":"edited placeholder"}]}`)
	event := db.Event{Type: "issue.snapshot", Payload: string(payload)}
	require.False(t, eventCreatesComment(event, commentUID))
	require.Empty(t, envelopeCreatedComments(api.EventEnvelope{Type: "issue.snapshot", Payload: payload}))
	event.Type = "issue.comment_edited"
	require.False(t, eventCreatesComment(event, commentUID))

	payload = jsontext.Value(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EE","comments":[{"comment_uid":"` + commentUID + `","author":"earlier-actor","body":"real comment","created_at":"2026-05-23T12:00:00Z"}]}`)
	event.Type, event.Payload = "issue.snapshot", string(payload)
	require.True(t, eventCreatesComment(event, commentUID))
	require.True(t, envelopeCreatedComments(api.EventEnvelope{Type: "issue.snapshot", Payload: payload})[commentUID])
}

func rejoinWithActor(t *testing.T, spoke moveDeliverySpoke, hubURL string, hubProject db.Project, hubStore db.Storage, capabilities string) {
	rejoinProjectWithActor(t, spoke, 0, hubURL, hubProject, hubStore, capabilities)
}

func rejoinProjectWithActor(t *testing.T, spoke moveDeliverySpoke, projectIndex int, hubURL string, hubProject db.Project, hubStore db.Storage, capabilities string) {
	t.Helper()
	ctx := t.Context()
	localProject := spoke.projects[projectIndex]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, localProject.ID)
	require.NoError(t, err)
	grant, err := hubStore.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &hubProject.ID, Capabilities: capabilities, Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: localProject.ID, Role: db.FederationRoleSpoke, HubURL: hubURL, HubProjectID: hubProject.ID, HubProjectUID: hubProject.UID, Enabled: true, PushEnabled: true, Actor: "new-actor", RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[hubProject.UID] = config.FederationCredential{HubURL: hubURL, HubProjectID: hubProject.ID, Token: grant.Token, Actor: "new-actor"}
}

func TestRejoinRejectsWrongActor(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "wrong-actor-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
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

func pushRejoin(t *testing.T, spoke moveDeliverySpoke) error {
	return pushRejoinProject(t, spoke, 0)
}

func pushRejoinProject(t *testing.T, spoke moveDeliverySpoke, projectIndex int) error {
	t.Helper()
	ctx := t.Context()
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[projectIndex].ID)
	if err != nil {
		return err
	}
	creds := spoke.credentials[spoke.projects[projectIndex].UID]
	client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
	if err != nil {
		return err
	}
	return pushDirectSpoke(ctx, spoke.env.DB, &binding, creds.HubProjectID, client, nil, nil, spoke.credentials, clientpkg.Opts{})
}

func TestReviewRetainedReply(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "reply-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "parent"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	independent, independentEvent, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "independent task", Author: "new-actor"})
	require.NoError(t, err)
	_, reply, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	err = pushRejoin(t, spoke)
	require.ErrorContains(t, err, parent.UID)
	quarantine, err := spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	require.Equal(t, []string{reply.UID}, quarantine.EventUIDs)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PushCursorEventID, independentEvent.ID)
	require.Less(t, binding.PushCursorEventID, reply.ID)
	_, err = hub.DB.IssueByUID(ctx, independent.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
	localEvents, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{reply.UID})
	require.NoError(t, err)
	require.Len(t, localEvents, 1)
	require.Equal(t, "new-actor", localEvents[0].Actor)
	require.Equal(t, reply.ContentHash, localEvents[0].ContentHash)
}

func TestRejoinReplyToAcceptedRetainedComment(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "accepted-reply-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "parent"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
}

func TestRejoinReplyToRetainedCommentInRecoverySnapshot(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "recovery-reply-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "retained parent"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
	var bodies []string
	for _, comment := range comments {
		bodies = append(bodies, comment.Body)
	}
	require.ElementsMatch(t, []string{"retained parent", "reply"}, bodies)
}

func TestRejoinSameRequestReplyParentNeedsNoHistoryRead(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "same-request-reply-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push")
	parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "parent"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, comments, 2)
}

func TestRejoinRetainedReplyNeedsHubReadAuthority(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "push-only-reply-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "retained parent"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push")
	_, reply, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	err = pushRejoin(t, spoke)
	require.ErrorContains(t, err, "check retained discussion comment "+parent.UID)
	_, err = spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	require.Less(t, binding.PushCursorEventID, reply.ID)
}

func TestRejoinReplyToRetainedCommentOnAnotherTask(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "cross-task-reply-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "reply task", Author: "old-actor"})
	require.NoError(t, err)
	parentIssue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "parent task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: parentIssue.ID, Author: "old-actor", Body: "parent"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, reply, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	require.ErrorContains(t, pushRejoin(t, spoke), parent.UID)
	quarantine, err := spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	require.Equal(t, []string{reply.UID}, quarantine.EventUIDs)
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

func TestRejoinRetainedCommentEditWaitsForCreation(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "retained-comment-edit-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	comment, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "original"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	updatedBody := "edited"
	_, edit, changed, err := spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "new-actor", Body: updatedBody})
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, edit)
	require.ErrorContains(t, pushRejoin(t, spoke), comment.UID)
	quarantine, err := spoke.env.DB.ActiveFederationQuarantine(ctx, spoke.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	require.Equal(t, []string{edit.UID}, quarantine.EventUIDs)
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
}

func TestReviewNewLinkToRetainedPeer(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "link-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "peer", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "new-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	remotePeer, err := hub.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := hub.DB.LinksByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, remotePeer.UID, links[0].ToIssueUID)
}

func TestRejoinBulkLinkAdditionPublishesRetainedPeer(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "bulk-link-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained parent", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	child, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "new child", Author: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: child.ID, Actor: "new-actor", SetParent: &peer.ID})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	remoteChild, err := hub.DB.IssueByUID(ctx, child.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	remotePeer, err := hub.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := hub.DB.LinksByIssue(ctx, remoteChild.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, remotePeer.UID, links[0].ToIssueUID)
}

func TestRejoinLinkToRetainedPeerWithCycle(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "cyclic-link-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	first, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "first", Author: "old-actor"})
	require.NoError(t, err)
	second, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "second", Author: "old-actor"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: first.ID, ToIssueID: second.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: first.ID, Actor: "old-actor", FromUID: first.UID, ToUID: second.UID, FromShortID: first.ShortID, ToShortID: second.ShortID})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: second.ID, ToIssueID: first.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: second.ID, Actor: "old-actor", FromUID: second.UID, ToUID: first.UID, FromShortID: second.ShortID, ToShortID: first.ShortID})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	newIssue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "new task", Author: "new-actor"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: newIssue.ID, ToIssueID: first.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: newIssue.ID, Actor: "new-actor", FromUID: newIssue.UID, ToUID: first.UID, FromShortID: newIssue.ShortID, ToShortID: first.ShortID})
	require.NoError(t, err)
	require.NoError(t, pushRejoin(t, spoke))
	for _, issue := range []db.Issue{first, second, newIssue} {
		_, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err, issue.Title)
	}
	remoteNewIssue, err := hub.DB.IssueByUID(ctx, newIssue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := hub.DB.LinksByIssue(ctx, remoteNewIssue.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, first.UID, links[0].ToIssueUID)
}

func TestRejoinNewLinkToRetainedPeerInAnotherProject(t *testing.T) {
	for _, withCredential := range []bool{true, false} {
		name := "credential available"
		if !withCredential {
			name = "credential missing"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "peer-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "peer", Author: "old-actor"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			require.NoError(t, spoke.sync(t, 1))
			rejoinProjectWithActor(t, spoke, 0, hub.URL, projects[0], hub.DB, "push,pull")
			rejoinProjectWithActor(t, spoke, 1, hub.URL, projects[1], hub.DB, "push,pull")
			peerCredential := spoke.credentials[projects[1].UID]
			if !withCredential {
				require.NoError(t, spoke.credentials.DeleteFederationCredential(ctx, projects[1].UID))
			}
			_, linkEvent, err := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "new-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "new-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
			require.NoError(t, err)
			err = pushRejoin(t, spoke)
			if !withCredential {
				require.ErrorContains(t, err, peer.UID)
				binding, bindingErr := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
				require.NoError(t, bindingErr)
				require.Less(t, binding.PushCursorEventID, linkEvent.ID)
				remoteIssue, issueErr := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, issueErr)
				links, linksErr := hub.DB.LinksByIssue(ctx, remoteIssue.ID)
				require.NoError(t, linksErr)
				require.Empty(t, links)
				require.NoError(t, spoke.credentials.StoreFederationCredential(ctx, projects[1].UID, peerCredential))
				err = pushRejoin(t, spoke)
				require.NoError(t, err)
			}
			require.NoError(t, err)
			remotePeer, err := hub.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			remoteIssue, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			links, err := hub.DB.LinksByIssue(ctx, remoteIssue.ID)
			require.NoError(t, err)
			require.Len(t, links, 1)
			require.Equal(t, remotePeer.UID, links[0].ToIssueUID)
			// The peer binding later sees its locally recorded recovery snapshot.
			// It must advance past the copy the source binding already published.
			require.NoError(t, pushRejoinProject(t, spoke, 1))
		})
	}
}

func TestRejoinRetainedIssueAndLaterTaskPublish(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	old, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, commentEvent, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local edit"})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	remote, err := hub.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "local edit", comments[0].Body)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, 0, 0))
	require.NoError(t, spoke.sync(t, 0))
	events, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{commentEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestRejoinedSpokeBatchesNewTasks(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "batch-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	_, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	require.NoError(t, pushRejoin(t, spoke))
	for _, title := range []string{"first", "second"} {
		_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: title, Author: "new-actor"})
		require.NoError(t, err)
	}
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	creds := spoke.credentials[projects[0].UID]
	client, err := NewClient(ctx, creds.HubURL, creds.Token, clientpkg.Opts{})
	require.NoError(t, err)
	transport := client.client.Transport
	requests := 0
	client.client.Transport = pushOnlyTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			requests++
		}
		return transport.RoundTrip(req)
	})
	require.NoError(t, pushDirectSpoke(ctx, spoke.env.DB, &binding, projects[0].ID, client, nil, nil, spoke.credentials, clientpkg.Opts{}))
	require.Equal(t, 1, requests)
}

func TestRejoinRetainedIssueKeepsCrossProjectLinkLocal(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project", "peer-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
	peerProject := spoke.projects[1]
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "old-actor"})
	require.NoError(t, err)
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: peerProject.ID, Title: "peer task", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 1))
	_, linkEvent, err := spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "old-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "later edit"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	remoteLinks, err := hub.DB.LinksByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Empty(t, remoteLinks)
	require.NoError(t, spoke.env.DB.ResetFederatedProject(ctx, spoke.projects[0].ID, 0, 0))
	require.NoError(t, spoke.sync(t, 0))
	issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	links, err := spoke.env.DB.LinksByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, peer.UID, links[0].ToIssueUID)
	events, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{linkEvent.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestRetainedTaskAfterRejoin(t *testing.T) {
	for _, mode := range []string{"previously-published", "unpublished-move"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "new-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "other task", Author: "old-actor"})
			require.NoError(t, err)
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "original comment"})
			require.NoError(t, err)
			if mode == "previously-published" {
				require.NoError(t, spoke.sync(t, 0))
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
			require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(ctx, projects[0].UID, spoke.credentials[projects[0].UID]))
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			move := func(dryRun bool) (int, string) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.ShortID), strings.NewReader(fmt.Sprintf(`{"actor":"new-actor","to_project_uid":%q,"dry_run":%t}`, projects[1].UID, dryRun)))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
				resp, err := spoke.env.HTTP.Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())
				return resp.StatusCode, string(body)
			}
			if mode == "unpublished-move" {
				before, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: spoke.projects[0].ID, Limit: 100})
				require.NoError(t, err)
				status, body := move(true)
				require.Equal(t, http.StatusConflict, status, body)
				after, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: spoke.projects[0].ID, Limit: 100})
				require.NoError(t, err)
				require.Equal(t, before, after)
				status, body = move(false)
				require.Equal(t, http.StatusConflict, status, body)
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(body), &response))
				require.Equal(t, "federation_sync_required", response.Error.Code)
				current, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, spoke.projects[0].ID, current.ProjectID)
				require.NoError(t, spoke.sync(t, 0))
			}
			status, body := move(false)
			require.Equal(t, http.StatusOK, status, body)
			var later db.Issue
			if mode == "unpublished-move" {
				later, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "later", Author: "new-actor"})
				require.NoError(t, err)
			}
			require.NoError(t, spoke.sync(t, 0))
			require.NoError(t, spoke.sync(t, 1))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, remote.ProjectID)
			if mode == "unpublished-move" {
				remote, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, projects[1].ID, remote.ProjectID)
			}
		})
	}
}

func TestMoveRetainedTaskAfterSecondRejoin(t *testing.T) {
	for _, tc := range []struct {
		name              string
		publishedSnapshot bool
		publishedIssue    bool
	}{
		{name: "published-snapshot", publishedSnapshot: true},
		{name: "unpublished-snapshot"},
		{name: "unpublished-snapshot-after-published-issue", publishedIssue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "third-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "old-actor"})
			require.NoError(t, err)
			if tc.publishedIssue {
				require.NoError(t, spoke.sync(t, 0))
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "recovered"})
			require.NoError(t, err)
			var snapshot db.Event
			if tc.publishedSnapshot {
				require.NoError(t, spoke.sync(t, 0))
			} else {
				snapshot, _, _, err = spoke.env.DB.RetainedIssueSnapshot(ctx, spoke.projects[0].ID, issue.UID)
				require.NoError(t, err)
			}
			_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull,claim", Actor: "third-actor"})
			require.NoError(t, err)
			_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Enabled: true, PushEnabled: true, Actor: "third-actor", RetainHistory: true})
			require.NoError(t, err)
			spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "third-actor"}
			if !tc.publishedSnapshot {
				_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: "third-actor"})
				require.NoError(t, err)
			}
			require.NoError(t, spoke.sync(t, 0))
			if !tc.publishedSnapshot {
				binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
				require.NoError(t, err)
				require.Greater(t, binding.PushCursorEventID, snapshot.ID)
				remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				if tc.publishedIssue {
					require.NoError(t, err)
					require.Equal(t, "old-actor", remote.Author)
					comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
					require.NoError(t, err)
					require.Empty(t, comments)
				} else {
					require.ErrorIs(t, err, db.ErrNotFound)
				}
			}
			require.NoError(t, config.DefaultFederationCredentialStore().StoreFederationCredential(ctx, projects[0].UID, spoke.credentials[projects[0].UID]))
			issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.ShortID), strings.NewReader(fmt.Sprintf(`{"actor":"third-actor","to_project_uid":%q}`, projects[1].UID)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
			resp, err := spoke.env.HTTP.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			if !tc.publishedSnapshot {
				require.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
				var response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(body, &response))
				if tc.publishedIssue {
					require.Equal(t, "federated_move_unsupported", response.Error.Code)
				} else {
					require.Equal(t, "federation_sync_required", response.Error.Code)
				}
				current, err := spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, spoke.projects[0].ID, current.ProjectID)
				return
			}
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			require.NoError(t, spoke.sync(t, 0))
			require.NoError(t, spoke.sync(t, 1))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[1].ID, remote.ProjectID)
		})
	}
}

func TestRejoinPublishesRecoverySnapshot(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "recovery-publication")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "new comment"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	counts := map[string]int{}
	publish := func(_ int64, events []db.Event) {
		for _, event := range events {
			if event.Type == "issue.snapshot" {
				counts[event.UID]++
			}
		}
	}
	for range 2 {
		require.NoError(t, SyncFederationOnceWithPulledEvents(ctx, spoke.env.DB, binding, spoke.credentials[projects[0].UID], clientpkg.Opts{}, publish, spoke.credentials))
	}
	events, err := spoke.env.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: spoke.projects[0].ID, IssueUID: issue.UID, Limit: 100})
	require.NoError(t, err)
	for _, event := range events {
		if event.Type == "issue.snapshot" && event.OriginInstanceUID == spoke.env.DB.InstanceUID() {
			require.Equal(t, 1, counts[event.UID])
			return
		}
	}
	t.Fatal("recovery snapshot not found")
}

func TestPushPendingPublishesRecoverySnapshot(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "push-publication")
	var push func(context.Context, db.FederationBinding, config.FederationCredential) error
	spoke := newMoveDeliverySpokeWithOptions(t, hub, projects, []testenv.Option{func(cfg *daemon.ServerConfig) { push = cfg.FederationPushPending }}, "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "new comment"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	sub := spoke.env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: spoke.projects[0].ID})
	defer sub.Unsub()
	require.NoError(t, push(ctx, binding, spoke.credentials[projects[0].UID]))
	select {
	case msg := <-sub.Ch:
		require.NotNil(t, msg.Event)
		require.Equal(t, "issue.snapshot", msg.Event.Type)
	default:
		t.Fatal("recovery snapshot was not published")
	}
}
