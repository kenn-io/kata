package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Contract: project policy is attributed, idempotent, visible to readers, and
// publishes exactly the committed event to streams and hooks.
func TestCronProjectPauseHTTP(t *testing.T) {
	sink := &recordingSink{}
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(c *daemon.ServerConfig) { c.Hooks = sink })
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron", project.ID)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	for _, step := range []struct {
		method          string
		changed, paused bool
	}{
		{http.MethodPut, true, true}, {http.MethodPut, false, true}, {http.MethodDelete, true, false}, {http.MethodDelete, false, false},
	} {
		before, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw := cronHTTP(t, env, step.method, path+"/pause", map[string]any{"actor": "operator", "reason": "Maintenance"})
		require.Equalf(t, 200, status, "%s", raw)
		var out struct {
			Pause   *db.CronProjectPause `json:"project_pause"`
			Changed bool                 `json:"changed"`
		}
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Equal(t, step.changed, out.Changed)
		require.Equal(t, step.paused, out.Pause != nil)
		if out.Pause != nil {
			require.Equal(t, "operator", out.Pause.PausedBy)
			require.Equal(t, "Maintenance", out.Pause.Reason)
		}
		events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, AfterID: before, Limit: 10})
		require.NoError(t, err)
		if step.changed {
			require.Len(t, events, 1)
			require.Equal(t, "operator", events[0].Actor)
			require.Equal(t, events[0], *receiveMsg(t, sub.Ch, time.Second, "project pause event").Event)
			captured := sink.snapshot()
			require.Equal(t, events[0], captured[len(captured)-1])
		} else {
			require.Empty(t, events)
			// Publication completes synchronously before the HTTP response.
			select {
			case <-sub.Ch:
				t.Fatal("no-op pause published an event")
			default:
			}
		}
		for _, route := range []string{"/capabilities", "/jobs"} {
			status, raw = cronHTTP(t, env, http.MethodGet, path+route, nil)
			require.Equalf(t, 200, status, "%s", raw)
			var read struct {
				Pause    *db.CronProjectPause `json:"project_pause"`
				Controls []string             `json:"controls"`
			}
			require.NoError(t, json.Unmarshal(raw, &read))
			require.Equal(t, out.Pause, read.Pause)
			require.Equal(t, []string{"job_pause", "project_pause", "run_request", "cancel_request", "resume_discard_missed"}, read.Controls)
			require.Contains(t, string(raw), `"project_pause":`)
		}
	}
}

// Contract: project controls and runtime requests honor host project scope
// (pause and resume need no body), refuse enrollment credentials, and refuse
// spoke writes with an owning-hub hint.
func TestCronControlRouteBoundaries(t *testing.T) {
	env, project, _, _ := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Actor: "operator", Name: "Execute review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	other := seedProject(t, env, "other-project")
	// The denied project's own eligible job, so only authorization can refuse it.
	otherJob, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: other.ID, Actor: "operator", Name: "Execute review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	jobs := map[int64]string{project.ID: job.UID, other.ID: otherJob.UID}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: closeEvidenceHostAccess{deniedProjectID: other.ID}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, pid := range []int64{project.ID, other.ID} {
		base := fmt.Sprintf("/api/v1/projects/%d/cron", pid)
		for _, route := range []struct{ method, path, body string }{
			{http.MethodPut, base + "/pause", ""}, {http.MethodDelete, base + "/pause", ""},
			{http.MethodPost, base + "/jobs/" + jobs[pid] + "/requests", fmt.Sprintf(`{"uid":%q}`, cronHTTPUID(t))},
			{http.MethodGet, base + "/requests", ""},
		} {
			req := httptest.NewRequestWithContext(t.Context(), route.method, route.path, strings.NewReader(route.body))
			if route.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req = req.WithContext(daemon.WithPrincipal(req.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "operator", Subject: "operator"}))
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			want := 200
			if pid == other.ID {
				want = 404
			}
			require.Equalf(t, want, rec.Code, "%s %s: %s", route.method, route.path, rec.Body.String())
		}
	}

	base := fmt.Sprintf("/api/v1/projects/%d/cron", project.ID)
	enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
	request := map[string]any{"uid": cronHTTPUID(t), "actor": "operator"}
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, base + "/pause", map[string]any{"actor": "operator"}}, {http.MethodDelete, base + "/pause", map[string]any{"actor": "operator"}},
		{http.MethodPost, base + "/jobs/" + job.UID + "/requests", request}, {http.MethodPost, base + "/runs/" + cronHTTPUID(t) + "/cancel-request", request},
	} {
		response, raw := envDoRaw(t, env, route.method, route.path, route.body, map[string]string{"Authorization": "Bearer " + enrollment.Token})
		require.Equalf(t, 403, response.StatusCode, "%s %s: %s", route.method, route.path, raw)
	}

	spoke := seedProject(t, env, "spoke-project")
	spokeJob, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: spoke.ID, Actor: "operator", Name: "Execute review", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: spoke.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: cronHTTPUID(t), Actor: "operator", Enabled: false})
	require.NoError(t, err)
	base = fmt.Sprintf("/api/v1/projects/%d/cron", spoke.ID)
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPut, base + "/pause", map[string]any{"actor": "operator"}}, {http.MethodDelete, base + "/pause", map[string]any{"actor": "operator"}},
		{http.MethodPost, base + "/jobs/" + spokeJob.UID + "/requests", map[string]any{"uid": cronHTTPUID(t), "actor": "operator"}},
	} {
		status, raw := cronHTTP(t, env, route.method, route.path, route.body)
		require.Equalf(t, 409, status, "%s", raw)
		require.Contains(t, string(raw), "owning hub")
	}
}

