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
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestRejoinBatchesAlreadyAcceptedOldEvents(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
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
			if missing {
				_, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "unpublished task", Author: "old-actor"})
				require.NoError(t, err)
			}
			_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
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
			creds := config.FederationCredential{HubURL: server.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "pull,push"}
			require.NoError(t, SyncFederationOnce(ctx, spoke.env.DB, binding, creds))
			wantRequests := 1
			if missing {
				wantRequests = 4
			}
			require.EqualValues(t, wantRequests, requests.Load())
			_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			hubIssue, err := hub.DB.IssueByUID(ctx, accepted.UID, db.IncludeDeletedNo)
			require.NoError(t, err)
			comments, err := hub.DB.CommentsByIssue(ctx, hubIssue.ID)
			require.NoError(t, err)
			require.Len(t, comments, 1)
		})
	}
}

func TestRejoinPublishesAfter500OldTasks(t *testing.T) {
	ctx := t.Context()
	hub, projects := newMoveDeliveryHub(t, "rejoin-project")
	spoke := newMoveDeliverySpoke(t, hub, projects, "old-actor")
	project := spoke.projects[0]
	_, err := spoke.env.DB.LeaveFederationReplica(ctx, project.ID)
	require.NoError(t, err)
	earlier, _, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "earlier task", Author: "new-actor"})
	require.NoError(t, err)
	var oldEvent db.Event
	for i := range 500 {
		issue, event, err := spoke.env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "old task", Author: "old-actor"})
		require.NoError(t, err)
		_, _, err = spoke.env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "old-actor", Body: "old comment"})
		require.NoError(t, err)
		if i == 0 {
			oldEvent = event
		}
	}
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
	binding, err := spoke.env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "pull,push"}
	syncCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	started := time.Now()
	require.NoError(t, SyncFederationOnce(syncCtx, spoke.env.DB, binding, creds))
	t.Logf("sync completed in %s", time.Since(started))
	_, err = hub.DB.IssueByUID(ctx, later.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = hub.DB.IssueByUID(ctx, earlier.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	events, err := spoke.env.DB.EventsByUIDs(ctx, project.ID, []string{oldEvent.UID})
	require.NoError(t, err)
	require.Equal(t, oldEvent.ContentHash, events[0].ContentHash)
}

func TestRejoinKeepsOldActorEventsLocal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		snapshots  bool
		quarantine bool
	}{
		{name: "old actor event"},
		{name: "unsent adoption snapshots", snapshots: true},
		{name: "existing actor quarantine", quarantine: true},
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
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.DB.InstanceUID(), ProjectID: &projects[0].ID, Capabilities: "pull,push", Actor: "new-actor"})
			require.NoError(t, err)
			var replica api.CreateFederationReplicaResponseBody
			postJSON(t, spoke.URL, "/api/v1/federation/replicas", map[string]any{
				"hub_url": hub.URL, "hub_project_id": projects[0].ID, "hub_project_uid": projects[0].UID,
				"project_name": project.Name, "replay_horizon_event_id": hubBinding.ReplayHorizonEventID, "token": grant.Token, "capabilities": "pull,push", "actor": "new-actor", "push_enabled": true,
			}, &replica)
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
			later, _, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "later task", Author: "new-actor"})
			require.NoError(t, err)
			creds := config.FederationCredential{HubURL: hub.URL, HubProjectID: projects[0].ID, Token: grant.Token, Actor: "new-actor", Capabilities: "pull,push"}
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
