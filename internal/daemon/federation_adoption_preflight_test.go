package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationsigning/signingpolicy"
	"go.kenn.io/kata/internal/uid"
)

func adoptionRun(t *testing.T, store db.Storage, project db.Project, sessions bool) db.ObserveCronRun {
	t.Helper()
	return adoptionRunAs(t, store, project, sessions, "worker")
}

// adoptionRunAs records the run's local events as actor, so a test that pushes
// them to a hub can use the actor its enrollment is bound to.
func adoptionRunAs(t *testing.T, store db.Storage, project db.Project, sessions bool, actor string) db.ObserveCronRun {
	t.Helper()
	definition := cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}
	job, _, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: actor, Definition: definition})
	require.NoError(t, err)
	runUID, err := uid.New()
	require.NoError(t, err)
	summary := cron.Summary{Version: 1}
	if sessions {
		marker := true
		summary.SessionsIncomplete = &marker
	}
	in := db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Actor: actor, Status: "succeeded", Summary: summary}
	observed, err := store.ObserveCronRun(t.Context(), in)
	require.NoError(t, err)
	in.ExpectedRevision = observed.Run.Revision
	return in
}

func adoptionParams(project db.Project, hubURL string) daemon.EnsureFederationReplicaParams {
	params := replicaServiceParams()
	params.ProjectName = project.Name
	params.AdoptExisting = true
	params.HubURL = hubURL
	params.Credential.HubURL = hubURL
	return params
}

type adoptionTransport func(*http.Request) (*http.Response, error)

func (a adoptionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return a(r) }

func TestAdoptionPreflightBeforeRekeyAndSessionFree(t *testing.T) {
	for _, session := range []bool{false, true} {
		t.Run(fmt.Sprint(session), func(t *testing.T) {
			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaLocalProjectUID)
			require.NoError(t, err)
			adoptionRun(t, store, project, session)
			gets := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gets++
				w.Header().Set(db.EventFeaturesHeader, db.CronEventFeature)
				_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, replicaHubProjectUID)
			}))
			t.Cleanup(server.Close)
			params := adoptionParams(project, server.URL)
			credentials := newReplicaCredentialStore()
			source := params.Credential
			source.Capabilities = "pull,push"
			credentials.put(project.UID, source)
			params.HubClientFactory = func(_ context.Context, c config.FederationCredential) (*http.Client, error) {
				require.Equal(t, params.Credential.Token, c.Token)
				return server.Client(), nil
			}
			_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
			if session {
				require.Error(t, err)
				require.Contains(t, err.Error(), "cron_run_sessions_v1")
				require.Equal(t, 1, gets)
				require.Zero(t, credentials.rekeyCalls)
				current, e := store.ProjectByID(t.Context(), project.ID)
				require.NoError(t, e)
				require.Equal(t, project.UID, current.UID)
				_, e = store.FederationBindingByProject(t.Context(), project.ID)
				require.ErrorIs(t, e, db.ErrNotFound)
			} else {
				require.NoError(t, err)
				require.Zero(t, gets)
			}
		})
	}
}

type lateSessionStore struct {
	*sqlitestore.Store
	before func()
}

func (s *lateSessionStore) AdoptProjectIntoFederation(ctx context.Context, p db.AdoptProjectIntoFederationParams) (db.AdoptProjectIntoFederationResult, error) {
	if s.before != nil {
		f := s.before
		s.before = nil
		f()
	}
	return s.Store.AdoptProjectIntoFederation(ctx, p)
}

func TestAdoptionLateSessionRefusesAndCompatibleRetry(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
		_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, replicaHubProjectUID)
	}))
	t.Cleanup(server.Close)
	params := adoptionParams(project, server.URL)
	params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) {
		return server.Client(), nil
	}
	credentials := newReplicaCredentialStore()
	source := params.Credential
	source.Capabilities = "pull,push"
	credentials.put(project.UID, source)
	var beforeEvents []db.Event
	var beforeCursor int64
	wrapped := &lateSessionStore{Store: store, before: func() {
		adoptionRun(t, store, project, true)
		beforeEvents, err = store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
		require.NoError(t, err)
		beforeCursor, err = store.MaxEventID(t.Context())
		require.NoError(t, err)
	}}
	_, err = daemon.EnsureFederationReplica(t.Context(), wrapped, credentials, nil, params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported event features")
	current, err := store.ProjectByID(t.Context(), project.ID)
	require.NoError(t, err)
	require.Equal(t, project.UID, current.UID)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	afterEvents, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, beforeEvents, afterEvents)
	afterCursor, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, beforeCursor, afterCursor)
	_, err = daemon.EnsureFederationReplica(t.Context(), wrapped, credentials, nil, params)
	require.NoError(t, err)
}

