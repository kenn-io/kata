package daemon_test

import (
	"bytes"
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
	type result struct {
		Pause   *db.CronProjectPause `json:"project_pause"`
		Changed bool                 `json:"changed"`
	}
	var first *db.CronProjectPause
	for _, step := range []struct {
		method  string
		changed bool
		paused  bool
	}{
		{http.MethodPut, true, true}, {http.MethodPut, false, true}, {http.MethodDelete, true, false}, {http.MethodDelete, false, false},
	} {
		before, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw := cronHTTP(t, env, step.method, path+"/pause", map[string]any{"actor": "operator", "reason": "Maintenance"})
		require.Equalf(t, 200, status, "%s", raw)
		var out result
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Equal(t, step.changed, out.Changed)
		require.Equal(t, step.paused, out.Pause != nil)
		if out.Pause != nil {
			require.Equal(t, "operator", out.Pause.PausedBy)
			require.Equal(t, "Maintenance", out.Pause.Reason)
			if first == nil {
				first = out.Pause
			} else {
				require.Equal(t, first, out.Pause)
			}
		}
		after, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		if step.changed {
			events, err := env.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, AfterID: before, Limit: 10})
			require.NoError(t, err)
			require.Len(t, events, 1)
			msg := receiveMsg(t, sub.Ch, time.Second, "project pause event")
			require.Equal(t, events[0], *msg.Event)
			captured := sink.snapshot()
			require.Equal(t, events[0], captured[len(captured)-1])
			require.Equal(t, "operator", events[0].Actor)
		} else {
			require.Equal(t, before, after)
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
			require.Equal(t, []string{"job_pause", "project_pause", "resume_discard_missed"}, read.Controls)
			require.Contains(t, string(raw), `"project_pause":`)
		}
	}
	status, raw := cronHTTP(t, env, http.MethodPut, path+"/pause", map[string]any{"actor": "operator", "reason": strings.Repeat("x", 257)})
	require.Equalf(t, 400, status, "%s", raw)
}

func TestCronProjectPauseBodylessAndMalformedRecovery(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/pause", project.ID)
	// Legacy state can exist from an older daemon. Generic production writes are reserved.
	_, err := env.DB.ExecContext(t.Context(), `UPDATE projects SET metadata='{"cron.pause":false}' WHERE id=?`, project.ID)
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodDelete, path, map[string]any{"actor": "operator"})
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), `"project_pause":null`)
	// A principal supplying the actor needs no body.
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, path, nil)
	req = req.WithContext(daemon.WithPrincipal(req.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "operator", Subject: "operator"}))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, rec.Body.String())
}

func TestCronProjectPauseScopeAndEnrollment(t *testing.T) {
	env, project, _, _ := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	other := seedProject(t, env, "other-project")
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: closeEvidenceHostAccess{deniedProjectID: other.ID}})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	for _, id := range []int64{project.ID, other.ID} {
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			req := httptest.NewRequestWithContext(t.Context(), method, fmt.Sprintf("/api/v1/projects/%d/cron/pause", id), bytes.NewBufferString(`{}`))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(daemon.WithPrincipal(req.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Actor: "operator", Subject: "operator"}))
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			want := 200
			if id == other.ID {
				want = 404
			}
			require.Equal(t, want, rec.Code, rec.Body.String())
		}
	}
	enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		response, raw := envDoRaw(t, env, method, fmt.Sprintf("/api/v1/projects/%d/cron/pause", project.ID), map[string]any{"actor": "operator"}, map[string]string{"Authorization": "Bearer " + enrollment.Token})
		require.Equalf(t, 403, response.StatusCode, "%s", raw)
	}
}

func TestCronProjectPauseSpokeHint(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "spoke-project")
	_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: cronHTTPUID(t), Actor: "operator", Enabled: false})
	require.NoError(t, err)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		status, raw := cronHTTP(t, env, method, fmt.Sprintf("/api/v1/projects/%d/cron/pause", project.ID), map[string]any{"actor": "operator"})
		require.Equalf(t, 409, status, "%s", raw)
		require.Contains(t, string(raw), "owning hub")
	}
}

