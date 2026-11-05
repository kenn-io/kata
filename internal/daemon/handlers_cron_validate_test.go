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
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Invalid definitions are preflight results; malformed envelopes are requests.
func TestCronValidateHTTP(t *testing.T) {
	env, project, _, native := nativeCronEnrollmentFixture(t, testenv.WithAuthToken("tok"))
	t.Run("collision wire", func(t *testing.T) {
		definition := cronHTTPDefinition(t)
		definition.Trigger = cron.Trigger{Kind: "cron", Cron: "0 9 * * *"}
		_, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Daily peer", Definition: definition, Actor: "operator"})
		require.NoError(t, err)
		definition.Enabled = false
		path := fmt.Sprintf("/api/v1/projects/%d/cron/jobs/validate?count=1&from=2026-11-03T00:00:00Z", project.ID)
		status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"name": "Candidate", "definition": definition})
		require.Equal(t, http.StatusOK, status, string(raw))
		var out map[string]any
		require.NoError(t, json.Unmarshal(raw, &out))
		require.Equal(t, true, out["valid"])
		require.Empty(t, out["errors"])
		require.Len(t, out["warnings"].([]any), 1)
		schedule := out["schedule"].(map[string]any)
		report, ok := schedule["collisions"].(map[string]any)
		require.True(t, ok, "previewable schedules need the collision report: %s", raw)
		items := report["items"].([]any)
		require.Len(t, items, 1)
		item := items[0].(map[string]any)
		require.IsType(t, float64(0), item["occurrences"])
		require.IsType(t, float64(0), item["peer_count"])
		require.IsType(t, "", item["local"])
		require.Len(t, schedule["next"], 1, "count bounds the next-fire preview")
		require.Equal(t, float64(7), item["occurrences"], "count does not shorten the collision horizon")
		require.Equal(t, "paused", out["warnings"].([]any)[0].(map[string]any)["code"])
	})
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
		require.NoError(t, json.Unmarshal(raw, &result))
		require.True(t, result.Valid)
		if resource == "jobs" {
			require.NotNil(t, result.Schedule)
			require.Len(t, result.Schedule.Next, 2)
			require.Equal(t, "2026-11-03T00:00:00Z", result.Schedule.From.Format(time.RFC3339))
		} else {
			require.Nil(t, result.Schedule)
		}
		{
			response := doReqEnv(t, env, http.MethodPost, env.URL+path, `{"name":"Inspect","definition":[]}`, "")
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
	for _, query := range []string{"?count=bad", "?count=0", "?count=101"} {
		status, raw := cronHTTP(t, env, http.MethodPost, path+query, map[string]any{"name": "Inspect", "definition": cronHTTPDefinition(t)})
		require.Equalf(t, 400, status, "%s", raw)
	}
	response := doReqEnv(t, env, http.MethodPost, env.URL+path, `{"name":"`+strings.Repeat("x", 1<<20)+`"}`, "")
	require.Equalf(t, 413, response.StatusCode, "%s", readClose(t, response))
	status, raw := cronHTTP(t, env, http.MethodPost, "/api/v1/projects/999999/cron/jobs/validate", map[string]any{"name": "Inspect", "definition": cronHTTPDefinition(t)})
	require.Equalf(t, 404, status, "%s", raw)
}

func TestCronValidateInvalidFromPrecedesDocument(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project := seedProject(t, env, "example-project")
	for _, resource := range []string{"jobs", "workflows"} {
		for _, from := range []string{"bad", "0000-01-01T00:00:00+14:00", "9999-12-31T23:59:59-12:00"} {
			t.Run(resource+"/"+from, func(t *testing.T) {
				path := fmt.Sprintf("/api/v1/projects/%d/cron/%s/validate?from=%s", project.ID, resource, url.QueryEscape(from))
				documents := []any{jsontext.Value(`[]`)}
				if resource == "workflows" {
					documents = append(documents, map[string]any{"version": 1, "steps": []any{map[string]any{"key": "review", "kind": "prompt", "prompt": "Review"}}})
				}
				for _, doc := range documents {
					status, raw := cronHTTP(t, env, http.MethodPost, path, map[string]any{"name": "Inspect", "definition": doc})
					require.Equal(t, http.StatusBadRequest, status, "%s", raw)
				}
			})
		}
	}
}
