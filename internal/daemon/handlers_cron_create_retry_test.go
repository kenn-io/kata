package daemon_test

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/testenv"
)

type cronLostResponseTransport struct{ next http.RoundTripper }

func (transport cronLostResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return nil, context.DeadlineExceeded
}

func TestCronStableCreateRetryReadback(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("tok"))
			project := seedProject(t, env, "retry-project")
			id := cronHTTPUID(t)
			path := fmt.Sprintf("/api/v1/projects/%d/cron/%ss/%s", project.ID, kind, id)
			var definition any = cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "git status --short"}}}
			if kind == "job" {
				job := cronHTTPDefinition(t)
				job.Kind, job.Enabled, job.Trigger = "job", false, cron.Trigger{Kind: "manual"}
				definition = job
			}
			body := map[string]any{"actor": "worker", "name": "Inspect", "definition": definition}
			encoded, err := json.Marshal(body)
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, env.URL+path, bytes.NewReader(encoded))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer tok")
			request.Header.Set("Content-Type", "application/json")
			transport := env.HTTP.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			lost := &http.Client{Transport: cronLostResponseTransport{next: transport}}
			_, err = lost.Do(request)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			beforeRetry, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			status, raw := cronHTTP(t, env, http.MethodPut, path, body)
			require.Equalf(t, 409, status, "%s", raw)
			afterRetry, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			require.Equal(t, beforeRetry, afterRetry, "retry emits no new definition")
			read := func() (string, string, *time.Time, jsontext.Value) {
				t.Helper()
				status, raw := cronHTTP(t, env, http.MethodGet, path, nil)
				require.Equalf(t, 200, status, "%s", raw)
				var envelope map[string]jsontext.Value
				require.NoError(t, json.Unmarshal(raw, &envelope))
				var value struct {
					UID        string         `json:"uid"`
					Name       string         `json:"name"`
					EventUID   string         `json:"definition_event_uid"`
					DeletedAt  *time.Time     `json:"deleted_at"`
					Definition jsontext.Value `json:"definition"`
				}
				require.NoError(t, json.Unmarshal(envelope[kind], &value))
				require.Equal(t, id, value.UID)
				return value.EventUID, value.Name, value.DeletedAt, value.Definition
			}
			winner, name, deleted, seenDefinition := read()
			require.Equal(t, "Inspect", name)
			require.Nil(t, deleted)
			intent, err := json.Marshal(definition)
			require.NoError(t, err)
			require.JSONEq(t, string(intent), string(seenDefinition))
			require.Contains(t, string(raw), winner)
			// A later edit and tombstone are conflicts, never recovered creates.
			body["name"], body["expected_event_uid"] = "Later edit", winner
			status, raw = cronHTTP(t, env, http.MethodPut, path, body)
			require.Equalf(t, 200, status, "%s", raw)
			body["name"] = "Inspect"
			delete(body, "expected_event_uid")
			status, raw = cronHTTP(t, env, http.MethodPut, path, body)
			require.Equalf(t, 409, status, "%s", raw)
			newWinner, name, deleted, _ := read()
			require.NotEqual(t, winner, newWinner)
			require.Equal(t, "Later edit", name)
			require.Nil(t, deleted)
			status, raw = cronHTTP(t, env, http.MethodDelete, path, map[string]any{"actor": "worker", "expected_event_uid": newWinner})
			require.Equalf(t, 200, status, "%s", raw)
			status, raw = cronHTTP(t, env, http.MethodPut, path, body)
			require.Equalf(t, 409, status, "%s", raw)
			_, _, deleted, _ = read()
			require.NotNil(t, deleted)
		})
	}
}
