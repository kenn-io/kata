package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
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

func TestFederatedDelayedSourceEventNotifiesCurrentProject(t *testing.T) {
	for _, mode := range []string{"native", "pull"} {
		for _, kind := range []string{"poll", "live", "poll-after-destination-sync"} {
			t.Run(mode+"-"+kind, func(t *testing.T) {
				if mode == "native" && kind == "poll-after-destination-sync" {
					t.Skip("destination reset is consumed by a native spoke")
				}
				ctx := t.Context()
				hub, projects, move, token, before := concurrentMoveDisplacementFixture(t)
				if move.IssueUID == nil {
					t.Fatal("missing issue UID")
				}
				env := hub
				currentProject := projects[1]
				var pull, syncCurrent func()
				if mode == "pull" {
					env = testenv.New(t, testenv.WithAuthToken("owner-token"))
					credentials := newReplicaCredentialStore()
					grant, err := hub.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: env.DB.InstanceUID(), Capabilities: "push,pull", Actor: "tester"})
					require.NoError(t, err)
					locals := make([]db.Project, len(projects))
					for i, project := range projects {
						locals[i], err = env.DB.CreateProjectWithUID(ctx, project.Name, project.UID)
						require.NoError(t, err)
						_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: locals[i].ID, Role: db.FederationRoleSpoke, HubURL: hub.URL, HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "tester"})
						require.NoError(t, err)
						require.NoError(t, credentials.StoreFederationCredential(ctx, project.UID, config.FederationCredential{HubURL: hub.URL, HubProjectID: project.ID, Token: grant.Token, Actor: "tester"}))
					}
					publisher := daemon.NewEventPublisher(env.Broadcaster, nil)
					sync := func(index int) {
						binding, err := env.DB.FederationBindingByProject(ctx, locals[index].ID)
						require.NoError(t, err)
						credential, _, err := credentials.FederationCredential(ctx, locals[index].UID)
						require.NoError(t, err)
						require.NoError(t, federation.SyncFederationOnceWithPulledEvents(ctx, env.DB, binding, credential, clientpkg.Opts{}, publisher.Events, credentials))
					}
					for i := range projects {
						sync(i)
					}
					currentProject = locals[1]
					before, err = env.DB.MaxEventID(ctx)
					require.NoError(t, err)
					pull = func() { sync(0) }
					syncCurrent = func() { sync(1) }
				}
				sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: currentProject.ID})
				defer sub.Unsub()
				eventUID, err := uid.New()
				require.NoError(t, err)
				at := move.CreatedAt.Add(time.Millisecond)
				event := db.RemoteEvent{EventUID: eventUID, OriginInstanceUID: move.OriginInstanceUID, ProjectUID: projects[0].UID, ProjectName: projects[0].Name, IssueUID: move.IssueUID, Type: "issue.updated", Actor: "tester", Payload: jsontext.Value(`{"title":"delayed source title"}`), HLCPhysicalMS: at.UnixMilli(), CreatedAt: at}
				event.ContentHash, err = db.RemoteEventContentHash(event)
				require.NoError(t, err)
				headers := bearer(token)
				headers[db.EventFeaturesHeader] = db.SupportedEventFeatures
				response, raw := envDoRaw(t, hub, http.MethodPost, projectPath(projects[0].ID)+"/federation/events:ingest", federationIngestBody(federationIngestEnvelope(t, 2, event)), headers)
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				if pull != nil {
					pull()
				}
				wantTitle := "delayed source title"
				if kind == "poll-after-destination-sync" {
					if syncCurrent == nil || pull == nil {
						t.Fatal("missing spoke sync")
					}
					priorBinding, err := env.DB.FederationBindingByProject(ctx, currentProject.ID)
					require.NoError(t, err)
					syncCurrent()
					afterBinding, err := env.DB.FederationBindingByProject(ctx, currentProject.ID)
					require.NoError(t, err)
					require.Equal(t, priorBinding.ReplayHorizonEventID, afterBinding.ReplayHorizonEventID, "content invalidation must not reset portable history")
					before, err = env.DB.MaxEventID(ctx)
					require.NoError(t, err)
					event.EventUID, err = uid.New()
					require.NoError(t, err)
					wantTitle = "second delayed source title"
					event.Payload = jsontext.Value(`{"title":"second delayed source title"}`)
					event.HLCPhysicalMS++
					event.CreatedAt = event.CreatedAt.Add(time.Millisecond)
					event.ContentHash, err = db.RemoteEventContentHash(event)
					require.NoError(t, err)
					response, raw = envDoRaw(t, hub, http.MethodPost, projectPath(projects[0].ID)+"/federation/events:ingest", federationIngestBody(federationIngestEnvelope(t, 3, event)), headers)
					require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
					pull()
				}
				got, err := env.DB.IssueByUID(ctx, *move.IssueUID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, currentProject.ID, got.ProjectID)
				require.Equal(t, wantTitle, got.Title)
				if kind == "live" {
					message := receiveMsg(t, sub.Ch, time.Second, "delayed source reset")
					require.Equal(t, daemon.StreamKindReset, message.Kind)
					require.Greater(t, message.ResetID, before)
					require.Nil(t, message.Event)
					return
				}
				readHeaders := bearer("owner-token")
				readHeaders[requestProjectScopeHeader] = requestScopeValue(t, currentProject.UID)
				response, raw = envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/events?after_id=%d", currentProject.ID, before), nil, readHeaders)
				require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
				var body api.PollEventsBody
				require.NoError(t, json.Unmarshal(raw, &body))
				require.True(t, body.ResetRequired)
				require.Greater(t, body.ResetAfterID, before)
				require.Empty(t, body.Events)
				require.NotContains(t, string(raw), *move.IssueUID)
			})
		}
	}
}
