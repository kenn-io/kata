package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronCLIHistory(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, kind := range []string{"job", "workflow"} {
		t.Run(kind, func(t *testing.T) {
			var identity string
			if kind == "job" {
				value, _, err := env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cronCLIJob(t).Definition.Native()})
				require.NoError(t, err)
				identity = value.UID
				def := value.Definition
				def.Options = jsontext.Value(`{"limit":9007199254740994,"decimal":1.000000000000000000001}`)
				_, _, err = env.DB.PutCronJob(t.Context(), db.PutCronJob{ProjectID: project.ID, UID: identity, Name: value.Name, Actor: "editor", Definition: def, ExpectedEventUID: value.DefinitionEventUID})
				require.NoError(t, err)
			} else {
				value, _, err := env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, Name: "Inspect", Actor: "operator", Definition: cron.WorkflowDefinition{Version: 1, Steps: []cron.WorkflowStep{{Key: "inspect", Kind: "command", Command: "true"}}, Options: jsontext.Value(`{"limit":9007199254740993}`)}})
				require.NoError(t, err)
				identity = value.UID
				def := value.Definition
				def.Options = jsontext.Value(`{"limit":9007199254740994,"decimal":1.000000000000000000001}`)
				_, _, err = env.DB.PutCronWorkflow(t.Context(), db.PutCronWorkflow{ProjectID: project.ID, UID: identity, Name: value.Name, Actor: "editor", Definition: def, ExpectedEventUID: value.DefinitionEventUID})
				require.NoError(t, err)
			}
			out, _, err := executeCronCLI(t, env.URL, nil, kind, "history", strings.ToLower(identity), "--diff")
			require.NoError(t, err)
			require.Contains(t, out, `9007199254740993`)
			require.Contains(t, out, `9007199254740994`)
			require.Contains(t, out, `1.000000000000000000001`)
			var page struct {
				Versions []struct {
					Changes jsontext.Value `json:"changes"`
				} `json:"versions"`
				Next string `json:"next_before_event_uid"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &page))
			require.Len(t, page.Versions, 2)
			require.Equal(t, `null`, string(page.Versions[1].Changes))
			require.Empty(t, page.Next)
			var changes []cron.Change
			require.NoError(t, json.Unmarshal(page.Versions[0].Changes, &changes))
			require.Len(t, changes, 2)
			require.Equal(t, "/definition/options/decimal", changes[0].Path)
			require.Equal(t, "/definition/options/limit", changes[1].Path)
			out, _, err = executeCronCLI(t, env.URL, nil, kind, "history", identity)
			require.NoError(t, err)
			require.NotContains(t, out, `"changes"`)
		})
	}
}

func historyStub(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects":
			_, _ = io.WriteString(w, `{"projects":[{"id":7,"name":"spoke-project"}]}`)
		case "/api/v1/projects/resolve":
			_, _ = io.WriteString(w, `{"project_id":7,"name":"spoke-project"}`)
		case "/api/v1/projects/7":
			_, _ = io.WriteString(w, `{"project":{"id":7,"name":"spoke-project"}}`)
		default:
			handler(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestCronCLIHistoryLookahead(t *testing.T) {
	for _, kind := range []string{"job", "workflow"} {
		for _, limit := range []int{1, 100} {
			t.Run(fmt.Sprintf("%s_%d", kind, limit), func(t *testing.T) {
				identity, cursor, older := cronCLIUID(t), cronCLIUID(t), cronCLIUID(t)
				var calls atomic.Int32
				server := historyStub(t, func(w http.ResponseWriter, r *http.Request) {
					call := calls.Add(1)
					require.Equal(t, fmt.Sprintf("/api/v1/projects/7/cron/%ss/%s/versions", kind, identity), r.URL.Path)
					value := map[string]any{"event_uid": cursor, "name": "Inspect", "definition": map[string]any{"enabled": false}, "actor": "editor", "current": true}
					page := map[string]any{}
					if call == 1 {
						require.Equal(t, strconv.Itoa(limit), r.URL.Query().Get("limit"))
						require.Empty(t, r.URL.Query().Get("before_event_uid"))
						values := make([]any, limit)
						for i := range values {
							values[i] = value
						}
						page["versions"] = values
						page["next_before_event_uid"] = cursor
					} else {
						require.Equal(t, int32(2), call)
						require.Equal(t, "1", r.URL.Query().Get("limit"))
						require.Equal(t, cursor, r.URL.Query().Get("before_event_uid"))
						page["versions"] = []any{map[string]any{"event_uid": older, "name": "Inspect", "definition": map[string]any{"enabled": true}, "actor": "operator", "current": false}}
					}
					raw, err := json.Marshal(page)
					require.NoError(t, err)
					_, _ = w.Write(raw)
				})
				out, _, err := executeCronCLI(t, server.URL, nil, kind, "history", identity, "--diff", "--limit", strconv.Itoa(limit))
				require.NoError(t, err)
				require.Equal(t, int32(2), calls.Load())
				var page struct {
					Versions []struct {
						Changes []cron.Change `json:"changes"`
					} `json:"versions"`
					Next string `json:"next_before_event_uid"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &page))
				require.Len(t, page.Versions, limit)
				require.Equal(t, cursor, page.Next)
				if limit > 1 {
					require.NotNil(t, page.Versions[0].Changes)
					require.Empty(t, page.Versions[0].Changes)
				}
				require.Len(t, page.Versions[limit-1].Changes, 1)
				require.Equal(t, "/definition/enabled", page.Versions[limit-1].Changes[0].Path)
			})
		}
	}
}

