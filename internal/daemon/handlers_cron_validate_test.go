package daemon_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Invalid definitions are preflight results; malformed envelopes are requests.
func TestCronValidateHTTP(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	for _, resource := range []string{"jobs", "workflows"} {
		path := fmt.Sprintf("/api/v1/projects/%d/cron/%s/validate", project.ID, resource)
		definition := any(cronHTTPDefinition(t))
		if resource == "workflows" {
			definition = map[string]any{"version": 1, "steps": []any{map[string]any{"key": "inspect", "kind": "prompt", "prompt": "Inspect", "options": jsontext.Value(`{"n":12345678901234567890123}`)}}}
		}
		before, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"name": "  Inspect  ", "definition": definition})
		require.Equalf(t, 200, status, "%s", raw)
		var result db.CronValidation
		result = db.CronValidation{}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.True(t, result.Valid)
		require.Equal(t, "Inspect", result.Name)
		if resource == "jobs" {
			require.NotNil(t, result.Schedule)
			require.Empty(t, result.Schedule.Next)
			require.Equal(t, "adapter_defined", result.Schedule.Basis)
		} else {
			require.Nil(t, result.Schedule)
			require.Contains(t, string(raw), "12345678901234567890123")
		}
		for _, doc := range []string{`null`, `[]`, `1`, `"text"`, `{}`, `{"unknown":1}`} {
			response := doReqEnv(t, env, http.MethodPost, env.URL+path, `{"name":"Inspect","definition":`+doc+`}`, "")
			raw := readClose(t, response)
			require.Equalf(t, 200, response.StatusCode, "%s", raw)
			result = db.CronValidation{}
			require.NoError(t, json.Unmarshal(raw, &result))
			require.False(t, result.Valid)
			require.Equal(t, "document", result.Errors[0].Stage)
			require.Nil(t, result.Schedule)
		}
		status, raw = cronHTTP(t, env, http.MethodPost, path, map[string]any{"definition": definition})
		require.Equalf(t, 200, status, "%s", raw)
		result = db.CronValidation{}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.Equal(t, "request", result.Errors[0].Stage)
		for _, body := range []string{`null`, `[]`, `1`, `{`, `{"name":1}`, `{"name":"Inspect","extra":true}`, `{"uid":"bad"}`, `{"name":null}`} {
			response := doReqEnv(t, env, http.MethodPost, env.URL+path, body, "")
			raw := readClose(t, response)
			require.Equalf(t, 400, response.StatusCode, "body %s: %s", body, raw)
		}
		after, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate", project.ID)
	for _, query := range []string{"?count=0", "?count=101", "?count=bad", "?from=bad"} {
		status, raw := cronHTTP(t, env, http.MethodPost, path+query, map[string]any{"name": "Inspect", "definition": cronHTTPDefinition(t)})
		require.Equalf(t, 400, status, "%s", raw)
	}
	response := doReqEnv(t, env, http.MethodPost, env.URL+path, `{"name":"`+strings.Repeat("x", 1<<20)+`"}`, "")
	require.Equalf(t, 413, response.StatusCode, "%s", readClose(t, response))
	status, raw := cronHTTP(t, env, http.MethodPost, "/api/v1/projects/999999/cron/jobs/validate", map[string]any{"name": "Inspect", "definition": cronHTTPDefinition(t)})
	require.Equalf(t, 404, status, "%s", raw)
}

func TestCronValidateSpokeAndEnrollment(t *testing.T) {
	env, project, _, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate?count=2&from=2026-11-03T00:00:00Z", project.ID)
	response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"name": "Inspect", "definition": definition}, map[string]string{"Authorization": "Bearer " + enrollment.Token})
	require.Equalf(t, 403, response.StatusCode, "%s", raw)
	_, err := env.DB.UpsertFederationBinding(t.Context(), db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://daemon.example", HubProjectID: 42, HubProjectUID: project.UID, Enabled: true, Actor: "operator"})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"name": "Inspect", "definition": definition})
	require.Equalf(t, 200, status, "%s", raw)
	var out db.CronValidation
	require.NoError(t, json.Unmarshal(raw, &out))
	require.True(t, out.Valid)
	require.Len(t, out.Schedule.Next, 2)
	require.Equal(t, "hub_owned", out.Warnings[0].Code)
}

func TestCronValidateReadCapability(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	other := seedProject(t, env, "other-project")
	host := &capabilitiesReadHost{projectID: project.ID}
	server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, HostAccess: host})
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	raw, err := json.Marshal(map[string]any{"name": "Inspect", "definition": cronHTTPDefinition(t)})
	require.NoError(t, err)
	for _, tc := range []struct {
		id   int64
		want int
	}{{project.ID, 200}, {other.ID, 404}} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate", tc.id), bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request = request.WithContext(daemon.WithPrincipal(request.Context(), daemon.Principal{Kind: daemon.PrincipalHost, Subject: "reader", Actor: "reader"}))
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equal(t, tc.want, response.Code, response.Body.String())
	}
	require.Len(t, host.calls, 2)
	require.Equal(t, "read", string(host.calls[0].Policy.Capability))
	require.False(t, host.calls[0].Policy.Mutation)
}

func TestCronValidateInvalidFromPrecedesDocument(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	for _, resource := range []string{"jobs", "workflows"} {
		for _, from := range []string{"0000-01-01T00:00:00+14:00", "9999-12-31T23:59:59-12:00"} {
			t.Run(resource+"/"+from, func(t *testing.T) {
				path := fmt.Sprintf("/api/v1/projects/%d/cron/%s/validate?from=%s", project.ID, resource, url.QueryEscape(from))
				definition := any(cronHTTPDefinition(t))
				if resource == "workflows" {
					definition = map[string]any{"version": 1, "steps": []any{map[string]any{"key": "review", "kind": "prompt", "prompt": "Review"}}}
				}
				for _, doc := range []any{definition, jsontext.Value(`[]`)} {
					status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"name": "Inspect", "definition": doc})
					require.Equal(t, http.StatusBadRequest, status, "%s", raw)
				}
			})
		}
	}
}
