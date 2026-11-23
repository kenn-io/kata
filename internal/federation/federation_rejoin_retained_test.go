package federation

import (
	"testing"

	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

type retainedSnapshotCounter struct {
	db.Storage
	calls int
}

func (s *retainedSnapshotCounter) RetainedIssueSnapshot(context.Context, int64, string) (db.Event, int64, error) {
	s.calls++
	return db.Event{}, 0, db.ErrNotFound
}

func TestRetainedLinkPeersSkipsOrdinaryBinding(t *testing.T) {
	issue, peer := "00000000000000000000000001", "00000000000000000000000002"
	payload, err := json.Marshal(map[string]string{"from_uid": issue, "to_uid": peer, "type": "blocks"})
	require.NoError(t, err)
	event := db.Event{Type: "issue.linked", IssueUID: &issue, Payload: string(payload)}
	store := &retainedSnapshotCounter{}
	binding := db.FederationBinding{}
	_, err = publishRetainedLinkPeers(t.Context(), store, &binding, 0, nil, []db.Event{event}, make(map[string]bool), nil, nil, nil, clientpkg.Opts{})
	require.NoError(t, err)
	require.Zero(t, store.calls)
}

func TestRejoinRetainedHistory(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "new reply"
		if edited {
			name = "edited reply"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "reply-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			if !edited {
				require.NoError(t, spoke.sync(t, 0))
			}
			grandparent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "grandparent"})
			require.NoError(t, err)
			parent, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "parent", ReplyToUID: grandparent.UID, ReplyKind: "reply"})
			require.NoError(t, err)
			if edited {
				require.NoError(t, spoke.sync(t, 0))
			}
			var reply db.Comment
			if edited {
				reply, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
				require.NoError(t, err)
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			if edited {
				_, _, _, err = spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: reply.UID, Actor: "new-actor", Body: "edited reply"})
			} else {
				reply, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply", ReplyToUID: parent.UID, ReplyKind: "reply"})
			}
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, comments, 3)
			require.Equal(t, grandparent.UID, comments[1].ReplyToUID)
			require.Equal(t, parent.UID, comments[2].ReplyToUID)
			require.Equal(t, reply.UID, comments[2].UID)
			if edited {
				require.Equal(t, "old-actor", comments[1].Author)
				require.Equal(t, "edited reply", comments[2].Body)
				require.NoError(t, spoke.sync(t, 0))
			}
		})
	}
	t.Run("unknown issue", checkRejoinUnknownIssue)
	t.Run("retained peer", func(t *testing.T) {
		for _, crossProject := range []bool{false, true} {
			name := "same project"
			if crossProject {
				name = "cross project"
			}
			t.Run(name, func(t *testing.T) { checkRejoinRetainedPeer(t, crossProject) })
		}
	})
	t.Run("recovery snapshots", checkRejoinRecoverySnapshots)
	t.Run("recovery after another rejoin", checkRecoveryAfterAnotherRejoin)
	t.Run("wrong actor after rejoin", checkRejoinRejectsWrongActor)
}

func checkRecoveryAfterAnotherRejoin(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "pending"
		if accepted {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "rejoin-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			snapshot, sourceID, err := spoke.env.DB.RetainedIssueSnapshot(ctx, spoke.projects[0].ID, issue.UID)
			require.NoError(t, err)
			if accepted {
				client, err := NewClient(ctx, hub.URL, spoke.credentials[projects[0].UID].Token, clientpkg.Opts{})
				require.NoError(t, err)
				envelope := federationIngestEnvelope(snapshot)
				envelope.EventID = sourceID
				_, err = client.IngestProjectEventsWithOptions(ctx, projects[0].ID, []api.FederationIngestEventEnvelope{envelope}, IngestProjectEventsOptions{})
				require.NoError(t, err)
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			again, _, err := spoke.env.DB.RetainedIssueSnapshot(ctx, spoke.projects[0].ID, issue.UID)
			require.NoError(t, err)
			require.Equal(t, snapshot.UID, again.UID)
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "later work"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, 0))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
		})
	}
}

