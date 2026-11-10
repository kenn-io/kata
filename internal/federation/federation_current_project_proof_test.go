package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// The sender can still see the old owner when another joined spoke has already
// moved the issue on the hub. Supplemental proofs cannot rely on local ownership.
func TestFederationOrdinarySourceProofIncludesJoinedCurrentProject(t *testing.T) {
	for _, kind := range []string{"same-origin", "other-binding-origin", "other-credential-origin", "other-actor", "pull-only", "relay"} {
		t.Run(kind, func(t *testing.T) {
			hub, projects := newMoveDeliveryHub(t, "proof-source", "proof-target")
			spoke := newMoveDeliverySpoke(t, hub, projects)
			issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Stale local owner", Author: "worker"})
			require.NoError(t, err)
			_, event, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Delayed source edit"})
			require.NoError(t, err)
			other := testenv.New(t)
			target, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[1].ID)
			require.NoError(t, err)
			switch kind {
			case "other-binding-origin":
				target.HubURL = other.URL
			case "other-credential-origin":
				credential := spoke.credentials[projects[1].UID]
				credential.HubURL = other.URL
				spoke.credentials[projects[1].UID] = credential
			case "other-actor":
				target.Actor = "other-worker"
			case "pull-only":
				target.PushEnabled = false
			}
			_, err = spoke.env.DB.UpsertFederationBinding(t.Context(), target)
			require.NoError(t, err)
			if kind == "relay" {
				bindingUID, err := uid.New()
				require.NoError(t, err)
				public, _, err := ed25519.GenerateKey(rand.Reader)
				require.NoError(t, err)
				require.NoError(t, spoke.env.DB.PinRootAuthority(t.Context(), db.RootKeyPin{ProjectUID: projects[1].UID, AuthorityUID: hub.DB.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}))
				_, err = spoke.env.DB.SetRelayBindingConfig(t.Context(), target.ProjectID, db.RelayBindingConfig{
					ProtocolVersion: db.RelayProtocolVersion, BindingUID: bindingUID,
					UpstreamInstanceUID: hub.DB.InstanceUID(), AuthorityUID: hub.DB.InstanceUID(),
					HubPath:    []string{hub.DB.InstanceUID(), spoke.env.DB.InstanceUID()},
					LocalActor: "worker", ResetEpoch: 1,
				})
				require.NoError(t, err)
			}
			binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
			require.NoError(t, err)
			tokens, err := federationMoveSourceTokens(t.Context(), spoke.env.DB, spoke.credentials, binding, hub.URL, []db.Event{event}, clientpkg.Opts{})
			if kind == "other-credential-origin" {
				require.Error(t, err, "credentials are never sent to a different configured origin")
				require.Empty(t, tokens)
				return
			}
			require.NoError(t, err)
			if kind == "same-origin" {
				require.Equal(t, spoke.credentials[projects[1].UID].Token, tokens[projects[1].UID])
			} else {
				require.NotContains(t, tokens, projects[1].UID)
			}
		})
	}
}

func TestSyncFederationDelayedSourceEditCurrentProjectAuthority(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "delayed-edit-source", "delayed-edit-target")
	sender := newMoveDeliverySpoke(t, hub, projects)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Delayed source work", Author: "worker"})
	require.NoError(t, err)
	for i := range projects {
		require.NoError(t, sender.sync(t, i))
	}
	local, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, stale, err := sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Authorized delayed edit"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	credential := sender.credentials[projects[1].UID]
	delete(sender.credentials, projects[1].UID)
	runner := &Runner{DB: sender.env.DB, Credentials: sender.credentials}
	binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	_, err = runner.runSpoke(t.Context(), activeSpokeBinding{binding: binding, project: sender.projects[0]}, sender.credentials, nil, nil)
	require.NoError(t, err, "missing enrollment retains its diagnostic through a full runner push/pull turn")
	status, err := sender.env.DB.FederationSyncStatusByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.NotNil(t, status.LastError)
	require.Contains(t, *status.LastError, projects[1].Name)
	require.NotNil(t, status.LastPullSuccessAt)
	require.NotNil(t, status.LastErrorAt)
	require.False(t, status.LastPullSuccessAt.Before(*status.LastErrorAt), "successful pull retains the push diagnostic")
	binding, err = sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.Less(t, binding.PushCursorEventID, stale.ID)
	comments, err := hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments)
	sender.credentials[projects[1].UID] = credential
	require.NoError(t, sender.sync(t, 0), "joined current-project proof allows retry without rewriting the source envelope")
	comments, err = hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "Authorized delayed edit", comments[0].Body)
}

type boundedPushPageStore struct {
	db.Storage
}

func (s *boundedPushPageStore) PendingFederationPushEvents(ctx context.Context, projectID int64, origin string, after int64, limit int) ([]db.Event, error) {
	return s.Storage.PendingFederationPushEvents(ctx, projectID, origin, after, min(limit, 1))
}

