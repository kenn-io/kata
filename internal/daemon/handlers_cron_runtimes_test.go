package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func runtimeHTTPBody() map[string]any {
	return map[string]any{"actor": "operator", "label": "example-worker", "adapter": "example", "features": []string{"cron_v1"}}
}
func TestCronRuntimeHTTP(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	registry := daemon.NewCronRuntimeRegistry(now, func() time.Time { return now })
	env := testenv.New(t, func(c *daemon.ServerConfig) { c.CronRuntimes = registry })
	p := seedProject(t, env, "example-project")
	other := seedProject(t, env, "other-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", p.ID)
	id := cronHTTPUID(t)
	status, raw := runtimeHTTPRequest(t, env, http.MethodGet, path, nil)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), `"state":"unknown"`)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	status, raw = runtimeHTTPRequest(t, env, http.MethodPut, path+"/"+id, runtimeHTTPBody())
	require.Equalf(t, 200, status, "%s", raw)
	var response api.BeatCronRuntimeResponse
	require.NoError(t, json.Unmarshal(raw, &response.Body))
	require.Equal(t, "live", response.Body.Runtime.Status)
	require.Equal(t, "not_applicable", response.Body.Forwarded)
	status, raw = runtimeHTTPRequest(t, env, http.MethodGet, path, nil)
	require.Equal(t, 200, status)
	require.Contains(t, string(raw), id)
	status, raw = runtimeHTTPRequest(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", other.ID), nil)
	require.Equal(t, 200, status)
	require.NotContains(t, string(raw), id)
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after, "heartbeats never produce events")
}
func TestCronRuntimeHTTPValidationAndCapacity(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	registry := daemon.NewCronRuntimeRegistry(now, func() time.Time { return now })
	env := testenv.New(t, func(c *daemon.ServerConfig) { c.CronRuntimes = registry })
	p := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/", p.ID)
	for _, tc := range []struct {
		name  string
		patch map[string]any
		field string
	}{
		{"blank label", map[string]any{"label": " "}, "label"},
		{"null state", map[string]any{"state": nil}, "state"},
		{"empty state", map[string]any{"state": ""}, "state"},
		{"null interval", map[string]any{"interval_seconds": nil}, "interval_seconds"},
		{"zero interval", map[string]any{"interval_seconds": 0}, "interval_seconds"},
		{"unknown field", map[string]any{"unknown": true}, "unknown"},
		{"null features", map[string]any{"features": nil}, "features"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := runtimeHTTPBody()
			maps.Copy(body, tc.patch)
			status, raw := runtimeHTTPRequest(t, env, http.MethodPut, path+cronHTTPUID(t), body)
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			var response api.ErrorEnvelope
			require.NoError(t, json.Unmarshal(raw, &response))
			require.Equal(t, "validation", response.Error.Code)
			require.Contains(t, response.Error.Message, tc.field)
		})
	}
	canonicalBody := runtimeHTTPBody()
	canonicalBody["interval_seconds"] = 60
	canonicalBody["state"] = "active"
	for _, id := range []string{strings.ToLower(cronHTTPUID(t)), "invalid"} {
		status, raw := runtimeHTTPRequest(t, env, http.MethodPut, path+id, canonicalBody)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		var response api.ErrorEnvelope
		require.NoError(t, json.Unmarshal(raw, &response))
		require.Equal(t, "validation", response.Error.Code)
		require.Contains(t, response.Error.Message, "runtime identity")
	}
	for range 256 {
		_, err := registry.Beat(p.ID, cronHTTPUID(t), "operator", cron.RuntimeBeat{Label: "worker", Adapter: "example"})
		require.NoError(t, err)
	}
	status, raw := runtimeHTTPRequest(t, env, http.MethodPut, path+cronHTTPUID(t), runtimeHTTPBody())
	require.Equal(t, http.StatusTooManyRequests, status, string(raw))
	var response api.ErrorEnvelope
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, "cron_runtime_limit", response.Error.Code)
}

func TestCronRuntimeHTTPBodyLimit(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	p := seedProject(t, env, "example-project")
	url := fmt.Sprintf("%s/api/v1/projects/%d/cron/runtimes/%s", env.URL, p.ID, cronHTTPUID(t))
	raw, err := json.Marshal(runtimeHTTPBody())
	require.NoError(t, err)
	body := string(raw) + strings.Repeat(" ", cron.RuntimeBeatLimit-len(raw))
	response := doReqEnv(t, env, http.MethodPut, url, body, "")
	responseBody := readClose(t, response)
	require.Equal(t, http.StatusOK, response.StatusCode, string(responseBody))

	response = doReqEnv(t, env, http.MethodPut, url, body+" ", "")
	responseBody = readClose(t, response)
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode, string(responseBody))
}