func TestAdoptionRelaySessionsRefusedWithoutHubProbe(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	adoptionRun(t, store, project, true)
	params := relayReplicaParams(t, store)
	params.ProjectName = project.Name
	params.AdoptExisting = true
	calls := 0
	params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) { calls++; return nil, nil }
	_, err = daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, params)
	require.ErrorIs(t, err, db.ErrFederatedReadOnly)
	require.Zero(t, calls)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestAdoptionPreflightUsesEffectiveSigningAndChecksIdentity(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	adoptionRun(t, store, project, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
		_, _ = fmt.Fprint(w, `{"project_id":42,"project_uid":"wrong"}`)
	}))
	t.Cleanup(server.Close)
	params := adoptionParams(project, server.URL)
	existing := params.Credential
	existing.Capabilities = "pull,push"
	existing.Signing = &signingpolicy.Source{KeyID: "example", HubURL: server.URL}
	credentials := newReplicaCredentialStore()
	credentials.put(replicaHubProjectUID, existing)
	calls := 0
	params.HubClientFactory = func(_ context.Context, c config.FederationCredential) (*http.Client, error) {
		calls++
		require.Equal(t, existing.Signing, c.Signing)
		require.Equal(t, existing.Token, c.Token)
		return server.Client(), nil
	}
	_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
	require.ErrorIs(t, err, daemon.ErrFederationReplicaBindingConflict)
	require.Equal(t, 1, calls)
	require.Zero(t, credentials.rekeyCalls)
	require.Zero(t, credentials.storeCalls)
}

func TestAdoptionHubProbeDoesNotHoldReplicaMutex(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	adoptionRun(t, store, project, true)
	entered, resume := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-resume
		w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
		_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, replicaHubProjectUID)
	}))
	t.Cleanup(server.Close)
	params := adoptionParams(project, server.URL)
	params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) { return server.Client(), nil }
	done := make(chan error, 1)
	go func() {
		_, e := daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, params)
		done <- e
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("hub probe did not start")
	}
	other := replicaServiceParams()
	other.HubProjectUID = "00000000000000000000000009"
	other.ProjectName = "other-project"
	other.HubProjectID = 43
	other.Credential.HubProjectID = 43
	completed := make(chan error, 1)
	go func() {
		_, e := daemon.EnsureFederationReplica(t.Context(), store, newReplicaCredentialStore(), nil, other)
		completed <- e
	}()
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("unrelated replica blocked by hub probe")
	}
	close(resume)
	require.NoError(t, <-done)
}

func TestAdoptionSameUIDSessionBaselineAndBoundRejoin(t *testing.T) {
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(t.Context(), "spoke-project", replicaHubProjectUID)
	require.NoError(t, err)
	adoptionRun(t, store, project, true)
	gets := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		gets++
		w.Header().Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
		_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, replicaHubProjectUID)
	}))
	t.Cleanup(server.Close)
	params := adoptionParams(project, server.URL)
	params.HubClientFactory = func(context.Context, config.FederationCredential) (*http.Client, error) { return server.Client(), nil }
	credentials := newReplicaCredentialStore()
	result, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
	require.NoError(t, err)
	require.True(t, result.Adopted)
	require.Equal(t, 1, gets)
	_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
	require.NoError(t, err)
	require.Equal(t, 1, gets)
}

