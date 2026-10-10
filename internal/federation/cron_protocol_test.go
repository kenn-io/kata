package federation_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"go.kenn.io/kata/internal/config"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func nativeStore(t *testing.T, backend string) db.Storage {
	t.Helper()
	var store db.Storage
	var err error
	if backend == "sqlite" {
		store, err = sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	} else {
		dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
		t.Cleanup(cleanup)
		store, err = pgstore.Open(t.Context(), dsn)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}
func nativeJob(t *testing.T, store db.Storage, project db.Project) (db.CronJob, db.Event) {
	t.Helper()
	job, event, err := store.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Review", Actor: "worker", Definition: cron.JobDefinition{Version: 1, Kind: "job", Trigger: cron.Trigger{Kind: "manual"}, Action: cron.Action{Kind: "execute", Prompt: "Review"}, Issue: &cron.IssuePolicy{Kind: "per-run", Title: "Review"}, Overlap: "forbid", Catchup: "skip"}})
	require.NoError(t, err)
	return job, event[0]
}
func nativeHTTP(t *testing.T, store db.Storage) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(daemon.NewServer(daemon.ServerConfig{DB: store, Broadcaster: daemon.NewEventBroadcaster()}).Handler())
	t.Cleanup(server.Close)
	return server
}
func nativeRequest(t *testing.T, url, token, features, method string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	if features != "" {
		req.Header.Set("X-Kata-Event-Features", features)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, data
}

func TestCronFederationFeatureGate(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			store := nativeStore(t, backend)
			server := nativeHTTP(t, store)
			project, err := store.CreateProject(t.Context(), "hub-project")
			require.NoError(t, err)
			binding, err := store.EnableProjectFederation(t.Context(), project.ID, "worker")
			require.NoError(t, err)
			origin, err := uid.New()
			require.NoError(t, err)
			grant, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "pull,push", Actor: "worker"})
			require.NoError(t, err)
			path := fmt.Sprintf("%s/api/v1/projects/%d/federation", server.URL, project.ID)
			for _, route := range []string{"/metadata", "/events?after_id=0"} {
				resp, data := nativeRequest(t, path+route, grant.Token, "", http.MethodGet, nil)
				require.Equal(t, 200, resp.StatusCode, string(data))
			}
			binding.Enabled = false
			_, err = store.UpsertFederationBinding(t.Context(), binding)
			require.NoError(t, err)
			resp, data := nativeRequest(t, path+"/metadata", grant.Token, "", http.MethodGet, nil)
			require.Equal(t, http.StatusForbidden, resp.StatusCode, string(data))
			require.Contains(t, string(data), "auth_invalid")
			binding.Enabled = true
			_, err = store.UpsertFederationBinding(t.Context(), binding)
			require.NoError(t, err)
			nativeJob(t, store, project)
			before, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			for _, features := range []string{"", "unknown_v1"} {
				for _, route := range []string{"/metadata", "/events?after_id=0"} {
					resp, data := nativeRequest(t, path+route, grant.Token, features, http.MethodGet, nil)
					require.Equal(t, 409, resp.StatusCode, string(data))
					require.Contains(t, string(data), "unsupported_event_features")
				}
				resp, data := nativeRequest(t, path+"/events:ingest", grant.Token, features, http.MethodPost, []byte(`{"schema_version":30,"events":[]}`))
				require.Equal(t, 409, resp.StatusCode, string(data))
				require.Contains(t, string(data), "unsupported_event_features")
			}
			after, err := store.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, before, after, "refusal cannot create a baseline")
			resp, data = nativeRequest(t, path+"/metadata", grant.Token, "cron_v1", http.MethodGet, nil)
			require.Equal(t, 200, resp.StatusCode, string(data))
			require.Equal(t, "cron_v1", resp.Header.Get("X-Kata-Required-Event-Features"))
			require.Equal(t, "cron_v1", resp.Header.Get("X-Kata-Event-Features"))
		})
	}
}

