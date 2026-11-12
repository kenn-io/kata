package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/uid"
)

func rejoinParams(project db.Project, endpoint string) daemon.EnsureFederationReplicaParams {
	p := adoptionParams(project, endpoint)
	p.AdoptExisting = false
	p.HubProjectUID = project.UID
	return p
}

// Rejoin re-offers local-origin history, even after a later summary clears sessions.
func TestRejoinRetainedSessionFeatures(t *testing.T) {
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, tc := range []struct {
			name, features, missing string
			clear, moved            bool
		}{
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
				in := adoptionRun(t, store, project, true)
				if tc.clear {
					in.Summary = cron.Summary{Version: 1}
					_, err = store.ObserveCronRun(t.Context(), in)
					require.NoError(t, err)
				}
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
				if tc.name == "cleared" {
					factory := params.HubClientFactory
					params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) {
						return nil, fmt.Errorf("hub unavailable")
					}
					_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
					require.ErrorIs(t, err, daemon.ErrFederationReplicaHubUnavailable)
					params.HubClientFactory = factory
				}
				_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
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

func TestRejoinWithoutDirectPushOrSessionsDoesNotProbeMetadata(t *testing.T) {
	for _, mode := range []string{"session-free", "pull-only", "relay"} {
		t.Run(mode, func(t *testing.T) {
			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
			require.NoError(t, err)
			adoptionRun(t, store, project, mode != "session-free")
			params := rejoinParams(project, "http://hub.example")
			switch mode {
			case "relay":
				params = relayReplicaParams(t, store)
				params.ProjectName = project.Name
			case "pull-only":
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

func TestRejoinWaitsForCommittedObservation(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
	require.NoError(t, err)
	wrapped := &signaledProjectLockStore{TransactionalStore: dbtest.TransactionalStore{Storage: store}, entered: make(chan struct{})}
	release, err := federationcoord.BeginSync(t.Context(), "other-process-"+federationcoord.Key(store.InstanceUID(), project.ID), wrapped, project.ID)
	require.NoError(t, err)
	t.Cleanup(release)
	params := rejoinParams(project, "http://hub.example")
	done := make(chan error, 1)
	go func() {
		_, e := daemon.EnsureFederationReplica(t.Context(), wrapped, newReplicaCredentialStore(), nil, params)
		done <- e
	}()
	select {
	case err := <-done:
		t.Fatalf("rejoin completed while observation held admission: %v", err)
	case <-wrapped.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("rejoin did not reach the project lock")
	}
	adoptionRun(t, store, project, true)
	release()
	select {
	case err := <-done:
		require.ErrorIs(t, err, daemon.ErrFederationReplicaHubUnavailable, "rejoin must inspect the observation committed before admission released")
	case <-time.After(5 * time.Second):
		t.Fatal("rejoin did not proceed after observation")
	}
}

func TestRejoinHoldsAdmissionDuringProbe(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
	require.NoError(t, err)
	adoptionRun(t, store, project, true)
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
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, e := daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, params)
		done <- e
	}()
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
		<-finished
	})
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("rejoin ended before metadata probe: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("rejoin did not reach metadata probe")
	}
	probeRelease, err := federationcoord.TryBeginSync(t.Context(), federationcoord.Key(store.InstanceUID(), project.ID), store, project.ID)
	if err == nil {
		probeRelease()
	}
	require.ErrorIs(t, err, federationcoord.ErrBusy)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	close(resume)
	err = <-done
	require.NoError(t, err)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
}

func TestRejoinCompatibleHubPushesRetainedSessions(t *testing.T) {
	f := newCronForwardFixture(t, nil)
	_, err := f.spoke.DB.LeaveFederationReplica(t.Context(), f.sp.ID)
	require.NoError(t, err)
	adoptionRunAs(t, f.spoke.DB, f.sp, true, f.credential.Actor)
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
	remote := make(map[string]string, len(hubEvents))
	for _, event := range hubEvents {
		remote[event.UID] = event.Actor
	}
	observations := 0
	for _, local := range pending {
		if local.Type == "cron.run.observed" {
			observations++
			actor, found := remote[local.UID]
			require.Truef(t, found, "retained observation %s was not pushed", local.UID)
			require.Equal(t, local.Actor, actor)
		}
	}
	require.Positive(t, observations)
}

// D24: a rejoin may probe and push with the credential saved under its hub UID.
func TestRejoinEmptyTokenUsesSavedCredential(t *testing.T) {
	for _, compatible := range []bool{true, false} {
		t.Run(fmt.Sprint(compatible), func(t *testing.T) {
			f := newCronForwardFixture(t, nil)
			_, err := f.spoke.DB.LeaveFederationReplica(t.Context(), f.sp.ID)
			require.NoError(t, err)
			adoptionRunAs(t, f.spoke.DB, f.sp, true, f.credential.Actor)
			before, err := f.spoke.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.sp.ID, Limit: 100})
			require.NoError(t, err)
			params := rejoinParams(f.sp, f.credential.HubURL)
			params.HubProjectID = f.hp.ID
			params.Credential = f.credential
			params.Credential.Token = ""
			params.ReplayHorizonEventID = 1
			credentials := newReplicaCredentialStore()
			credentials.put(f.sp.UID, f.credential)
			calls := 0
			params.HubClientFactory = func(ctx context.Context, c config.FederationCredential) (*http.Client, error) {
				calls++
				require.Equal(t, f.credential, c)
				client, e := federation.CredentialHTTPClient(ctx, c)
				if e != nil {
					return nil, e
				}
				transport := client.Transport
				client.Transport = adoptionTransport(func(r *http.Request) (*http.Response, error) {
					response, e := transport.RoundTrip(r)
					if e == nil && !compatible {
						response.Header.Set(db.EventFeaturesHeader, db.CronEventFeature)
					}
					return response, e
				})
				return client, nil
			}
			_, err = daemon.EnsureFederationReplica(t.Context(), f.spoke.DB, credentials, nil, params)
			if !compatible {
				require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
				current, e := f.spoke.DB.ProjectByID(t.Context(), f.sp.ID)
				require.NoError(t, e)
				require.Equal(t, f.sp, current)
				_, e = f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
				require.ErrorIs(t, e, db.ErrNotFound)
				after, e := f.spoke.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.sp.ID, Limit: 100})
				require.NoError(t, e)
				require.Equal(t, before, after)
			} else {
				require.NoError(t, err)
				binding, e := f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
				require.NoError(t, e)
				pending, e := f.spoke.DB.PendingFederationPushEvents(t.Context(), f.sp.ID, f.spoke.DB.InstanceUID(), 0, 100)
				require.NoError(t, e)
				require.NotEmpty(t, pending)
				require.NoError(t, federation.SyncFederationOnce(t.Context(), f.spoke.DB, binding, f.credential))
				binding, e = f.spoke.DB.FederationBindingByProject(t.Context(), f.sp.ID)
				require.NoError(t, e)
				require.GreaterOrEqual(t, binding.PushCursorEventID, pending[len(pending)-1].ID)
				hubEvents, e := f.hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: f.hp.ID, Limit: 100})
				require.NoError(t, e)
				remote := make(map[string]string, len(hubEvents))
				for _, event := range hubEvents {
					remote[event.UID] = event.Actor
				}
				observations := 0
				for _, event := range pending {
					if event.Type == "cron.run.observed" {
						observations++
						actor, found := remote[event.UID]
						require.True(t, found)
						require.Equal(t, event.Actor, actor)
					}
				}
				require.Positive(t, observations)
			}
			require.Equal(t, 1, calls)
			credential, found, err := credentials.FederationCredential(t.Context(), f.sp.UID)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, f.credential, credential)
			require.Zero(t, credentials.storeCalls)
			require.Zero(t, credentials.rekeyCalls)
		})
	}
}
