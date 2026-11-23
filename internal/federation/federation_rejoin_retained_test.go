package federation

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/testenv"
)

func TestLegacyBindingKeepsNewWrongActorQuarantined(t *testing.T) {
	for _, timing := range []string{"before rejoin", "at rejoin", "after rejoin"} {
		t.Run(timing, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "rejoin-project")
			spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
			binding, err := spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			time.Sleep(time.Until(binding.CreatedAt.Add(time.Millisecond)))
			_, bad, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "wrong actor", Author: "old-actor"})
			require.NoError(t, err)
			require.True(t, bad.CreatedAt.After(binding.CreatedAt))
			if timing != "after rejoin" {
				createdAt := bad.CreatedAt
				if timing == "before rejoin" {
					createdAt = createdAt.Add(time.Millisecond)
				}
				_, err = spoke.env.DB.ExecContext(ctx, `UPDATE federation_bindings SET created_at = ? WHERE project_id = ?`, createdAt.UTC().Format(time.RFC3339Nano), binding.ProjectID)
				require.NoError(t, err)
			}
			binding.Actor = "new-actor"
			_, err = spoke.env.DB.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "push,pull", Actor: "new-actor"})
			require.NoError(t, err)
			spoke.credentials[projects[0].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor"}
			require.ErrorContains(t, spoke.sync(t, 0), "does not match bound actor")
			binding, err = spoke.env.DB.FederationBindingByProject(ctx, spoke.projects[0].ID)
			require.NoError(t, err)
			require.Zero(t, binding.RetainedThroughEventID)
			if timing == "before rejoin" {
				later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "later task", Author: "new-actor"})
				require.NoError(t, err)
				require.NoError(t, spoke.sync(t, 0))
				_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, spoke.sync(t, 0), ErrFederationPushQuarantined)
			}
		})
	}
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
			if edited {
				require.Len(t, comments, 3)
				found := false
				for _, comment := range comments {
					if comment.UID == reply.UID {
						require.Equal(t, "edited reply", comment.Body)
						found = true
					}
				}
				require.True(t, found)
			} else {
				require.Len(t, comments, 1)
				require.Equal(t, reply.UID, comments[0].UID)
				require.Equal(t, "reply", comments[0].Body)
			}
		})
	}
	t.Run("unknown issue", checkRejoinUnknownIssue)
	t.Run("recovery snapshots", checkRejoinRecoverySnapshots)
	t.Run("wrong actor after rejoin", checkRejoinRejectsWrongActor)
}

func TestRejoinMoveWithAcceptedRecoverySnapshotAndRetainedLink(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "source-project", "target-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor", "old-actor")
	issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained task", Author: "old-actor"})
	require.NoError(t, err)
	peer, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: "retained peer", Author: "old-actor"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateLinkAndEvent(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: peer.ID, Type: "blocks", Author: "old-actor"}, db.LinkEventParams{EventType: "issue.linked", EventIssueID: issue.ID, Actor: "old-actor", FromUID: issue.UID, ToUID: peer.UID, FromShortID: issue.ShortID, ToShortID: peer.ShortID})
	require.NoError(t, err)
	rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull,claim")
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "later work"})
	require.NoError(t, err)
	require.NoError(t, spoke.sync(t, 0), "the hub accepts the current actor's retained snapshot before the move")

	_, err = spoke.env.DB.LeaveFederationReplica(ctx, spoke.projects[1].ID)
	require.NoError(t, err)
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &projects[1].ID, Capabilities: "push,pull,claim", Actor: "new-actor"})
	require.NoError(t, err)
	_, err = spoke.env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: spoke.projects[1].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: projects[1].ID, HubProjectUID: projects[1].UID, Actor: "new-actor", Enabled: true, PushEnabled: true, RetainHistory: true})
	require.NoError(t, err)
	spoke.credentials[projects[1].UID] = config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[1].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "claim,pull,push"}
	t.Setenv("KATA_HOME", spoke.env.Home)
	for _, project := range projects {
		credential := spoke.credentials[project.UID]
		credential.Capabilities = "claim,pull,push"
		require.NoError(t, config.WriteFederationCredential(project.UID, credential))
	}

	issue, err = spoke.env.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	body := []byte(fmt.Sprintf(`{"actor":"new-actor","to_project_uid":%q}`, projects[1].UID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/move", spoke.env.URL, spoke.projects[0].ID, issue.UID), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, issue.Revision))
	resp, err := spoke.env.HTTP.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, spoke.sync(t, 1))

	remote, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, projects[1].ID, remote.ProjectID)
	moves, err := hub.DB.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projects[1].ID, IssueUID: issue.UID, Types: []string{"issue.moved"}, Limit: 10})
	require.NoError(t, err)
	require.Len(t, moves, 1)
	var moved struct {
		Snapshot db.IssueDocumentPayload `json:"snapshot"`
	}
	require.NoError(t, json.Unmarshal([]byte(moves[0].Payload), &moved))
	require.Len(t, moved.Snapshot.Links, 1)
	require.Equal(t, peer.UID, moved.Snapshot.Links[0].ToIssueUID)
	require.Equal(t, "new-actor", moved.Snapshot.Links[0].Author)
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
	for _, mode := range []string{"not queued", "multiple queued", "rejoin pending", "rejoin accepted"} {
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
			} else if mode != "rejoin pending" && mode != "rejoin accepted" {
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
					if mode == "multiple queued" || mode == "rejoin accepted" {
						envelope := federationIngestEnvelope(snapshot)
						envelope.EventID = sourceID
						_, err = client.IngestProjectEventsWithOptions(ctx, projects[0].ID, []api.FederationIngestEventEnvelope{envelope}, IngestProjectEventsOptions{})
						require.NoError(t, err)
					}
				}
			}
			if mode == "rejoin pending" || mode == "rejoin accepted" {
				rejoinWithActor(t, spoke, hub.URL, projects[0], hub.DB, "push,pull")
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