// D24: omitted enrollment tokens use the saved hub credential without writing it.
func TestAdoptionPreflightEmptyTokenUsesSavedCredential(t *testing.T) {
	t.Setenv("TEST_REPLICA_SIGNING_KEY", strings.Repeat("k", 64))
	projectAccessBackends(t, func(t *testing.T, store db.Storage) {
		for _, name := range []string{"supporting hub", "unsupporting hub", "explicit signing", "replaced during probe", "removed during probe", "no saved credential", "different origin", "different project", "session-free"} {
			t.Run(name, func(t *testing.T) {
				projectUID, err := uid.New()
				require.NoError(t, err)
				hubUID, err := uid.New()
				require.NoError(t, err)
				project, err := store.CreateProjectWithUID(t.Context(), "spoke-"+projectUID, projectUID)
				require.NoError(t, err)
				adoptionRun(t, store, project, name != "session-free")
				before, err := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
				require.NoError(t, err)
				cursor, err := store.MaxEventID(t.Context())
				require.NoError(t, err)
				runs, err := store.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
				require.NoError(t, err)
				credentials := newReplicaCredentialStore()
				var calls atomic.Int32
				var saved, replacement config.FederationCredential
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					switch name {
					case "replaced during probe":
						credentials.put(hubUID, replacement)
					case "removed during probe":
						_ = credentials.DeleteFederationCredential(t.Context(), hubUID)
					}
					features := db.SupportedEventFeatures
					if name == "unsupporting hub" {
						features = db.CronEventFeature
					}
					w.Header().Set(db.EventFeaturesHeader, features)
					_, _ = fmt.Fprintf(w, `{"project_id":42,"project_uid":%q}`, hubUID)
				}))
				t.Cleanup(server.Close)
				params := adoptionParams(project, server.URL)
				params.HubProjectUID = hubUID
				saved = params.Credential
				saved.Signing = &signingpolicy.Source{KeyID: "saved", KeyEnv: "TEST_REPLICA_SIGNING_KEY", HubURL: server.URL}
				if name == "different origin" {
					saved.HubURL = "http://other.example"
				}
				if name == "different project" {
					saved.HubProjectID++
				}
				replacement = saved
				replacement.Token = "replacement-token"
				if name != "no saved credential" && name != "session-free" {
					credentials.put(hubUID, saved)
				}
				params.Credential.Token = ""
				if name == "explicit signing" {
					params.Credential.Signing = &signingpolicy.Source{KeyID: "requested", KeyEnv: "TEST_REPLICA_SIGNING_KEY", HubURL: server.URL}
				}
				params.HubClientFactory = func(_ context.Context, c config.FederationCredential) (*http.Client, error) {
					require.Equal(t, saved, c)
					return server.Client(), nil
				}
				_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
				success := name == "supporting hub" || name == "explicit signing" || name == "session-free"
				if success {
					require.NoError(t, err)
				} else {
					if name == "unsupporting hub" {
						require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
					} else {
						require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
					}
					if name == "replaced during probe" || name == "removed during probe" {
						require.Contains(t, err.Error(), "federation credential changed while contacting the hub")
					}
					current, e := store.ProjectByID(t.Context(), project.ID)
					require.NoError(t, e)
					require.Equal(t, project, current)
					_, e = store.FederationBindingByProject(t.Context(), project.ID)
					require.ErrorIs(t, e, db.ErrNotFound)
					after, e := store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
					require.NoError(t, e)
					require.Equal(t, before, after)
					afterCursor, e := store.MaxEventID(t.Context())
					require.NoError(t, e)
					require.Equal(t, cursor, afterCursor)
					afterRuns, e := store.ListCronRuns(t.Context(), db.CronRunList{ProjectID: project.ID})
					require.NoError(t, e)
					require.Equal(t, runs, afterRuns)
				}
				if name == "no saved credential" || name == "different origin" || name == "different project" || name == "session-free" {
					require.Zero(t, calls.Load())
				} else {
					require.EqualValues(t, 1, calls.Load())
				}
				currentCredential, found, err := credentials.FederationCredential(t.Context(), hubUID)
				require.NoError(t, err)
				if name == "no saved credential" || name == "session-free" || name == "removed during probe" {
					require.False(t, found)
				} else {
					require.True(t, found)
					if name == "replaced during probe" {
						require.Equal(t, replacement, currentCredential)
					} else {
						require.Equal(t, saved, currentCredential)
					}
				}
				require.Zero(t, credentials.storeCalls)
				require.Zero(t, credentials.rekeyCalls)
			})
		}
	})
}
