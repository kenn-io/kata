package daemon_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func concurrentMoveDisplacementFixture(t *testing.T) (*testenv.Env, []db.Project, db.RemoteEvent, string, int64) {
	t.Helper()
	ctx := t.Context()
	env := testenv.New(t, testenv.WithAuthToken("owner-token"))
	projects := make([]db.Project, 3)
	for i, name := range []string{"concurrent-source", "concurrent-middle", "concurrent-target"} {
		projects[i] = createFederatedHubProject(t, env, name)
	}
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "Cached concurrent issue", Author: "tester"})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	grant, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, Capabilities: "push,pull", Actor: "tester"})
	require.NoError(t, err)
	move := func(to int, at time.Time) db.RemoteEvent {
		eventUID, err := uid.New()
		require.NoError(t, err)
		payload, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": projects[0].UID, "from_short_id": issue.ShortID, "to_project_uid": projects[to].UID, "to_short_id": issue.ShortID, "snapshot": map[string]string{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "author": "tester", "status": "open"}})
		require.NoError(t, err)
		event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: origin, ProjectUID: projects[to].UID, ProjectName: projects[to].Name, IssueUID: &issue.UID, Type: "issue.moved", Actor: "tester", Payload: payload, HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
		event.ContentHash, err = db.RemoteEventContentHash(event)
		require.NoError(t, err)
		return event
	}
	at := time.Now().UTC().Add(time.Second).Truncate(time.Millisecond)
	first, second := move(1, at), move(2, at.Add(time.Millisecond))
	_, err = env.DB.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[1].ID, SpokeInstanceUID: origin, BoundActor: "tester", FederationEnrollmentID: grant.Enrollment.ID, EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: first}}})
	require.NoError(t, err)
	_, sentinel, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[1].ID, Title: "Live displaced project sentinel", Author: "tester"})
	require.NoError(t, err)
	return env, projects, second, grant.Token, sentinel.ID
}

func TestFederationConcurrentMoveResetsActualDeparture(t *testing.T) {
	for _, kind := range []string{"poll", "resume", "live"} {
		t.Run(kind, func(t *testing.T) {
			env, projects, move, token, before := concurrentMoveDisplacementFixture(t)
			if move.IssueUID == nil {
				t.Fatal("missing moved issue UID")
			}
			readHeaders := bearer("owner-token")
			readHeaders[requestProjectScopeHeader] = requestScopeValue(t, projects[1].UID)
			var framer *sseFramer
			if kind == "live" {
				response := openSSE(t, env, fmt.Sprintf("project_id=%d&after_id=%d", projects[1].ID, before-1), http.Header{"Authorization": {"Bearer owner-token"}, requestProjectScopeHeader: {readHeaders[requestProjectScopeHeader]}})
				defer func() { _ = response.Body.Close() }()
				require.Equal(t, http.StatusOK, response.StatusCode)
				framer = newSSEFramer(response.Body)
				frame, ok := framer.Next(t, 2*time.Second)
				require.True(t, ok)
				require.Equal(t, strconv.FormatInt(before, 10), frame.id)
			}
			headers := bearer(token)
			headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
			response, raw := envDoRaw(t, env, http.MethodPost, projectPath(projects[2].ID)+"/federation/events:ingest", federationIngestBody(federationIngestEnvelope(t, 2, move)), headers)
			require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
			stored, err := env.DB.EventsByUIDs(t.Context(), projects[2].ID, []string{move.EventUID})
			require.NoError(t, err)
			require.Len(t, stored, 1)
			cursor := stored[0].ID
			require.Equal(t, move.ContentHash, stored[0].ContentHash)
			if kind == "poll" {
				response, raw = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", projects[1].ID, before), nil, readHeaders)
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				var body api.PollEventsBody
				require.NoError(t, json.Unmarshal(raw, &body))
				require.True(t, body.ResetRequired)
				require.Equal(t, cursor, body.ResetAfterID)
				require.Equal(t, cursor, body.NextAfterID)
				require.Empty(t, body.Events)
				require.NotContains(t, string(raw), *move.IssueUID)
				require.NotContains(t, string(raw), projects[2].UID)
				return
			}
			if kind == "resume" {
				response := openSSE(t, env, fmt.Sprintf("project_id=%d", projects[1].ID), http.Header{"Authorization": {"Bearer owner-token"}, requestProjectScopeHeader: {readHeaders[requestProjectScopeHeader]}, "Last-Event-Id": {strconv.FormatInt(before, 10)}})
				defer func() { _ = response.Body.Close() }()
				require.Equal(t, http.StatusOK, response.StatusCode)
				framer = newSSEFramer(response.Body)
			}
			if framer == nil {
				t.Fatal("missing event stream")
			}
			frame, ok := framer.Next(t, 2*time.Second)
			require.True(t, ok, "actual departure must invalidate B before any heartbeat")
			require.Equal(t, "sync.reset_required", frame.event)
			require.Equal(t, strconv.FormatInt(cursor, 10), frame.id)
			require.JSONEq(t, fmt.Sprintf(`{"event_id":%d,"reset_after_id":%d}`, cursor, cursor), frame.data)
			require.NotContains(t, frame.data, *move.IssueUID)
			require.NotContains(t, frame.data, projects[2].UID)
		})
	}
}