func TestCronOldPollRacesFirstCommit(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			store := nativeStore(t, backend)
			server := nativeHTTP(t, store)
			for iteration := range 8 {
				project, err := store.CreateProject(t.Context(), fmt.Sprintf("race-project-%d", iteration))
				require.NoError(t, err)
				_, err = store.EnableProjectFederation(t.Context(), project.ID, "worker")
				require.NoError(t, err)
				origin, err := uid.New()
				require.NoError(t, err)
				grant, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: origin, ProjectID: &project.ID, Capabilities: "pull", Actor: "worker"})
				require.NoError(t, err)
				path := fmt.Sprintf("%s/api/v1/projects/%d/federation/events?after_id=0", server.URL, project.ID)
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer "+grant.Token)
				started := make(chan struct{})
				type response struct {
					status int
					raw    []byte
					err    error
				}
				done := make(chan response, 1)
				go func() {
					close(started)
					resp, err := http.DefaultClient.Do(request)
					if err != nil {
						done <- response{err: err}
						return
					}
					defer func() { _ = resp.Body.Close() }()
					raw, err := io.ReadAll(resp.Body)
					done <- response{status: resp.StatusCode, raw: raw, err: err}
				}()
				<-started
				_, first := nativeJob(t, store, project)
				result := <-done
				require.NoError(t, result.err)
				if result.status == http.StatusOK {
					var body api.PollEventsBody
					require.NoError(t, json.Unmarshal(result.raw, &body))
					require.Less(t, body.NextAfterID, first.ID)
					for _, event := range body.Events {
						require.Empty(t, db.EventRequiredFeatures(event.Type), "old poll leaked a cron event")
					}
				} else {
					require.Equal(t, http.StatusConflict, result.status, string(result.raw))
					require.Contains(t, string(result.raw), "unsupported_event_features")
				}
				resp, raw := nativeRequest(t, path, grant.Token, "", http.MethodGet, nil)
				require.Equal(t, http.StatusConflict, resp.StatusCode, string(raw))
			}
		})
	}
}

func TestCronReturnedPageRequiresFeaturesEvenWithoutLatch(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			store := nativeStore(t, backend)
			project, err := store.CreateProject(t.Context(), "hub-project")
			require.NoError(t, err)
			_, err = store.EnableProjectFederation(t.Context(), project.ID, "worker")
			require.NoError(t, err)
			nativeJob(t, store, project)
			_, err = store.ReadFederation(t.Context(), db.FederationReadParams{ProjectID: project.ID, Limit: 100})
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
		})
	}
}

func TestCronClientRejectsUnknownRequirementsBeforeReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "cron_v1", r.Header.Get("X-Kata-Event-Features"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kata-Required-Event-Features", "future_v2")
		_, _ = w.Write([]byte(`{"reset_required":true,"reset_after_id":90,"next_after_id":90,"events":[]}`))
	}))
	defer server.Close()
	client, err := federation.NewClient(t.Context(), server.URL, "token", clientpkg.Opts{})
	require.NoError(t, err)
	body, err := client.PollProjectEvents(t.Context(), 1, 0, 100)
	require.Error(t, err)
	require.False(t, body.ResetRequired)
	require.Zero(t, body.NextAfterID)
}