// Contract: discard is a separate supported action requiring retained CAS;
// committed policy events reach subscribers before the enabled definition.
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
	require.Equal(t, *first.Event, hooks[len(hooks)-2])
	require.Equal(t, *second.Event, hooks[len(hooks)-1])
	status, raw = cronHTTP(t, env, http.MethodGet, path+"/capabilities", nil)
	require.Equal(t, 200, status)
	var caps struct {
		Controls []string `json:"controls"`
		Revision int64    `json:"project_revision"`
	}
	require.NoError(t, json.Unmarshal(raw, &caps))
	require.Contains(t, caps.Controls, "resume_discard_missed")
	require.Positive(t, caps.Revision)
	for _, guard := range []bool{false, true} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, env.URL+path+"/pause?discard_missed=true", strings.NewReader(`{"actor":"operator"}`))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer tok")
		req.Header.Set("Content-Type", "application/json")
		if guard {
			req.Header.Set("If-Match", fmt.Sprintf(`"rev-%d"`, caps.Revision))
		}
		response, err := env.HTTP.Do(req)
		require.NoError(t, err)
		body := readClose(t, response)
		want := 400
		if guard {
			want = 200
		}
		require.Equalf(t, want, response.StatusCode, "%s", body)
	}
}

func TestCronDiscardEnrollmentBoundary(t *testing.T) {
	env, p, _, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	definition.Enabled = false
	native, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Recurring review", Actor: "tester", Definition: definition})
	require.NoError(t, err)
	adapter, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Adapter review", Actor: "tester", Definition: cronHTTPDefinition(t)})
	require.NoError(t, err)
	enrollment := createClaimEnrollment(t, env, p.ID, cronHTTPUID(t), "push")
	for _, job := range []db.CronJob{native, adapter} {
		raw := map[string]any{"discard_missed": true, "expected_event_uid": job.DefinitionEventUID}
		response, body := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/resume", p.ID, job.UID), raw, map[string]string{"Authorization": "Bearer " + enrollment.Token})
		want := 200
		if job.UID == adapter.UID {
			want = 403
		}
		require.Equalf(t, want, response.StatusCode, "%s", body)
	}
}

// Contract: cutoff state lives beside the strict inner job, preserving clients
// that reject unknown members in the portable job record.
func TestCronPassoverReadEnvelopes(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "control-read-project")
	definition := cronHTTPDefinition(t)
	definition.Enabled = false
	job, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: p.ID, Name: "Inspect", Actor: "operator", Definition: definition})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs", p.ID)
	status, raw := cronHTTP(t, env, http.MethodPost, path+"/"+job.UID+"/resume", map[string]any{"actor": "operator", "discard_missed": true, "expected_event_uid": job.DefinitionEventUID})
	require.Equalf(t, 200, status, "%s", raw)
	var expected *db.CronPassoverState
	for _, route := range []string{"action", "show", "list"} {
		if route == "show" {
			status, raw = cronHTTP(t, env, http.MethodGet, path+"/"+job.UID, nil)
		}
		if route == "list" {
			status, raw = cronHTTP(t, env, http.MethodGet, path, nil)
		}
		require.Equal(t, 200, status)
		var wire struct {
			Job       jsontext.Value                   `json:"job"`
			Jobs      []jsontext.Value                 `json:"jobs"`
			Passover  *db.CronPassoverState            `json:"passover"`
			Passovers map[string]*db.CronPassoverState `json:"passovers"`
		}
		require.NoError(t, json.Unmarshal(raw, &wire))
		state := wire.Passover
		inner := wire.Job
		if route == "list" {
			state = wire.Passovers[job.UID]
			require.Len(t, wire.Jobs, 1)
			inner = wire.Jobs[0]
		}
		require.NotNil(t, state, route)
		require.False(t, state.Invalid)
		require.Equal(t, "job", state.Scope)
		if expected == nil {
			expected = state
		} else {
			require.Equal(t, expected, state)
		}
		var strict api.CronJob
		require.NoError(t, json.Unmarshal(inner, &strict, json.RejectUnknownMembers(true)))
		require.True(t, strict.Definition.Enabled)
	}
}
