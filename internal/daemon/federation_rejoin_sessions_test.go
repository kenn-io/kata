package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func rejoinSessionRun(t *testing.T, store db.Storage, project db.Project, clear bool) {
	t.Helper()
	definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "tester", Definition: definition})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	sessions := []cron.Session{{Agent: "claude", SessionID: "00000000-0000-4000-8000-000000000001"}}
	in := db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: "tester", Status: "succeeded", Summary: cron.Summary{Version: 1, Sessions: &sessions}}
	observed, err := store.ObserveCronRun(t.Context(), in)
	require.NoError(t, err)
	if clear {
		in.Summary = cron.Summary{Version: 1}
		in.ExpectedRevision = observed.Run.Revision
		_, err = store.ObserveCronRun(t.Context(), in)
		require.NoError(t, err)
	}
}

func rejoinParams(project db.Project, endpoint string) daemon.EnsureFederationReplicaParams {
	p := replicaServiceParams()
	p.ProjectName = project.Name
	p.HubProjectUID = project.UID
	p.HubURL = endpoint
	p.Credential.HubURL = endpoint
	return p
}

// Rejoin re-offers local-origin history, even after a later summary clears sessions.
func TestRejoinRetainedSessionFeatures(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, tc := range []struct {
			name, features, missing string
			clear, moved            bool
		}{
			{"current", db.CronEventFeature, db.CronRunSessionsEventFeature, false, false},
			{"cleared", db.CronEventFeature, db.CronRunSessionsEventFeature, true, false},
			{"missing cron", db.CronRunSessionsEventFeature, db.CronEventFeature, false, false},
			{"missing move", db.MergeEventFeatures(db.CronEventFeature, db.CronRunSessionsEventFeature), db.IssueMoveEventFeature, false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				projectUID, err := uid.New()
				require.NoError(t, err)
				project, err := store.CreateProjectWithUID(t.Context(), "spoke-"+projectUID, projectUID)
				require.NoError(t, err)
				if tc.moved {
					source, e := store.CreateProject(t.Context(), "source-project")
					require.NoError(t, e)
					issue, _, e := store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, Title: "Example", Author: "worker"})
					require.NoError(t, e)
					_, e = store.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: project.ID, IfMatchRev: issue.Revision, Actor: "worker"})
					require.NoError(t, e)
				}
				rejoinSessionRun(t, store, project, tc.clear)
				before, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
				var features atomic.Value
				features.Store(tc.features)
				var gets atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					gets.Add(1)
					w.Header().Set(db.EventFeaturesHeader, features.Load().(string))
					_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, project.UID)
				}))
				t.Cleanup(server.Close)
				params := rejoinParams(project, server.URL)
				params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) { return server.Client(), nil }
				credentials := newReplicaCredentialStore()
				credentials.put(project.UID, params.Credential)
				_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
				require.Error(t, err)
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				require.Contains(t, err.Error(), tc.missing)
				require.EqualValues(t, 1, gets.Load())
				current, err := store.ProjectByID(t.Context(), project.ID)
				require.NoError(t, err)
				require.Equal(t, project, current)
				_, err = store.FederationBindingByProject(t.Context(), project.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
				credential, found, err := credentials.FederationCredential(t.Context(), project.UID)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, params.Credential, credential)
				require.Zero(t, credentials.storeCalls)
				require.Zero(t, credentials.rekeyCalls)
				after, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
				require.Equal(t, before, after)
				features.Store(db.SupportedEventFeatures)
				result, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
				require.NoError(t, err)
				require.Equal(t, project.UID, result.Project.UID)
				binding, err := store.FederationBindingByProject(t.Context(), project.ID)
				require.NoError(t, err)
				require.True(t, binding.PushEnabled)
				require.Zero(t, binding.PushCursorEventID)
			})
		}
	})
}

func TestRejoinMetadataFailurePreservesState(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
			require.NoError(t, err)
			rejoinSessionRun(t, store, project, true)
			before, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
				_, _ = fmt.Fprint(w, `{"project_id":42,"project_uid":"wrong"}`)
			}))
			t.Cleanup(server.Close)
			params := rejoinParams(project, server.URL)
			params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) {
				if !mismatch {
					return nil, fmt.Errorf("hub unavailable")
				}
				return server.Client(), nil
			}
			credentials := newReplicaCredentialStore()
			credentials.put(project.UID, params.Credential)
			_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
			require.Error(t, err)
			current, err := store.ProjectByID(t.Context(), project.ID)
			require.NoError(t, err)
			require.Equal(t, project, current)
			_, err = store.FederationBindingByProject(t.Context(), project.ID)
			require.ErrorIs(t, err, db.ErrNotFound)
			credential, found, err := credentials.FederationCredential(t.Context(), project.UID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, params.Credential, credential)
			require.Zero(t, credentials.storeCalls)
			require.Zero(t, credentials.rekeyCalls)
			after, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestRejoinWithoutSessionsDoesNotProbeMetadata(t *testing.T) {
	for _, hasCron := range []bool{false, true} {
		t.Run(fmt.Sprint(hasCron), func(t *testing.T) {
			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
			require.NoError(t, err)
			if hasCron {
				adoptionRun(t, store, project, false)
			}
			var calls atomic.Int32
			params := rejoinParams(project, "http://hub.example")
			params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) {
				calls.Add(1)
				return nil, fmt.Errorf("unexpected metadata request")
			}
			_, err = daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, params)
			require.NoError(t, err)
			require.Zero(t, calls.Load())
		})
	}
}

