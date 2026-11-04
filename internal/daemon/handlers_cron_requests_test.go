package daemon_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func cronRequestHTTPFixture(t *testing.T) (*testenv.Env, db.Project, db.CronJob) {
	t.Helper()
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "request-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Actor: "operator", Name: "Execute review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	return env, p, job
}

func TestCronRequestsHTTP(t *testing.T) {
	sink := &recordingSink{}
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(c *daemon.ServerConfig) { c.Hooks = sink })
	p := seedProject(t, env, "request-project")
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Actor: "operator", Name: "Execute review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	base := fmt.Sprintf("/api/v1/projects/%d/cron", p.ID)
	path := base + "/jobs/" + job.UID + "/requests"
	status, raw := cronHTTP(t, env, http.MethodGet, base+"/requests", nil)
	require.Equal(t, 200, status, string(raw))
	require.JSONEq(t, `{"requests":[]}`, string(raw))
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: p.ID})
	defer sub.Unsub()
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	id := cronHTTPUID(t)
	body := map[string]any{"uid": id, "actor": "operator", "ttl_seconds": 60}
	status, raw = cronHTTP(t, env, http.MethodPost, path, body)
	require.Equal(t, 200, status, string(raw))
	var first db.RequestCronOut
	require.NoError(t, json.Unmarshal(raw, &first))
	require.False(t, first.Replayed)
	require.NotNil(t, first.Event)
	require.Equal(t, id, first.Request.UID)
	require.Equal(t, job.DefinitionEventUID, first.Request.DefinitionEventUID)
	require.Equal(t, "operator", first.Request.RequestedBy)
	require.Equal(t, time.Minute, first.Request.ExpiresAt.Sub(first.Request.RequestedAt))
	events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: p.ID, AfterID: before, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, events[0], *first.Event)
	msg := receiveMsg(t, sub.Ch, time.Second, "run request event")
	require.Equal(t, *first.Event, *msg.Event)
	captured := sink.snapshot()
	require.Equal(t, *first.Event, captured[len(captured)-1])
	status, raw = cronHTTP(t, env, http.MethodPost, path, body)
	require.Equal(t, 200, status, string(raw))
	var replay db.RequestCronOut
	require.NoError(t, json.Unmarshal(raw, &replay))
	require.True(t, replay.Replayed)
	require.Nil(t, replay.Event)
	require.Equal(t, first.Request, replay.Request)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.Event.ID, after)
	body["ttl_seconds"] = 61
	status, raw = cronHTTP(t, env, http.MethodPost, path, body)
	require.Equal(t, 409, status, string(raw))
	require.Contains(t, string(raw), "request_conflict")
	status, raw = cronHTTP(t, env, http.MethodGet, base+"/requests?state=pending", nil)
	require.Equal(t, 200, status, string(raw))
	var list struct {
		Requests []db.CronRequest `json:"requests"`
	}
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Requests, 1)
	require.Equal(t, "pending", list.Requests[0].State)
	status, raw = cronHTTP(t, env, http.MethodGet, base+"/requests?state=unknown", nil)
	require.Equal(t, 400, status, string(raw))
	status, raw = cronHTTP(t, env, http.MethodGet, base+"/capabilities", nil)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"run_request"`)
	require.Contains(t, string(raw), `"cancel_request"`)
	// Pause refuses new run requests but leaves cancel admission available.
	_, err = env.DB.SetCronProjectPause(t.Context(), db.SetCronProjectPauseIn{ProjectID: p.ID, Actor: "operator", Paused: true})
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"uid": cronHTTPUID(t), "actor": "operator"})
	require.Equal(t, 409, status, string(raw))
	require.Contains(t, string(raw), "request_unavailable")
	run, err := env.DB.ObserveCronRun(t.Context(), db.ObserveCronRun{ProjectID: p.ID, UID: cronHTTPUID(t), Actor: "operator", JobUID: &job.UID, DefinitionEventUID: &job.DefinitionEventUID, Status: "running", Summary: cron.Summary{Version: 1}})
	require.NoError(t, err)
	status, raw = cronHTTP(t, env, http.MethodPost, base+"/runs/"+run.Run.UID+"/cancel-request", map[string]any{"uid": cronHTTPUID(t), "actor": "operator"})
	require.Equal(t, 200, status, string(raw))
	var cancel db.RequestCronOut
	require.NoError(t, json.Unmarshal(raw, &cancel))
	require.Equal(t, "cancel", cancel.Request.Kind)
	require.Equal(t, run.Run.UID, *cancel.Request.RunUID)
	require.Equal(t, 10*time.Minute, cancel.Request.ExpiresAt.Sub(cancel.Request.RequestedAt))
}

func TestCronRequestsStrictBodyAndClock(t *testing.T) {
	env, p, job := cronRequestHTTPFixture(t)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/requests", p.ID, job.UID)
	for _, body := range []any{nil, map[string]any{"actor": "operator"}, map[string]any{"uid": "bad", "actor": "operator"}, map[string]any{"uid": cronHTTPUID(t), "actor": "operator", "ttl_seconds": 0}, map[string]any{"uid": cronHTTPUID(t), "actor": "operator", "ttl_seconds": 59}, map[string]any{"uid": cronHTTPUID(t), "actor": "operator", "ttl_seconds": 86401}, map[string]any{"uid": cronHTTPUID(t), "actor": "operator", "ttl_seconds": 60.5}, map[string]any{"uid": cronHTTPUID(t), "actor": "operator", "extra": true}} {
		status, raw := cronHTTP(t, env, http.MethodPost, path, body)
		require.Equal(t, 400, status, string(raw))
	}
	for _, clock := range []struct {
		at     time.Time
		status int
	}{{time.Now().Add(-time.Minute), 410}, {time.Now().Add(time.Hour), 400}} {
		id, e := uid.FromTime(clock.at)
		require.NoError(t, e)
		status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"uid": id, "actor": "operator"})
		require.Equal(t, clock.status, status, string(raw))
		require.Contains(t, string(raw), "clock")
	}
	other := seedProject(t, env, "other-request-project")
	status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/requests", other.ID, job.UID), map[string]any{"uid": cronHTTPUID(t), "actor": "operator"})
	require.Equal(t, 404, status, string(raw))
}

func TestCronRequestsRootAndScope(t *testing.T) {
	env, p, job := cronRequestHTTPFixture(t)
	other := seedProject(t, env, "other-scope-project")
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: closeEvidenceHostAccess{deniedProjectID: other.ID}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, pid := range []int64{p.ID, other.ID} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			if pid == other.ID && method == http.MethodPost {
				continue // A cross-project job does not exercise project authorization.
			}
			path := fmt.Sprintf("/api/v1/projects/%d/cron/requests", pid)
			if method == http.MethodPost {
				path = fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/requests", pid, job.UID)
			}
			body := fmt.Sprintf(`{"uid":%q}`, cronHTTPUID(t))
			if method == http.MethodGet {
				body = ""
			}
			req := httptest.NewRequestWithContext(t.Context(), method, path, bytes.NewBufferString(body))
			if method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			req = req.WithContext(daemon.WithPrincipal(req.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "operator", Subject: "operator"}))
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			want := 200
			if pid == other.ID {
				want = 404
			}
			require.Equal(t, want, rec.Code, rec.Body.String())
		}
	}
	_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: p.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: p.UID, Actor: "operator", Enabled: false})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/requests", p.ID, job.UID), map[string]any{"uid": cronHTTPUID(t), "actor": "operator"})
	require.Equal(t, 409, status, string(raw))
	require.Contains(t, string(raw), "owning hub")
}

func TestCronRequestsRejectEnrollmentSelfAuth(t *testing.T) {
	env, p, _, _ := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	enrollment := createClaimEnrollment(t, env, p.ID, cronHTTPUID(t), "push")
	for _, path := range []string{fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/requests", p.ID, cronHTTPUID(t)), fmt.Sprintf("/api/v1/projects/%d/cron/runs/%s/cancel-request", p.ID, cronHTTPUID(t))} {
		response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"uid": cronHTTPUID(t), "actor": "operator"}, map[string]string{"Authorization": "Bearer " + enrollment.Token})
		require.Equal(t, 403, response.StatusCode, string(raw))
	}
}