func TestSyncFederationMissingEnrollmentDoesNotBlockOtherPushes(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
	sender := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, sender.sync(t, 0))
	local, err := sender.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, commentEvent, err := sender.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Retained stale comment"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	good, _, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Independent first batch", Author: "worker"})
	require.NoError(t, err)
	paged := &boundedPushPageStore{Storage: sender.env.DB}
	push := func() {
		t.Helper()
		binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
		require.NoError(t, err)
		client, err := NewClient(t.Context(), hub.URL, sender.credentials[projects[0].UID].Token, clientpkg.Opts{})
		require.NoError(t, err)
		require.NoError(t, pushDirectSpoke(t.Context(), paged, &binding, projects[0].ID, client, nil, sender.credentials, clientpkg.Opts{}))
	}
	push()
	_, err = hub.DB.IssueByUID(t.Context(), good.UID, db.IncludeDeletedNo)
	require.NoError(t, err, "independent work arrives after an all-rejected page")
	status, err := sender.env.DB.FederationSyncStatusByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.NotNil(t, status.LastError)
	require.Contains(t, *status.LastError, projects[1].Name)
	require.Contains(t, *status.LastError, issue.UID)
	require.NotNil(t, status.LastPullSuccessAt)
	binding, err := sender.env.DB.FederationBindingByProject(t.Context(), sender.projects[0].ID)
	require.NoError(t, err)
	require.Less(t, binding.PushCursorEventID, commentEvent.ID, "retain the original rejected event for retry")
	later, _, err := sender.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: sender.projects[0].ID, Title: "Independent later turn", Author: "worker"})
	require.NoError(t, err)
	push()
	_, err = hub.DB.IssueByUID(t.Context(), later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Empty(t, comments)

	// Joining the destination alone must recover the retained event next turn.
	target, err := sender.env.DB.CreateProjectWithUID(t.Context(), projects[1].Name, projects[1].UID)
	require.NoError(t, err)
	_, err = sender.env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: target.ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: sender.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull", Actor: "worker"})
	require.NoError(t, err)
	sender.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "worker"}
	push()
	comments, err = hub.DB.CommentsByIssue(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "Retained stale comment", comments[0].Body)
	retained, err := hub.DB.EventsByUIDs(t.Context(), projects[0].ID, []string{commentEvent.UID})
	require.NoError(t, err)
	require.Len(t, retained, 1)
	require.Equal(t, commentEvent.ContentHash, retained[0].ContentHash)
	_, err = sender.env.DB.ActiveFederationQuarantine(t.Context(), sender.projects[0].ID, db.FederationQuarantineDirectionPush)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestFederationMissingEnrollmentThenPoisonBlocksPush(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "poison-source", "poison-target")
	spoke := newMoveDeliverySpoke(t, hub, projects[:1])
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Moved work", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, stale, err := spoke.env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: local.ID, Author: "worker", Body: "Retain this event"})
	require.NoError(t, err)
	_, err = hub.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: projects[0].ID, ToProjectID: projects[1].ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, poison, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Malformed secondary reference", Author: "worker"})
	require.NoError(t, err)
	// Corrupt only this synthetic retained event, as a real 400 poison delivery.
	_, err = spoke.env.DB.ExecContext(t.Context(), `UPDATE events SET related_issue_uid = ? WHERE id = ?`, "01HZNQ7VFPK1XGD8R5MABCD4EB", poison.ID)
	require.NoError(t, err)
	good, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "Wait behind poison", Author: "worker"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
	require.NoError(t, err)
	client, err := NewClient(t.Context(), hub.URL, spoke.credentials[projects[0].UID].Token, clientpkg.Opts{})
	require.NoError(t, err)
	paged := &boundedPushPageStore{Storage: spoke.env.DB}
	err = pushDirectSpoke(t.Context(), paged, &binding, projects[0].ID, client, nil, spoke.credentials, clientpkg.Opts{})
	var status *HubStatusError
	require.ErrorAs(t, err, &status)
	require.Equal(t, 400, status.StatusCode)
	q, err := spoke.env.DB.ActiveFederationQuarantine(t.Context(), binding.ProjectID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	require.Equal(t, []string{stale.UID, poison.UID}, q.EventUIDs)
	require.Equal(t, stale.ID, q.FirstEventID)
	require.Equal(t, poison.ID, q.LastEventID)
	err = pushDirectSpoke(t.Context(), paged, &binding, projects[0].ID, client, nil, spoke.credentials, clientpkg.Opts{})
	require.ErrorIs(t, err, ErrFederationPushQuarantined)
	_, err = hub.DB.IssueByUID(t.Context(), good.UID, db.IncludeDeletedNo)
	require.ErrorIs(t, err, db.ErrNotFound)
}
