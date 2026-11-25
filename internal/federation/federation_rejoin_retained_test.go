package federation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestRejoinKeepsOldActorEventsLocal(t *testing.T) {
	for _, pushEnabled := range []bool{true, false} {
		name := "push-enabled rejoin"
		if !pushEnabled {
			name = "enable push after rejoin"
		}
		t.Run(name, func(t *testing.T) { testRejoinKeepsOldActorEventsLocal(t, pushEnabled) })
	}
}

func TestRejoinResumesActorMismatchQuarantine(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	_, old, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "pull,push", Actor: "new-actor"})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, projects[0].ID)
	require.NoError(t, err)
	var replica api.CreateFederationReplicaResponseBody
	postJSON(t, spoke.env.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url": hub.URL, "hub_project_id": projects[0].ID, "hub_project_uid": projects[0].UID,
		"project_name": project.Name, "replay_horizon_event_id": hubBinding.ReplayHorizonEventID, "token": grant.Token, "capabilities": "pull,push", "actor": "new-actor", "push_enabled": true,
	}, &replica)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID: project.ID, Direction: db.FederationQuarantineDirectionPush,
		FirstEventID: old.ID, LastEventID: old.ID, EventUIDs: []string{old.UID},
		Error:     fmt.Sprintf(`hub /api/v1/projects/42/federation/events:ingest returned 400: {"status":400,"error":{"code":"validation","message":"federation ingest validation: event %s actor \"old-actor\" does not match bound actor"}}`, old.UID),
		CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "pull,push"}
	require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	events, err := spoke.env.DB.EventsByUIDs(ctx, project.ID, []string{old.UID})
	require.NoError(t, err)
	require.Equal(t, old.ContentHash, events[0].ContentHash)
}

func testRejoinKeepsOldActorEventsLocal(t *testing.T, pushEnabled bool) {
	t.Helper()
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	eligible, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "eligible task", Author: "new-actor"})
	require.NoError(t, err)
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "peer task", Author: "new-actor"})
	require.NoError(t, err)
	old, event, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
	require.NoError(t, err)
	parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: eligible.ID, Author: "old-actor", Body: "local parent"})
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "pull,push", Actor: "new-actor"})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, projects[0].ID)
	require.NoError(t, err)
	var replica api.CreateFederationReplicaResponseBody
	postJSON(t, spoke.env.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url": hub.URL, "hub_project_id": projects[0].ID, "hub_project_uid": projects[0].UID,
		"project_name": project.Name, "replay_horizon_event_id": hubBinding.ReplayHorizonEventID, "token": grant.Token, "capabilities": "pull,push", "actor": "new-actor", "push_enabled": pushEnabled,
	}, &replica)
	if !pushEnabled {
		cursor, err := spoke.env.DB.MaxEventID(ctx)
		require.NoError(t, err)
		_, err = spoke.env.DB.EnableFederationPush(ctx, project.ID, cursor)
		require.NoError(t, err)
	}
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local comment"})
	require.NoError(t, err)
	_, _, _, err = spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: eligible.ID, CommentUID: parent.UID, Actor: "new-actor", Body: "edited local parent"})
	require.NoError(t, err)
	reply, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: eligible.ID, Author: "new-actor", Body: "local reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: eligible.ID, Author: "new-actor", Body: "local nested reply", ReplyToUID: reply.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, err = spoke.env.DB.EditIssueAtomic(ctx, db.EditIssueAtomicParams{IssueID: eligible.ID, Actor: "new-actor", AddBlocks: []int64{old.ID, peer.ID}})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)

	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "pull,push"}
	require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
	hubEligible, err := hub.DB.IssueByUID(ctx, eligible.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	hubPeer, err := hub.DB.IssueByUID(ctx, peer.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.LinkByEndpoints(ctx, hubEligible.ID, hubPeer.ID, "blocks")
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, hubEligible.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
	events, err := spoke.env.DB.EventsByUIDs(ctx, project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{event}, events)
	binding, err = spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	err = spoke.env.DB.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, spoke.env.DB.InstanceUID(), binding.PushCursorEventID)
	require.NoError(t, err)
	events, err = spoke.env.DB.EventsByUIDs(ctx, project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, event.UID, events[0].UID)
	require.Equal(t, event.ContentHash, events[0].ContentHash)
	_, err = spoke.env.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

func TestRejoinPublishesEditsAfterLostAcknowledgement(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	issue, accepted, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "accepted task", Author: "old-actor"})
	require.NoError(t, err)
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_spoke_push_cursor_advance=error")
	require.Error(t, spoke.sync(t, 0))
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	hubIssue, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "pull,push", Actor: "new-actor"})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, projects[0].ID)
	require.NoError(t, err)
	var replica api.CreateFederationReplicaResponseBody
	postJSON(t, spoke.env.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url": hub.URL, "hub_project_id": projects[0].ID, "hub_project_uid": projects[0].UID,
		"project_name": project.Name, "replay_horizon_event_id": hubBinding.ReplayHorizonEventID, "token": grant.Token, "capabilities": "pull,push", "actor": "new-actor", "push_enabled": true,
	}, &replica)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "edit after rejoin"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "pull,push"}
	require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
	comments, err := hub.DB.CommentsByIssue(ctx, hubIssue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "edit after rejoin", comments[0].Body)
	var local bool
	err = spoke.env.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM meta WHERE key=$1 AND value='local')`, db.FederationAcknowledgedEventPrefix+project.UID+"."+accepted.UID).Scan(&local)
	require.NoError(t, err)
	require.False(t, local)
}

func TestRejoinKeepsOnlyCandidateEventsLocal(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "new-actor")
	_, event, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "foreign task", Author: "old-actor"})
	require.NoError(t, err)
	require.Equal(t, "new-actor", event.Actor)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	pending, err := spoke.env.DB.PendingFederationPushEvents(ctx, spoke.projects[0].ID, spoke.env.DB.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	kept, err := spoke.env.DB.KeepFederationEventLocal(ctx, spoke.projects[0].ID, event.ID)
	require.NoError(t, err)
	require.False(t, kept)
	require.NoError(t, spoke.sync(t, 0))
}
