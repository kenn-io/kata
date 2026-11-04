package daemon_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
			assertNoReceive(t, sub.Ch, 20*time.Millisecond, "no-op pause") //nolint:kennlint // deadlinetest: bounded absence check for the no-op pause broadcast contract
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
			require.Equal(t, []string{"job_pause", "project_pause", "run_request", "cancel_request"}, read.Controls)
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