// Contract: discard is a separate supported action requiring retained CAS;
// committed policy events reach subscribers before the enabled definition. The
// cutoff sits beside the strict job record in action, show, and list envelopes.
func TestCronDiscardResumeHTTP(t *testing.T) {
	sink := &recordingSink{}
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(c *daemon.ServerConfig) { c.Hooks = sink })
	p := seedProject(t, env, "discard-project")
	definition := cronHTTPDefinition(t)
	definition.Enabled = false
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron", p.ID)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{})
	defer sub.Unsub()
	status, raw := cronHTTP(t, env, http.MethodPost, path+"/jobs/"+job.UID+"/resume", map[string]any{"actor": "operator", "discard_missed": true})
	require.Equalf(t, 400, status, "%s", raw)
	status, raw = cronHTTP(t, env, http.MethodPost, path+"/jobs/"+job.UID+"/resume", map[string]any{"actor": "operator", "discard_missed": true, "expected_event_uid": job.DefinitionEventUID})
	require.Equalf(t, 200, status, "%s", raw)
	first := receiveMsg(t, sub.Ch, time.Second, "passover")
	second := receiveMsg(t, sub.Ch, time.Second, "enable")
	require.Equal(t, "project.metadata_updated", first.Event.Type)
	require.Equal(t, "cron.job.updated", second.Event.Type)
	hooks := sink.snapshot()
	require.Equal(t, []db.Event{*first.Event, *second.Event}, hooks[len(hooks)-2:])

	var cutoff *db.CronPassoverState
	for _, route := range []string{"action", "show", "list"} {
		switch route {
		case "show":
			status, raw = cronHTTP(t, env, http.MethodGet, path+"/jobs/"+job.UID, nil)
		case "list":
			status, raw = cronHTTP(t, env, http.MethodGet, path+"/jobs", nil)
		}
		require.Equal(t, 200, status)
		var wire struct {
			Job       jsontext.Value                   `json:"job"`
			Jobs      []jsontext.Value                 `json:"jobs"`
			Passover  *db.CronPassoverState            `json:"passover"`
			Passovers map[string]*db.CronPassoverState `json:"passovers"`
		}
		require.NoError(t, json.Unmarshal(raw, &wire))
		state, inner := wire.Passover, wire.Job
		if route == "list" {
			require.Len(t, wire.Jobs, 1)
			state, inner = wire.Passovers[job.UID], wire.Jobs[0]
		}
		require.NotNil(t, state, route)
		require.False(t, state.Invalid)
		require.Equal(t, "job", state.Scope)
		if cutoff == nil {
			cutoff = state
		}
		require.Equal(t, cutoff, state)
		var strict api.CronJob
		require.NoError(t, json.Unmarshal(inner, &strict, json.RejectUnknownMembers(true)))
		require.True(t, strict.Definition.Enabled)
	}

	status, raw = cronHTTP(t, env, http.MethodGet, path+"/capabilities", nil)
	require.Equal(t, 200, status)
	var caps struct {
		Revision int64 `json:"project_revision"`
	}
	require.NoError(t, json.Unmarshal(raw, &caps))
	require.Positive(t, caps.Revision)
	for _, guard := range []string{"", fmt.Sprintf(`"rev-%d"`, caps.Revision)} {
		headers := map[string]string{"Authorization": "Bearer tok"}
		if guard != "" {
			headers["If-Match"] = guard
		}
		response, body := envDoRaw(t, env, http.MethodDelete, path+"/pause?discard_missed=true", map[string]any{"actor": "operator"}, headers)
		want := 400
		if guard != "" {
			want = 200
		}
		require.Equalf(t, want, response.StatusCode, "%s", body)
	}
}