func TestFederationPulledConcurrentMoveResetsActualDeparture(t *testing.T) {
	for _, kind := range []string{"poll", "resume", "live"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			hub, projects, move, token, _ := concurrentMoveDisplacementFixture(t)
			spoke := testenv.New(t, testenv.WithAuthToken("owner-token"))
			credentials := newReplicaCredentialStore()
			grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.DB.InstanceUID(), Capabilities: "push,pull", Actor: "tester"})
			require.NoError(t, err)
			local := make([]db.Project, len(projects))
			for i, project := range projects {
				local[i], err = spoke.DB.CreateProjectWithUID(ctx, project.Name, project.UID)
				require.NoError(t, err)
				_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local[i].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
				require.NoError(t, err)
				require.NoError(t, credentials.StoreFederationCredential(ctx, project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token, Actor: "tester"}))
			}
			publisher := daemon.NewEventPublisher(spoke.Broadcaster, nil)
			sync := func(index int) {
				binding, err := spoke.DB.FederationBindingByProject(ctx, local[index].ID)
				require.NoError(t, err)
				credential, _, err := credentials.FederationCredential(ctx, local[index].UID)
				require.NoError(t, err)
				require.NoError(t, federation.SyncFederationOnceWithPulledEvents(ctx, spoke.DB, binding, credential, clientpkg.Opts{}, publisher.Events, credentials))
			}
			for i := range projects {
				sync(i)
			}
			_, sentinel, err := spoke.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: local[1].ID, Title: "Local displaced project sentinel", Author: "tester"})
			require.NoError(t, err)
			before := sentinel.ID
			readHeaders := bearer("owner-token")
			readHeaders[requestProjectScopeHeader] = requestScopeValue(t, local[1].UID)
			var framer *sseFramer
			if kind == "live" {
				response := openSSE(t, spoke, fmt.Sprintf("project_id=%d&after_id=%d", local[1].ID, before-1), http.Header{"Authorization": {"Bearer owner-token"}, requestProjectScopeHeader: {readHeaders[requestProjectScopeHeader]}})
				defer func() { _ = response.Body.Close() }()
				require.Equal(t, http.StatusOK, response.StatusCode)
				framer = newSSEFramer(response.Body)
				frame, ok := framer.Next(t, 2*time.Second)
				require.True(t, ok)
				require.Equal(t, strconv.FormatInt(before, 10), frame.id)
			}
			headers := bearer(token)
			headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
			response, raw := envDoRaw(t, hub, http.MethodPost, projectPath(projects[2].ID)+"/federation/events:ingest", federationIngestBody(federationIngestEnvelope(t, 2, move)), headers)
			require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
			sync(2)
			if kind == "poll" {
				response, raw = envDoRaw(t, spoke, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", local[1].ID, before), nil, readHeaders)
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				var body api.PollEventsBody
				require.NoError(t, json.Unmarshal(raw, &body))
				require.True(t, body.ResetRequired)
				require.Greater(t, body.ResetAfterID, before)
				require.Empty(t, body.Events)
				require.NotContains(t, string(raw), local[2].UID)
				return
			}
			if kind == "resume" {
				response := openSSE(t, spoke, fmt.Sprintf("project_id=%d", local[1].ID), http.Header{"Authorization": {"Bearer owner-token"}, requestProjectScopeHeader: {readHeaders[requestProjectScopeHeader]}, "Last-Event-Id": {strconv.FormatInt(before, 10)}})
				defer func() { _ = response.Body.Close() }()
				require.Equal(t, http.StatusOK, response.StatusCode)
				framer = newSSEFramer(response.Body)
			}
			if framer == nil {
				t.Fatal("missing pulled event stream")
			}
			frame, ok := framer.Next(t, 2*time.Second)
			require.True(t, ok, "trusted materialization must invalidate its actual source immediately")
			require.Equal(t, "sync.reset_required", frame.event)
			cursor, err := strconv.ParseInt(frame.id, 10, 64)
			require.NoError(t, err)
			require.Greater(t, cursor, before)
			require.JSONEq(t, fmt.Sprintf(`{"event_id":%d,"reset_after_id":%d}`, cursor, cursor), frame.data)
			require.NotContains(t, frame.data, local[2].UID)
		})
	}
}

func TestEventPublisherMaterializationResetSkipsHooks(t *testing.T) {
	broadcaster := daemon.NewEventBroadcaster()
	sub := broadcaster.Subscribe(daemon.SubFilter{ProjectID: 7})
	defer sub.Unsub()
	sink := &publisherSink{}
	publisher := daemon.NewEventPublisher(broadcaster, sink)
	publisher.Event(7, db.Event{ID: 42, ProjectID: 7, Type: db.ProjectScopeResetEventType})
	message := receiveMsg(t, sub.Ch, time.Second, "materialization reset")
	require.Equal(t, daemon.StreamKindReset, message.Kind)
	require.Equal(t, int64(42), message.ResetID)
	require.Nil(t, message.Event, "control publication carries only project and cursor")
	require.Empty(t, sink.ids(), "reset controls are not hook mutations")
}