func checkRejoinRetainedPeer(t *testing.T, crossProject bool) {
	t.Helper()
	ctx := t.Context()
	var projects []db.Project
	hub := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.AuthorizeFederationReference = func(_ context.Context, _ db.Transaction, source, peer daemon.HostFederationAccessRequest) error {
			if crossProject && source.Project.UID == projects[0].UID && source.Enrollment.Actor == "new-actor" && peer.Project.UID == projects[1].UID && peer.Enrollment.Actor == "peer-actor" {
				return nil
			}
			return daemon.ErrHostAccessDenied
		}
	})
	names, actors := []string{"rejoin-project"}, []string{"old-actor"}
	if crossProject {
		names, actors = append(names, "peer-project"), append(actors, "peer-actor")
	}
	for _, name := range names {
		project, err := hub.DB.CreateProject(ctx, name)
		require.NoError(t, err)
		_, err = hub.DB.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
		projects = append(projects, project)
	}
	spoke := newMoveDeliverySpoke(t, hub, projects, actors...)
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained", Author: "old-actor"})
	require.NoError(t, err)
	peerIndex := 0
	if crossProject {
		peerIndex = 1
	}
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[peerIndex].ID, Title: "peer", Author: actors[peerIndex]})
	require.NoError(t, err)
	if crossProject {
		require.NoError(t, spoke.sync(t, 1))
	}
	var incoming db.Issue
	if !crossProject {
		incoming, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "incoming", Author: "old-actor"})
		require.NoError(t, err)
		_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: incoming.ID, ToIssueID: issue.ID, Type: "parent", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: incoming.ID, Actor: "old-actor", FromUID: incoming.UID, ToUID: issue.UID, FromShortID: incoming.ShortID, ToShortID: issue.ShortID})
		require.NoError(t, err)
	}
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "old-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "current edit"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, "current edit", comments[0].Body)
	links, err := hub.DB.LinksByIssue(ctx, remote.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, peer.UID, links[0].ToIssueUID)
	if !crossProject {
		_, err = hub.DB.IssueByUID(ctx, incoming.UID, db.IncludeDeletedNo)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
}

func TestRejoinReturnsToOriginalActor(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	issue, creation, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained", Author: "old-actor"})
	require.NoError(t, err)
	unpublished, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "unpublished", Author: "old-actor"})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "recovered"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[0].ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull", Actor: "old-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[0].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, Actor: "old-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "old-actor"}
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later", Author: "old-actor"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0))
	events, err := spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{creation.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{creation}, events)
	_, err = hub.DB.IssueByUID(ctx, unpublished.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
}

func checkRejoinUnknownIssue(t *testing.T) {
	for _, mode := range []string{"old actor", "restored", "moved"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			names, actors := []string{"rejoin-project"}, []string{"old-actor"}
			if mode == "moved" {
				names, actors = append(names, "target-project"), append(actors, "old-actor")
			}
			hub, projects := newMoveDeliveryHub(t, names...)
			spoke := newMoveDeliverySpoke(t, hub, projects, actors...)
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "old task", Author: "old-actor"})
			require.NoError(t, err)
			index := 0
			if mode == "moved" {
				for _, project := range spoke.projects {
					_, err = spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
					require.NoError(t, err)
				}
				moved, err := spoke.env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: spoke.projects[0].ID, ToProjectID: spoke.projects[1].ID, IfMatchRev: issue.Revision, Actor: "old-actor"})
				require.NoError(t, err)
				issue, index = moved.Issue, 1
				grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[index].ID, Capabilities: "push,pull", Actor: "new-actor"})
				require.NoError(t, err)
				_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[index].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[index].ID, HubProjectUID: projects[index].UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
				require.NoError(t, err)
				spoke.credentials[projects[index].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[index].ID, Token: grant.Token, Actor: "new-actor"}
			} else {
				rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			}
			if mode == "restored" {
				require.NoError(t, pushRejoin(t, spoke))
				records, err := dbtest.CollectImportRecords(ctx, spoke.env.DB, db.ExportFilter{IncludeDeleted: true})
				require.NoError(t, err)
				restored := testenv.New(t)
				require.NoError(t, restored.DB.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
				grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: restored.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull", Actor: "new-actor"})
				require.NoError(t, err)
				spoke.env = restored
				spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor"}
			}
			if mode == "old actor" {
				later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "new task", Author: "new-actor"})
				require.NoError(t, err)
				require.NoError(t, spoke.sync(t, 0))
				_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				_, err = spoke.env.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "new-actor", Patch: map[string]jsontext.Value{"area": jsontext.Value(`"api"`)}})
				require.NoError(t, err)
			}
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "new comment"})
			require.NoError(t, err)
			require.NoError(t, spoke.sync(t, index))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			require.Equal(t, projects[index].ID, remote.ProjectID)
			if mode == "old actor" {
				require.JSONEq(t, `{"area":"api"}`, string(remote.Metadata))
			}
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "new comment", comments[0].Body)
		})
	}
}