func TestRejoinWithoutDirectPushDoesNotProbeMetadata(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprint(relay), func(t *testing.T) {
			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
			require.NoError(t, err)
			rejoinSessionRun(t, store, project, false)
			params := rejoinParams(project, "http://hub.example")
			if relay {
				params = relayReplicaParams(t, store)
				params.ProjectName = project.Name
			} else {
				params.PushEnabled = false
			}
			var calls atomic.Int32
			params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) {
				calls.Add(1)
				return nil, fmt.Errorf("unexpected metadata request")
			}
			_, err = daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, params)
			require.NoError(t, err)
			require.Zero(t, calls.Load())
		})
	}
}

func TestRejoinWaitsForObservationAdmission(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
	require.NoError(t, err)
	release, err := federationcoord.BeginSync(t.Context(), federationcoord.Key(store.InstanceUID(), project.ID), store, project.ID)
	require.NoError(t, err)
	t.Cleanup(release)
	params := rejoinParams(project, "http://hub.example")
	done := make(chan error, 1)
	go func() {
		_, e := daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, params)
		done <- e
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
waitForRejoin:
	for {
		select {
		case err := <-done:
			t.Fatalf("rejoin completed while observation held admission: %v", err)
		case <-deadline.C:
			t.Fatal("rejoin did not begin waiting for admission")
		default:
		}
		probeRelease, probeErr := federationcoord.TryBeginSync(t.Context(), federationcoord.Key(store.InstanceUID(), project.ID), store, project.ID)
		if probeErr != nil {
			require.ErrorIs(t, probeErr, federationcoord.ErrBusy)
			break waitForRejoin
		}
		probeRelease()
		runtime.Gosched()
	}
	rejoinSessionRun(t, store, project, true)
	release()
	select {
	case err := <-done:
		require.Error(t, err, "rejoin must inspect the observation committed before admission released")
	case <-time.After(5 * time.Second):
		t.Fatal("rejoin did not proceed after observation")
	}
}

func TestRejoinExcludesNewObservationUntilBinding(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
	require.NoError(t, err)
	rejoinSessionRun(t, store, project, false)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Other review", Actor: "tester", Definition: definition})
	require.NoError(t, err)
	entered, resume := make(chan struct{}), make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-resume
		w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
		_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, project.UID)
	}))
	t.Cleanup(hub.Close)
	params := rejoinParams(project, hub.URL)
	params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) { return hub.Client(), nil }
	done := make(chan error, 1)
	go func() {
		_, e := daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, params)
		done <- e
	}()
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		<-done
	})
	select {
	case <-entered:
	case err := <-done:
		done <- err
		t.Fatalf("rejoin ended before metadata probe: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("rejoin did not reach metadata probe")
	}
	srv := daemon.NewServer(forwardingTestConfig(daemon.ServerConfig{DB: store}))
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	endpoint := httptest.NewServer(srv.Handler())
	t.Cleanup(endpoint.Close)
	target := &testenv.Env{DB: store, URL: endpoint.URL, HTTP: endpoint.Client()}
	runUID := cronHTTPUID(t)
	body := map[string]any{"job_uid": job.UID, "definition_event_uid": job.DefinitionEventUID, "status": "running", "expected_revision": 0, "summary": map[string]any{"version": 1, "sessions_incomplete": true}}
	response, raw := envDoRaw(t, target, http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s", project.ID, runUID), body, nil)
	require.Equalf(t, http.StatusServiceUnavailable, response.StatusCode, "%s", raw)
	_, err = store.CronRun(t.Context(), project.ID, runUID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	close(resume)
	err = <-done
	done <- err
	require.NoError(t, err)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
}

func TestRejoinCompatibleHubPushesRetainedSessions(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	_, err := f.spoke.DB.LeaveFederationReplica(t.Context(), f.sp.ID)
	require.NoError(t, err)
	rejoinSessionRun(t, f.spoke.DB, f.sp, true)
	params := rejoinParams(f.sp, f.credential.HubURL)
	params.HubProjectID = f.hp.ID
	params.Credential = f.credential
	params.ReplayHorizonEventID = 1
	params.HubClientFactory = federation.CredentialHTTPClient
	_, err = daemon.EnsureFederationReplica(t.Context(), f.spoke.DB, newReplicaCredentialStore(), nil, params)
	require.NoError(t, err)
	binding, err := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
	require.NoError(t, err)
	pending, err := f.spoke.DB.PendingFederationPushEvents(t.Context(), f.sp.ID, f.spoke.DB.InstanceUID(), 0, 100)
	require.NoError(t, err)
	require.NotEmpty(t, pending)
	require.NoError(t, federation.SyncFederationOnce(t.Context(), f.spoke.DB, binding, f.credential))
	binding, err = f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, binding.PushCursorEventID, pending[len(pending)-1].ID)
	hubEvents, err := f.hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.hp.ID, Limit: 100})
	require.NoError(t, err)
	for _, local := range pending {
		if local.Type != "cron.run.observed" {
			continue
		}
		found := false
		for _, remote := range hubEvents {
			if remote.UID == local.UID {
				found = true
				require.Equal(t, local.Actor, remote.Actor)
			}
		}
		require.True(t, found, "retained observation %s was not pushed", local.UID)
	}
}