func TestCronCLIHistoryLookaheadFailure(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(strconv.FormatBool(empty), func(t *testing.T) {
			identity, cursor := cronCLIUID(t), cronCLIUID(t)
			var calls atomic.Int32
			server := historyStub(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Query().Get("before_event_uid") != "" {
					if empty {
						_, _ = io.WriteString(w, `{"versions":[]}`)
					} else {
						w.WriteHeader(400)
						_, _ = io.WriteString(w, `{"error":{"code":"validation","message":"version not found"}}`)
					}
					return
				}
				_, _ = fmt.Fprintf(w, `{"versions":[{"event_uid":%q,"name":"Inspect","definition":{}}],"next_before_event_uid":%q}`, cursor, cursor)
			})
			out, _, err := executeCronCLI(t, server.URL, nil, "job", "history", identity, "--diff")
			require.Error(t, err)
			require.Equal(t, int32(2), calls.Load())
			if empty {
				require.Contains(t, err.Error(), "history changed")
			} else {
				require.Contains(t, err.Error(), "version not found")
			}
			require.Empty(t, out)
		})
	}
}

func TestCronCLIHistoryDaemonCompatibility(t *testing.T) {
	for _, domain := range []bool{false, true} {
		t.Run(strconv.FormatBool(domain), func(t *testing.T) {
			identity := cronCLIUID(t)
			server := historyStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(404)
				if domain {
					_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"cron not found"}}`)
				} else {
					_, _ = io.WriteString(w, "404 page not found\n")
				}
			})
			_, _, err := executeCronCLI(t, server.URL, nil, "job", "history", identity)
			require.Error(t, err)
			if domain {
				require.Contains(t, err.Error(), "cron not found")
				require.NotContains(t, err.Error(), "upgrade")
			} else {
				require.Contains(t, err.Error(), "daemon does not support cron definition history; upgrade it")
			}
		})
	}
}

func TestCronCLIHistoryFlagsRegistered(t *testing.T) {
	for _, resource := range []string{"job", "workflow"} {
		t.Run(resource, func(t *testing.T) {
			command := newCronOperationCmd(resource, "history")
			require.NotNil(t, command.Flags().Lookup("diff"))
			require.NotNil(t, command.Flags().Lookup("before-event-uid"))
		})
	}
}

func TestCronCLIHistorySkippedDiagnostics(t *testing.T) {
	identity, cursor, older := cronCLIUID(t), cronCLIUID(t), cronCLIUID(t)
	ids := make([]string, 21)
	for i := range ids {
		ids[i] = fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5F%02d", i)
	}
	server := historyStub(t, func(w http.ResponseWriter, r *http.Request) {
		page := map[string]any{"versions": []any{map[string]any{"event_uid": cursor, "name": "Inspect", "definition": map[string]any{}}}, "next_before_event_uid": cursor, "skipped_event_uids": ids[1:]}
		if r.URL.Query().Get("before_event_uid") != "" {
			page = map[string]any{"versions": []any{map[string]any{"event_uid": older, "name": "Inspect", "definition": map[string]any{}}}, "skipped_event_uids": ids[:20]}
		}
		raw, err := json.Marshal(page)
		require.NoError(t, err)
		_, _ = w.Write(raw)
	})
	out, _, err := executeCronCLI(t, server.URL, nil, "job", "history", identity, "--diff")
	require.NoError(t, err)
	var page struct {
		Skipped []string `json:"skipped_event_uids"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &page))
	require.Equal(t, ids[:20], page.Skipped)
}