func checkRejoinRecoverySnapshots(t *testing.T) {
	for _, mode := range []string{"not queued", "queued", "multiple queued"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "rejoin-project", "other-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
			issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "task", Author: "old-actor"})
			require.NoError(t, err)
			issues := []db.Issue{issue}
			if mode == "multiple queued" {
				second, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "second", Author: "old-actor"})
				require.NoError(t, err)
				issues = append(issues, second)
			} else {
				require.NoError(t, spoke.sync(t, 0))
			}
			rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
			if mode == "multiple queued" {
				_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "later work"})
				require.NoError(t, err)
			}
			var snapshot db.Event
			if mode != "not queued" {
				credential := spoke.credentials[projects[0].UID]
				client, err := NewClient(ctx, hub.URL, credential.Token, clientpkg.Opts{})
				require.NoError(t, err)
				for i, item := range issues {
					if i == 1 {
						_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[1].ID, Title: "unrelated", Author: "old-actor"})
						require.NoError(t, err)
					}
					var sourceID int64
					snapshot, sourceID, err = spoke.env.DB.RetainedIssueSnapshot(ctx, spoke.projects[0].ID, item.UID)
					require.NoError(t, err)
					if mode == "multiple queued" {
						envelope := federationIngestEnvelope(snapshot)
						envelope.EventID = sourceID
						_, err = client.IngestProjectEventsWithOptions(ctx, projects[0].ID, []api.FederationIngestEventEnvelope{envelope}, IngestProjectEventsOptions{})
						require.NoError(t, err)
					}
				}
			}
			if mode != "multiple queued" {
				_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "later work"})
				require.NoError(t, err)
			}
			require.NoError(t, spoke.sync(t, 0))
			remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(ctx, remote.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "later work", comments[0].Body)
			if mode == "queued" {
				require.Equal(t, "old-actor", remote.Author)
				_, err = spoke.env.DB.EventsByUIDs(ctx, spoke.projects[0].ID, []string{snapshot.UID})
				require.ErrorIs(t, err, db.ErrNotFound)
			}
			if mode == "not queued" {
				snapshots, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projects[0].ID, IssueUID: issue.UID, Types: []string{"issue.snapshot"}, Limit: 10})
				require.NoError(t, err)
				require.Empty(t, snapshots)
			}
			if mode == "multiple queued" {
				assertNoRejoinPushPending(t, spoke)
			}
		})
	}
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

func checkRejoinRejectsWrongActor(t *testing.T) {
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

func assertNoRejoinPushPending(t *testing.T, spoke moveDeliverySpoke) {
	t.Helper()
	binding, err := spoke.env.DB.FederationBindingByProject(t.Context(), spoke.projects[0].ID)
	require.NoError(t, err)
	pending, err := spoke.env.DB.PendingFederationPushEvents(t.Context(), binding.ProjectID, spoke.env.DB.InstanceUID(), binding.PushCursorEventID, 100)
	require.NoError(t, err)
	require.Empty(t, pending)
}
