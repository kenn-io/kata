package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Invalid definitions are preflight results; malformed envelopes are requests.
func TestCronValidateHTTP(t *testing.T) {
	env, project, _, native := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	for _, resource := range []string{"jobs", "workflows"} {
		path := fmt.Sprintf("/api/v1/projects/%d/cron/%s/validate", project.ID, resource)
		definition := any(native)
		if resource == "workflows" {
			definition = map[string]any{"version": 1, "steps": []any{map[string]any{"key": "inspect", "kind": "prompt", "prompt": "Inspect"}}}
		}
		before, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		status, raw := cronHTTP(t, env, http.MethodPost, path+"?count=2&from=2026-11-03T00:00:00Z", map[string]any{"name": "Inspect", "definition": definition})
		require.Equalf(t, 200, status, "%s", raw)
		var result db.CronValidation
		result = db.CronValidation{}
		require.NoError(t, json.Unmarshal(raw, &result))
		require.True(t, result.Valid)
		if resource == "jobs" {
			require.NotNil(t, result.Schedule)
			require.Len(t, result.Schedule.Next, 2)
			require.Equal(t, "2026-11-03T00:00:00Z", result.Schedule.From.Format(time.RFC3339))
		} else {
			require.Nil(t, result.Schedule)
		}
		for _, doc := range []string{`[]`} {
			response := doReqEnv(t, env, http.MethodPost, env.URL+path, `{"name":"Inspect","definition":`+doc+`}`, "")
			raw := readClose(t, response)
			require.Equalf(t, 200, response.StatusCode, "%s", raw)
			result = db.CronValidation{}
			require.NoError(t, json.Unmarshal(raw, &result))
			require.False(t, result.Valid)
			require.Equal(t, "document", result.Errors[0].Stage)
			require.Nil(t, result.Schedule)
		}
		bodies := []string{`{"uid":null}`}
		if resource == "jobs" {
			bodies = []string{`null`, `[]`, `1`, `{`, `{"name":1}`, `{"name":"Inspect","extra":true}`, `{"name":null}`, `{"uid":null}`, `{"expected_event_uid":null}`, `{"actor":null}`}
		}
		for _, body := range bodies {
			response := doReqEnv(t, env, http.MethodPost, env.URL+path, body, "")
			raw := readClose(t, response)
			require.Equalf(t, 400, response.StatusCode, "body %s: %s", body, raw)
		}

		after, err := env.DB.MaxEventID(t.Context())
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate", project.ID)
	for _, query := range []string{"?count=0", "?count=101", "?count=bad"} {
		status, raw := cronHTTP(t, env, http.MethodPost, path+query, map[string]any{"name": "Inspect", "definition": cronHTTPDefinition(t)})
		require.Equalf(t, 400, status, "%s", raw)
	}
	response := doReqEnv(t, env, http.MethodPost, env.URL+path, `{"name":"`+strings.Repeat("x", 1<<20)+`"}`, "")
	require.Equalf(t, 413, response.StatusCode, "%s", readClose(t, response))
	status, raw := cronHTTP(t, env, http.MethodPost, "/api/v1/projects/999999/cron/jobs/validate", map[string]any{"name": "Inspect", "definition": cronHTTPDefinition(t)})
	require.Equalf(t, 404, status, "%s", raw)
}

func TestCronValidateEnrollment(t *testing.T) {
	env, project, _, definition := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	enrollment := createClaimEnrollment(t, env, project.ID, cronHTTPUID(t), "push")
	path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate?count=2&from=2026-11-03T00:00:00Z", project.ID)
	response, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{"name": "Inspect", "definition": definition}, map[string]string{"Authorization": "Bearer " + enrollment.Token})
	require.Equalf(t, 403, response.StatusCode, "%s", raw)
}
func TestCronValidateInvalidFromPrecedesDocument(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	for _, resource := range []string{"jobs", "workflows"} {
		for _, from := range []string{"bad", "0000-01-01T00:00:00+14:00", "9999-12-31T23:59:59-12:00"} {
			t.Run(resource+"/"+from, func(t *testing.T) {
				path := fmt.Sprintf("/api/v1/projects/%d/cron/%s/validate?from=%s", project.ID, resource, url.QueryEscape(from))
				definition := any(cronHTTPDefinition(t))
				if resource == "workflows" {
					definition = map[string]any{"version": 1, "steps": []any{map[string]any{"key": "review", "kind": "prompt", "prompt": "Review"}}}
				}
				documents := []any{jsontext.Value(`[]`)}
				if resource == "workflows" {
					documents = append(documents, definition)
				}
				for _, doc := range documents {
					status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"name": "Inspect", "definition": doc})
					require.Equal(t, http.StatusBadRequest, status, "%s", raw)
				}
			})
		}
	}
}
