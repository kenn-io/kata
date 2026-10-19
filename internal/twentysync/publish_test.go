package twentysync

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
	"go.kenn.io/kata/internal/db"
)

func TestPublishTwenty(t *testing.T) {
	var redirectRequests atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectRequests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer destination.Close()
	for _, scenario := range []string{"success", "empty_body", "required_custom", "optional_custom", "schema", "workspace", "drop", "503", "malformed", "empty_response", "redirect", "read_failure", "closed", "admission", "oversize"} {
		t.Run(scenario, func(t *testing.T) {
			creates, admissions := 0, 0
			client, c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer example-token", r.Header.Get("Authorization"))
				if r.URL.Path == "/metadata" {
					fields := defaultFields()
					for _, f := range fields {
						f["isSystem"] = false
						f["isNullable"] = true
						f["defaultValue"] = nil
					}
					if scenario == "schema" {
						fields[1]["type"] = "TEXT"
					}
					if scenario == "required_custom" || scenario == "optional_custom" {
						fields = append(fields, map[string]any{"id": "44444444-4444-4444-8444-444444444444", "name": "customRequired", "type": "TEXT", "isActive": true, "isSystem": false, "isNullable": scenario == "optional_custom", "defaultValue": nil})
					}
					workspace := workspaceID
					if scenario == "workspace" {
						workspace = taskID
					}
					standardMetadata(t, w, r, workspace, fields)
					return
				}
				if r.Method == http.MethodPost {
					require.Equal(t, "/rest/tasks", r.URL.Path)
					creates++
					require.Equal(t, 1, admissions)
					var p map[string]any
					require.NoError(t, json.UnmarshalRead(r.Body, &p))
					body := "**世界**\nBody"
					if scenario == "empty_body" {
						body = ""
					}
					require.Equal(t, map[string]any{"title": "Native 🐟", "bodyV2": map[string]any{"markdown": body}, "status": "TODO"}, p)
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
					case "malformed":
						_, _ = w.Write([]byte(`{"data":`))
						return
					case "empty_response":
						w.WriteHeader(201)
						return
					case "redirect":
						w.Header().Set("Location", destination.URL)
						w.WriteHeader(307)
						return
					}
					writeJSON(t, w, map[string]any{"data": map[string]any{"createTask": map[string]any{"id": taskID}}})
					return
				}
				require.Equal(t, "/rest/tasks/"+taskID, r.URL.Path)
				if scenario == "read_failure" {
					w.WriteHeader(404)
					return
				}
				row := taskWire(taskID)
				if scenario == "closed" {
					row["status"] = "DONE"
				}
				writeJSON(t, w, map[string]any{"data": map[string]any{"task": row}})
			})
			raw, err := EncodeConfig(c)
			require.NoError(t, err)
			session, err := NewAdapter(nil, client).OpenPublish(t.Context(), db.IssueSyncBinding{Provider: "twenty", SourceKey: c.SourceKey(), RemoteID: c.RemoteID(), Config: raw})
			require.NoError(t, err)
			i := db.Issue{Title: "Native 🐟", Body: "**世界**\nBody", Status: "open"}
			if scenario == "empty_body" {
				i.Body = ""
			}
			if scenario == "oversize" {
				i.Body = strings.Repeat("x", maxMarkdownBytes+1)
			}
			payload, err := session.PrepareCreate(t.Context(), i)
			if scenario == "required_custom" || scenario == "schema" || scenario == "workspace" || scenario == "oversize" {
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
			if scenario == "success" || scenario == "empty_body" || scenario == "optional_custom" {
				require.NoError(t, err)
				require.Equal(t, "task:"+taskID, remote.ExternalID)
				require.Equal(t, "open", remote.Status)
				require.NotNil(t, remote.SourceUpdatedAt)
				_, err = session.ReadCreated(t.Context(), remote)
				require.NoError(t, err)
				_, err = session.ReadCreated(t.Context(), db.IssuePublicationRemote{ExternalID: taskID})
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "secret")
				if scenario == "read_failure" || scenario == "closed" {
					require.Equal(t, "task:"+taskID, remote.ExternalID)
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