func TestCronClientDoesNotAssumeFutureEventsAreLegacy(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"push_cursor_event_id":1}`))
	}))
	t.Cleanup(server.Close)
	client, err := federation.NewClient(t.Context(), server.URL, "", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.IngestProjectEvents(t.Context(), 1, []api.FederationIngestEventEnvelope{{Type: "project.future_feature"}})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	require.Zero(t, requests, "future events require an explicit wire mapping before publication")
}

func TestCronClientRefusesPublicationToOldHub(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts++
			_, _ = w.Write([]byte(`{"accepted":1,"push_cursor_event_id":10}`))
			return
		}
		_, _ = w.Write([]byte(`{"project_id":1,"project_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAA","replay_horizon_event_id":1,"baseline_through_event_id":1}`))
	}))
	defer server.Close()
	client, err := federation.NewClient(t.Context(), server.URL, "token", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.IngestProjectEvents(t.Context(), 1, []api.FederationIngestEventEnvelope{{EventID: 10, Type: "cron.job.created"}})
	require.Error(t, err)
	require.Zero(t, posts, "publication requires positive hub support before POST")
}

func TestPushOnlyEnrollmentCanPublishCronEvents(t *testing.T) {
	store := nativeStore(t, "sqlite")
	server := nativeHTTP(t, store)
	project, err := store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(t.Context(), project.ID, "worker")
	require.NoError(t, err)
	originUID, err := uid.New()
	require.NoError(t, err)
	enrollment, err := store.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{
		SpokeInstanceUID: originUID,
		ProjectID:        &project.ID,
		Capabilities:     "push",
		Actor:            "worker",
	})
	require.NoError(t, err)

	event, jobUID := pushCronCreatedEvent(t, project, originUID)
	client, err := federation.NewClient(t.Context(), server.URL, enrollment.Token, clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.IngestProjectEvents(t.Context(), project.ID, []api.FederationIngestEventEnvelope{event})
	require.NoError(t, err, "push-only feature discovery must allow cron publication")
	job, err := store.CronJob(t.Context(), project.ID, jobUID)
	require.NoError(t, err)
	require.Equal(t, "Review", job.Name)
}

func pushCronCreatedEvent(t *testing.T, project db.Project, originUID string) (api.FederationIngestEventEnvelope, string) {
	t.Helper()
	eventUID, err := uid.New()
	require.NoError(t, err)
	jobUID, err := uid.New()
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	definition, err := json.Marshal(cron.JobDefinition{
		Version: 1,
		Kind:    "job",
		Trigger: cron.Trigger{Kind: "manual"},
		Action:  cron.Action{Kind: "execute", Prompt: "Review"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", Title: "Review"},
		Overlap: "forbid",
		Catchup: "skip",
	})
	require.NoError(t, err)
	payload, err := json.Marshal(db.CronDefinitionEvent{
		UID: jobUID, ProjectUID: project.UID, Name: "Review",
		Definition: jsontext.Value(definition), Author: "worker",
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	physicalMS := now.UnixMilli()
	contentHash, err := db.EventContentHash(db.EventHashInput{
		UID: eventUID, OriginInstanceUID: originUID, ProjectUID: project.UID,
		ProjectName: project.Name, Type: "cron.job.created", Actor: "worker",
		HLCPhysicalMS: physicalMS, Payload: payload,
		CreatedAt: now.Format(db.EventTimestampFormat),
	})
	require.NoError(t, err)
	return api.FederationIngestEventEnvelope{
		EventID: 1, EventUID: eventUID, OriginInstanceUID: originUID,
		ProjectUID: project.UID, ProjectName: project.Name,
		Type: "cron.job.created", Actor: "worker", HLCPhysicalMS: physicalMS,
		ContentHash: contentHash, Payload: jsontext.Value(payload), CreatedAt: now,
	}, jobUID
}

func TestCronFederationBackendPairs(t *testing.T) {
	for _, hubBackend := range []string{"sqlite", "postgres"} {
		for _, spokeBackend := range []string{"sqlite", "postgres"} {
			t.Run(hubBackend+"_to_"+spokeBackend, func(t *testing.T) {
				ctx := t.Context()
				hub := nativeStore(t, hubBackend)
				spoke := nativeStore(t, spokeBackend)
				server := nativeHTTP(t, hub)
				project, err := hub.CreateProject(ctx, "hub-project")
				require.NoError(t, err)
				workflow, _, err := hub.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: project.ID, Name: "Review workflow", Actor: "worker", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status"}}}})
				require.NoError(t, err)
				job, _ := nativeJob(t, hub, project)
				definition := job.Definition
				definition.Action = cron.Action{Kind: "execute", WorkflowUID: workflow.UID}
				job, _, err = hub.PutCronJob(ctx, db.PutCronJob{ProjectID: project.ID, UID: job.UID, ExpectedEventUID: job.DefinitionEventUID, Actor: "worker", Name: job.Name, Definition: definition})
				require.NoError(t, err)

				occurrence := "manual:review"
				expectedRuns := []db.CronRun{}
				for range 2 {
					runUID, err := uid.New()
					require.NoError(t, err)
					result, err := hub.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: project.ID, UID: runUID, JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, WorkflowUID: &workflow.UID, WorkflowDefinitionEventUID: &workflow.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: "worker", Status: "succeeded", Summary: cron.Summary{Version: 1, Message: "Reviewed", InputTokens: 123}})
					require.NoError(t, err)
					expectedRuns = append(expectedRuns, result.Run)
				}
				binding, err := hub.EnableProjectFederation(ctx, project.ID, "worker")
				require.NoError(t, err)
				grant, err := hub.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.InstanceUID(), ProjectID: &project.ID, Capabilities: "pull,push", Actor: "worker"})
				require.NoError(t, err)
				local, err := spoke.CreateProject(ctx, "spoke-project")
				require.NoError(t, err)
				adopted, err := spoke.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: local.ID, HubURL: server.URL, HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: binding.ReplayHorizonEventID, Actor: "worker", EmptyOnly: true})
				require.NoError(t, err)
				current, err := spoke.EnableFederationPush(ctx, local.ID, adopted.Binding.PushCursorEventID)
				require.NoError(t, err)
				credentials := config.FederationCredential{HubURL: server.URL, HubProjectID: project.ID, Token: grant.Token}
				require.NoError(t, federation.SyncFederationOnce(ctx, spoke, current, credentials))
				mirrored, err := spoke.CronJob(ctx, local.ID, job.UID)
				require.NoError(t, err)
				require.Equal(t, job.Definition, mirrored.Definition)
				require.Equal(t, job.DefinitionHLC, mirrored.DefinitionHLC)
				require.Equal(t, job.DefinitionEventUID, mirrored.DefinitionEventUID)
				var runs []db.CronRunExport
				for r, e := range spoke.ExportCronRuns(ctx, db.ExportFilter{ProjectID: &local.ID}) {
					require.NoError(t, e)
					runs = append(runs, r)
				}

				require.Len(t, runs, 2)
				byUID := make(map[string]db.CronRun, len(runs))
				for _, run := range runs {
					byUID[run.UID] = db.CronRun(run)
				}
				for _, want := range expectedRuns {
					got, exists := byUID[want.UID]
					require.True(t, exists, "independent run UID missing from peer")
					want.ProjectID, want.ID = local.ID, got.ID
					require.Equal(t, want, got)
				}
				mirrored, _, err = spoke.PutCronJob(ctx, db.PutCronJob{ProjectID: local.ID, UID: job.UID, ExpectedEventUID: mirrored.DefinitionEventUID, Actor: "worker", Name: "Edited on spoke", Definition: mirrored.Definition})
				require.NoError(t, err)
				current, err = spoke.FederationBindingByProject(ctx, local.ID)
				require.NoError(t, err)
				require.NoError(t, federation.SyncFederationOnce(ctx, spoke, current, credentials))
				onHub, err := hub.CronJob(ctx, project.ID, job.UID)
				require.NoError(t, err)
				require.Equal(t, mirrored.DefinitionEventUID, onHub.DefinitionEventUID)
				require.Equal(t, "Edited on spoke", onHub.Name)
				runUID, err := uid.New()
				require.NoError(t, err)
				logged, err := spoke.ObserveCronRun(ctx, db.ObserveCronRun{ProjectID: local.ID, UID: runUID, JobUID: &mirrored.UID, DefinitionEventUID: &mirrored.DefinitionEventUID, OccurrenceKey: &occurrence, Actor: "worker", Status: "running", Summary: cron.Summary{Version: 1}})
				require.NoError(t, err)
				current, err = spoke.FederationBindingByProject(ctx, local.ID)
				require.NoError(t, err)
				require.NoError(t, federation.SyncFederationOnce(ctx, spoke, current, credentials))
				published, err := hub.CronRun(ctx, project.ID, runUID)
				require.NoError(t, err)
				logged.Run.ProjectID = project.ID
				logged.Run.ID = published.ID
				require.Equal(t, logged.Run, published)

			})
		}
	}
}