func TestCronRuntimeHTTPAuthority(t *testing.T) {
	for _, kind := range []string{"readonly", "identity"} {
		t.Run(kind, func(t *testing.T) {
			opts := []testenv.Option{testenv.WithAuthToken("tok"), testenv.WithRequireTokenIdentity()}
			token := "tok"
			want := 200
			if kind == "readonly" {
				opts = []testenv.Option{testenv.WithInsecureReadonly()}
				token = ""
				want = 401
			}
			env := testenv.New(t, opts...)
			p := seedProject(t, env, "example-project")
			path := fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", p.ID)
			if kind == "identity" {
				_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "identity-token", Actor: "bound-actor", AdminActor: db.BootstrapActor})
				require.NoError(t, err)
				token = "identity-token"
				want = 200
			}
			headers := map[string]string{}
			if token != "" {
				headers["Authorization"] = "Bearer " + token
			}
			if kind == "readonly" {
				resp, raw := envDoRaw(t, env, http.MethodGet, path, nil, headers)
				require.Equalf(t, 200, resp.StatusCode, "%s", raw)
			}
			resp, raw := envDoRaw(t, env, http.MethodPut, path+"/"+cronHTTPUID(t), runtimeHTTPBody(), headers)
			require.Equalf(t, want, resp.StatusCode, "%s", raw)
			if kind == "identity" {
				require.Contains(t, string(raw), `"actor":"bound-actor"`)
			}
		})
	}
	t.Run("host_policy", func(t *testing.T) {
		env := testenv.New(t)
		project := seedProject(t, env, "example-project")
		host := &capabilitiesReadHost{projectID: project.ID}
		server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host})
		t.Cleanup(func() { require.NoError(t, server.Close()) })
		path := fmt.Sprintf("/api/v1/projects/%d/cron/runtimes", project.ID)
		for _, tc := range []struct {
			method, suffix string
			status         int
		}{
			{http.MethodGet, "", http.StatusOK},
			{http.MethodPut, "/" + cronHTTPUID(t), http.StatusNotFound},
		} {
			body, err := json.Marshal(runtimeHTTPBody())
			require.NoError(t, err)
			request := httptest.NewRequestWithContext(t.Context(), tc.method, path+tc.suffix, strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, request)
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
		}
		require.Len(t, host.calls, 2)
		require.Equal(t, "listCronRuntimes", host.calls[0].ID)
		require.False(t, host.calls[0].Policy.Mutation)
		require.Equal(t, "beatCronRuntime", host.calls[1].ID)
		require.True(t, host.calls[1].Policy.Mutation)
	})
}

type revokingRuntimeStore struct {
	db.Storage
	tokenID int64
	begin   func(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func (s revokingRuntimeStore) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	_, _, err := s.RevokeAPIToken(ctx, s.tokenID, db.BootstrapActor)
	if err != nil {
		return nil, err
	}
	return s.begin(ctx, opts)
}
func TestCronRuntimeLateRevocation(t *testing.T) {
	env := testenv.New(t)
	p := seedProject(t, env, "example-project")
	token, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "runtime-token", Actor: "operator", AdminActor: db.BootstrapActor})
	require.NoError(t, err)
	registry := daemon.NewCronRuntimeRegistry(time.Now(), nil)
	store := revokingRuntimeStore{Storage: env.DB, tokenID: token.ID, begin: env.DB.BeginTx}
	srv := daemon.NewServer(daemon.ServerConfig{DB: store, CronRuntimes: registry, Auth: config.AuthConfig{Token: "tok", RequireTokenIdentity: true}})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/%s", p.ID, cronHTTPUID(t)), strings.NewReader(`{"label":"worker","adapter":"example"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer runtime-token")
	t.Cleanup(func() { require.NoError(t, srv.Close()) })
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, req)
	require.Equal(t, 401, recorder.Code, recorder.Body.String())
	require.Empty(t, registry.List(p.ID, nil, "dev").Runtimes)
}

// Contract: optional choices replace the old advertisement and reject ambiguous nulls.
func TestCronRuntimeChoicesHTTP(t *testing.T) {
	env := testenv.New(t)
	p := seedProject(t, env, "example-project")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/runtimes/%s", p.ID, cronHTTPUID(t))
	body := runtimeHTTPBody()
	body["choices"] = map[string][]string{"options.agent": {"review", "default"}, "checkout_key": {}}
	status, raw := runtimeHTTPRequest(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	require.Contains(t, string(raw), `"options.agent":["default","review"]`)
	distinct := make([]string, 33)
	for i := range distinct {
		distinct[i] = fmt.Sprintf("agent-%02d", i)
	}
	body["choices"] = map[string][]string{"options.agent": distinct[:32]}
	status, raw = runtimeHTTPRequest(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	for _, bad := range []any{nil, map[string]any{"options.agent": nil}, map[string][]string{"unknown": {}}, map[string][]string{"options.agent": {"a", "a"}}, map[string][]string{"options.agent": make([]string, 33)}, map[string][]string{"options.agent": distinct}} {
		body["choices"] = bad
		status, raw = runtimeHTTPRequest(t, env, http.MethodPut, path, body)
		require.Equalf(t, 400, status, "%s", raw)
	}
	delete(body, "choices")
	status, raw = runtimeHTTPRequest(t, env, http.MethodPut, path, body)
	require.Equalf(t, 200, status, "%s", raw)
	require.NotContains(t, string(raw), "options.agent")
}

// Runtime fixtures use owner-local authority without inventing a bearer token.
func runtimeHTTPRequest(t *testing.T, env *testenv.Env, method, path string, body any) (int, []byte) {
	t.Helper()
	response, raw := envDoRaw(t, env, method, path, body, nil)
	return response.StatusCode, raw
}
