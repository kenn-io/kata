package todoistsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func TestPublishTodoist(t *testing.T) {
	var redirectRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectRequests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer destination.Close()
	for _, scenario := range []string{"success", "empty_body", "null_updated", "wrong_account", "archived", "wrong_project", "closed", "incomplete", "drop", "503", "empty_response", "malformed", "redirect", "admission", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			creates, admissions := 0, 0
			cfg := testConfig()
			row := testTask()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer fixture-secret", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				reply := func(v any) { raw, err := json.Marshal(v); require.NoError(t, err); _, _ = w.Write(raw) }
				switch r.URL.Path {
				case "/api/v1/user":
					id := cfg.AccountID
					if scenario == "wrong_account" {
						id = "7654321"
					}
					reply(map[string]any{"id": id})
				case "/api/v1/projects/" + cfg.ProjectID:
					reply(map[string]any{"id": cfg.ProjectID, "name": "Example tasks", "is_archived": scenario == "archived", "is_deleted": false})
				case "/api/v1/tasks":
					require.Equal(t, http.MethodPost, r.Method)
					creates++
					require.Equal(t, 1, admissions)
					var payload map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &payload))
					body := "**世界**\nBody"
					if scenario == "empty_body" {
						body = ""
					}
					require.Equal(t, map[string]any{"content": "Native 🐟", "description": body, "project_id": cfg.ProjectID}, payload)
					switch scenario {
					case "drop":
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = conn.Close()
						return
					case "503":
						w.WriteHeader(503)
						_, _ = w.Write([]byte("secret diagnostic"))
						return
					case "empty_response":
						w.WriteHeader(200)
						return
					case "malformed":
						_, _ = w.Write([]byte(`{"id":`))
						return
					case "redirect":
						w.Header().Set("Location", destination.URL)
						w.WriteHeader(307)
						return
					case "wrong_project":
						row.ProjectID = "otherproject"
					case "closed":
						row.Checked = true
					}
					v := taskJSON(row)
					if scenario == "null_updated" {
						v["updated_at"] = nil
					}
					if scenario == "incomplete" {
						delete(v, "checked")
					}
					reply(v)
				case "/api/v1/tasks/" + row.ID:
					v := taskJSON(row)
					if scenario == "null_updated" {
						v["updated_at"] = nil
					}
					reply(v)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			cfg.APIOrigin = server.URL
			client := NewClient(ClientConfig{Daemon: config.TodoistSyncConfig{APIOrigin: server.URL, TokenEnv: "EXAMPLE_TOKEN"}, LookupEnv: func(string) (string, bool) { return "fixture-secret", true }})
			raw, err := EncodeConfig(cfg)
			require.NoError(t, err)
			session, err := NewAdapter(nil, client).OpenPublish(t.Context(), db.IssueSyncBinding{Provider: "todoist", SourceKey: cfg.SourceKey(), RemoteID: cfg.RemoteID(), Config: raw})
			if scenario == "wrong_account" {
				require.Error(t, err)
				require.Zero(t, creates)
				return
			}
			require.NoError(t, err)
			issue := db.Issue{Title: "Native 🐟", Body: "**世界**\nBody", Status: "open"}
			if scenario == "empty_body" {
				issue.Body = ""
			}
			if scenario == "oversize" {
				issue.Body = strings.Repeat("x", (1<<20)+1)
			}
			payload, err := session.PrepareCreate(t.Context(), issue)
			if scenario == "archived" || scenario == "oversize" {
				require.Error(t, err)
				require.Zero(t, creates)
				return
			}
			require.NoError(t, err)
			remote, err := session.Create(t.Context(), payload, func(context.Context) error {
				admissions++
				if scenario == "admission" {
					return errors.New("denied")
				}
				return nil
			})
			if scenario == "success" || scenario == "empty_body" || scenario == "null_updated" {
				require.NoError(t, err)
				require.Equal(t, "task:"+row.ID, remote.ExternalID)
				require.Equal(t, "open", remote.Status)
				if scenario == "null_updated" {
					require.Nil(t, remote.SourceUpdatedAt)
				} else {
					require.Equal(t, row.UpdatedAt, *remote.SourceUpdatedAt)
				}
				_, err = session.ReadCreated(t.Context(), remote)
				require.NoError(t, err)
				_, err = session.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: row.ID})
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret")
				if scenario == "incomplete" || scenario == "closed" {
					require.Equal(t, "task:"+row.ID, remote.ExternalID)
				}
			}
			if scenario == "admission" {
				require.Zero(t, creates)
			} else {
				require.Equal(t, 1, creates)
			}
		})
	}
	require.Zero(t, redirectRequests.Load(), "publication redirects must not reach another origin")
}
