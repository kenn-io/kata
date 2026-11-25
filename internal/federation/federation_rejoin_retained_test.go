package federation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type commentDependentProbe struct {
	db.Storage
	ids       []int64
	candidate bool
}

func (s *commentDependentProbe) KeepFederationCommentDependentLocal(_ context.Context, _, eventID int64) (bool, error) {
	s.ids = append(s.ids, eventID)
	return false, nil
}

func (s *commentDependentProbe) FederationCommentDependent(context.Context, int64, int64) (string, string, error) {
	if s.candidate {
		return "task", "comment", nil
	}
	return "", "", nil
}

func TestRejoinCommentDependentScan(t *testing.T) {
	store := &commentDependentProbe{}
	pending := []db.Event{
		{ID: 1, Type: "issue.commented", Payload: `{"body":"plain"}`},
		{ID: 2, Type: "issue.commented", Payload: `{"reply_to_uid":"target"}`},
		{ID: 3, Type: "issue.comment_edited", Payload: `{}`},
	}
	_, err := keepFederationCommentDependentsLocal(t.Context(), store, &db.FederationBinding{}, pending, nil, 0, nil)
	require.NoError(t, err)
	require.Equal(t, []int64{2, 3}, store.ids)
}

func TestRejoinCommentHistory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantKeep bool
		wantErr  bool
	}{
		{name: "snapshot", status: http.StatusOK, body: `{"events":[{"event_id":1,"issue_uid":"task","type":"issue.snapshot","payload":{"comments":[{"comment_uid":"comment"}]}}],"next_after_id":1}`},
		{name: "repeated adoption", status: http.StatusOK, body: `{"events":[{"event_id":1,"issue_uid":"task","type":"issue.restated","payload":{"comments":[{"comment_uid":"comment"}]}}],"next_after_id":1}`},
		{name: "edit", status: http.StatusOK, body: `{"events":[{"event_id":1,"issue_uid":"task","type":"issue.comment_edited","payload":{"comment_uid":"comment"}}],"next_after_id":1}`},
		{name: "missing", status: http.StatusNotFound, wantKeep: true},
		{name: "push only", status: http.StatusForbidden, wantKeep: true},
		{name: "retry", status: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if tc.status == http.StatusOK && r.URL.Query().Get("after_id") != "0" {
					_, _ = w.Write([]byte(`{"events":[],"next_after_id":1}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewClient(t.Context(), server.URL, "token", clientpkg.Opts{})
			require.NoError(t, err)
			store := &commentDependentProbe{candidate: true}
			pending := []db.Event{{ID: 1, Type: "issue.comment_edited", Actor: "new-actor"}, {ID: 2, Type: "issue.comment_edited", Actor: "new-actor"}}
			_, err = keepFederationCommentDependentsLocal(t.Context(), store, &db.FederationBinding{Actor: "new-actor"}, pending, client, 1, make(map[string]map[string]bool))
			if tc.wantErr {
				require.Error(t, err)
				require.Empty(t, store.ids)
				return
			}
			require.NoError(t, err)
			if tc.wantKeep {
				require.Equal(t, []int64{1, 2}, store.ids)
			} else {
				require.Empty(t, store.ids)
			}
			require.LessOrEqual(t, calls.Load(), int32(2))
		})
	}
}

func checkRejoinBatchesAlreadyAcceptedOldEvents(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	var accepted db.Issue
	for i := range 5 {
		issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "accepted task", Author: "old-actor"})
		require.NoError(t, err)
		if i == 0 {
			accepted = issue
		}
	}
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "before_spoke_push_cursor_advance=error")
	require.Error(t, spoke.sync(t, 0))
	t.Setenv("KATA_TEST_FEDERATION_FAILPOINTS", "")
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	localComment, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: accepted.ID, Author: "old-actor", Body: "local comment"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "earlier task", Author: "new-actor"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "unpublished task", Author: "old-actor"})
	require.NoError(t, err)
	token := rejoinNewActor(t, hub, projects[0], spoke.env, project)
	_, _, _, err = spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: accepted.ID, CommentUID: localComment.UID, Actor: "new-actor", Body: "edited locally"})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: accepted.ID, Author: "new-actor", Body: "later comment"})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)
	target, err := url.Parse(hub.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/federation/events:ingest") {
			requests.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: server.URL, HubProjectID: projects[0].ID, Token: token, Actor: "new-actor", Capabilities: "pull,push"}
	require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
	require.LessOrEqual(t, requests.Load(), int32(8))
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	hubIssue, err := hub.DB.IssueByUID(ctx, accepted.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	comments, err := hub.DB.CommentsByIssue(ctx, hubIssue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
}

func checkRejoinPublishesAfter500OldTasks(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	earlier, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "earlier task", Author: "new-actor"})
	require.NoError(t, err)
	var oldIssues []db.Issue
	var oldEvents []db.Event
	var oldEventUIDs []string
	for range 500 {
		issue, event, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
		require.NoError(t, err)
		oldIssues = append(oldIssues, issue)
		oldEvents = append(oldEvents, event)
		oldEventUIDs = append(oldEventUIDs, event.UID)
		_, commentEvent, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "old comment"})
		require.NoError(t, err)
		oldEvents = append(oldEvents, commentEvent)
		oldEventUIDs = append(oldEventUIDs, commentEvent.UID)
	}
	token := rejoinNewActor(t, hub, projects[0], spoke.env, project)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: token, Actor: "new-actor", Capabilities: "pull,push"}
	require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.IssueByUID(ctx, earlier.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	for _, old := range oldIssues {
		_, err = hub.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	events, err := spoke.env.DB.EventsByUIDs(ctx, project.ID, oldEventUIDs)
	require.NoError(t, err)
	require.ElementsMatch(t, oldEvents, events)
}

func checkRejoinContinuesAfterLocalWorkflowAndJob(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	definition := cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}
	workflow, workflowEvent, err := spoke.env.DB.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "workflow", Actor: "old-actor", Definition: definition})
	require.NoError(t, err)
	job := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", WorkflowUID: workflow.UID}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	oldJob, _, err := spoke.env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, Name: "old job", Actor: "old-actor", Definition: job})
	require.NoError(t, err)
	runUID := mustTestUID(t)
	run, err := spoke.env.DB.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &oldJob.UID, DefinitionEventUID: &oldJob.DefinitionEventUID, Actor: "old-actor", Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	token := rejoinNewActor(t, hub, projects[0], spoke.env, project)
	_, _, err = spoke.env.DB.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, UID: workflow.UID, ExpectedEventUID: workflow.DefinitionEventUID, Name: "edited workflow", Actor: "new-actor", Definition: definition})
	require.NoError(t, err)
	_, _, err = spoke.env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: oldJob.UID, ExpectedEventUID: oldJob.DefinitionEventUID, Name: "edited old job", Actor: "new-actor", Definition: job})
	require.NoError(t, err)
	_, err = spoke.env.DB.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &oldJob.UID, DefinitionEventUID: &oldJob.DefinitionEventUID, Actor: "old-actor", Status: "succeeded", Summary: cron.Summary{Version: 1}, ExpectedRevision: run.Run.Revision})
	require.NoError(t, err)
	later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
	require.NoError(t, err)
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: token, Actor: "new-actor", Capabilities: "pull,push"}
	err = SyncFederationOnce(ctx, spoke.env.DB, binding, creds)

	require.NoError(t, err)
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	events, err := spoke.env.DB.EventsByUIDs(ctx, project.ID, []string{workflowEvent.UID})
	require.NoError(t, err)
	require.Equal(t, []db.Event{workflowEvent}, events)
}

func TestRejoinKeepsOldActorEventsLocal(t *testing.T) {
	t.Run("snapshot comments before pull", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "rejoin-project")
		spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
		project := spoke.projects[0]
		issue, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "shared task", Author: "old-actor"})
		require.NoError(t, err)
		shared, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "shared"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 0))
		_, err = spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		local, _, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "local"})
		require.NoError(t, err)
		hubBinding, err := hub.DB.FederationBindingByProject(ctx, projects[0].ID)
		require.NoError(t, err)
		_, err = spoke.env.DB.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, ReplayHorizonEventID: hubBinding.ReplayHorizonEventID, Actor: "old-actor"})
		require.NoError(t, err)
		_, err = spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		token := rejoinNewActor(t, hub, projects[0], spoke.env, project)
		for _, comment := range []db.Comment{shared, local} {
			_, _, changed, err := spoke.env.DB.EditComment(ctx, db.EditCommentParams{IssueID: issue.ID, CommentUID: comment.UID, Actor: "new-actor", Body: "edited " + comment.Body})
			require.NoError(t, err)
			require.True(t, changed)
			_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "new-actor", Body: "reply to " + comment.Body, ReplyToUID: comment.UID, ReplyKind: "reply"})
			require.NoError(t, err)
		}
		later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
		require.NoError(t, err)
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: token, Actor: "new-actor", Capabilities: "pull,push"}
		require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
		_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		hubIssue, err := hub.DB.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		comments, err := hub.DB.CommentsByIssue(ctx, hubIssue.ID)
		require.NoError(t, err)
		require.Len(t, comments, 2)
		require.Equal(t, "edited shared", comments[0].Body)
		require.Equal(t, "reply to shared", comments[1].Body)
		require.Equal(t, shared.UID, comments[1].ReplyToUID)
	})
	t.Run("cross-task reply to retained comment", func(t *testing.T) {
		ctx := t.Context()
		hub, projects := newMoveDeliveryHub(t, "rejoin-project")
		spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
		project := spoke.projects[0]
		target, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "target", Author: "old-actor"})
		require.NoError(t, err)
		source, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "source", Author: "old-actor"})
		require.NoError(t, err)
		require.NoError(t, spoke.sync(t, 0))
		_, err = spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
		require.NoError(t, err)
		comment, original, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "old-actor", Body: "local"})
		require.NoError(t, err)
		token := rejoinNewActor(t, hub, projects[0], spoke.env, project)
		_, reply, err := spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "new-actor", Body: "reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
		require.NoError(t, err)
		later, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later", Author: "new-actor"})
		require.NoError(t, err)
		binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
		require.NoError(t, err)
		creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: token, Actor: "new-actor", Capabilities: "pull,push"}
		require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
		_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		hubSource, err := hub.DB.IssueByUID(ctx, source.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		comments, err := hub.DB.CommentsByIssue(ctx, hubSource.ID)
		require.NoError(t, err)
		require.Empty(t, comments)
		events, err := spoke.env.DB.EventsByUIDs(ctx, project.ID, []string{original.UID, reply.UID})
		require.NoError(t, err)
		require.ElementsMatch(t, []db.Event{original, reply}, events)
	})
	t.Run("accepted duplicates", checkRejoinBatchesAlreadyAcceptedOldEvents)
	t.Run("more than one page", checkRejoinPublishesAfter500OldTasks)
	t.Run("workflow and job", checkRejoinContinuesAfterLocalWorkflowAndJob)
	for _, tc := range []struct {
		name       string
		snapshots  bool
		quarantine bool
		race       bool
	}{
		{name: "old actor event"},
		{name: "unsent adoption snapshots", snapshots: true},
		{name: "existing actor quarantine", quarantine: true},
		{name: "comment on missing task", race: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			hub, projects := newMoveDeliveryHub(t, "rejoin-project")
			var spoke *testenv.Env
			var project db.Project
			if tc.snapshots {
				spoke = testenv.New(t)
				var err error
				project, err = spoke.DB.CreateProject(ctx, "rejoin-project")
				require.NoError(t, err)
			} else {
				joined := newMoveDeliverySpoke(t, hub, projects, "old-actor")
				spoke, project = joined.env, joined.projects[0]
			}
			hubBinding, err := hub.DB.FederationBindingByProject(ctx, projects[0].ID)
			require.NoError(t, err)
			var old db.Issue
			var event db.Event
			if tc.snapshots {
				for i := range 3 {
					issue, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "standalone task", Author: "old-actor"})
					require.NoError(t, err)
					if i == 0 {
						old = issue
					}
				}
				adoption, err := spoke.DB.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: hub.URL, HubProjectID: projects[0].ID, HubProjectUID: projects[0].UID, ReplayHorizonEventID: hubBinding.ReplayHorizonEventID, Actor: "old-actor"})
				require.NoError(t, err)
				require.EqualValues(t, 3, adoption.AdoptionSnapshotCount)
			}
			_, err = spoke.DB.LeaveFederationReplica(ctx, project.ID)
			require.NoError(t, err)
			if !tc.snapshots {
				old, event, err = spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
				require.NoError(t, err)
			}
			token := rejoinNewActor(t, hub, projects[0], spoke, project)
			binding, err := spoke.DB.FederationBindingByProject(ctx, project.ID)
			require.NoError(t, err)
			if tc.snapshots {
				pending, err := spoke.DB.PendingFederationPushEvents(ctx, project.ID, spoke.DB.InstanceUID(), binding.PushCursorEventID, 100)
				require.NoError(t, err)
				for _, candidate := range pending {
					if candidate.Type == "issue.snapshot" && candidate.IssueUID != nil && *candidate.IssueUID == old.UID {
						event = candidate
						break
					}
				}
				require.NotEmpty(t, event.UID)
			}
			if tc.quarantine {
				_, err = spoke.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
					ProjectID: project.ID, Direction: db.FederationQuarantineDirectionPush,
					FirstEventID: event.ID, LastEventID: event.ID, EventUIDs: []string{event.UID},
					Error:     fmt.Sprintf(`hub /api/v1/projects/42/federation/events:ingest returned 400: {"status":400,"error":{"code":"validation","message":"federation ingest validation: event %s actor \"old-actor\" does not match bound actor"}}`, event.UID),
					CreatedAt: time.Now().UTC(),
				})
				require.NoError(t, err)
			}
			if tc.race {
				_, _, err = spoke.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: old.ID, Author: "new-actor", Body: "local comment"})
				require.NoError(t, err)
			}
			later, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
			require.NoError(t, err)
			creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: token, Actor: "new-actor", Capabilities: "pull,push"}
			require.NoError(t, SyncFederationOnce(ctx, spoke.DB, binding, creds))
			_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			_, err = hub.DB.IssueByUID(ctx, old.UID, db.IncludeDeletedNo)
			require.ErrorIs(t, err, db.ErrNotFound)
			events, err := spoke.DB.EventsByUIDs(ctx, project.ID, []string{event.UID})
			require.NoError(t, err)
			require.Equal(t, []db.Event{event}, events)

		})
	}
}

func rejoinNewActor(t *testing.T, hub *testenv.Env, hubProject db.Project, spoke *testenv.Env, project db.Project) string {
	t.Helper()
	ctx := t.Context()
	grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.DB.InstanceUID(), ProjectID: &hubProject.ID, Capabilities: "pull,push", Actor: "new-actor"})
	require.NoError(t, err)
	hubBinding, err := hub.DB.FederationBindingByProject(ctx, hubProject.ID)
	require.NoError(t, err)
	var replica api.CreateFederationReplicaResponseBody
	postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
		"hub_url": hub.URL, "hub_project_id": hubProject.ID, "hub_project_uid": hubProject.UID,
		"project_name": project.Name, "replay_horizon_event_id": hubBinding.ReplayHorizonEventID, "token": grant.Token, "capabilities": "pull,push", "actor": "new-actor", "push_enabled": true,
	}, &replica)
	return grant.Token
}
