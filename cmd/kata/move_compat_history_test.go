package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestMoveCLI_LegacyDaemonContract(t *testing.T) {
	for _, single := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			t.Run(map[bool]string{false: "tree", true: "single"}[single]+map[bool]string{false: "_write", true: "_dry"}[dry], func(t *testing.T) {
				moves := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/health":
						_, _ = w.Write([]byte(`{"ok":true,"api_schema_version":"0.34.0"}`))
					case "/api/v1/projects/resolve":
						_, _ = w.Write([]byte(`{"project":{"id":1,"name":"source-project","uid":"01HZNQ7VFPK1XGD8R5MABXD4EX"}}`))
					case "/api/v1/projects":
						_, _ = w.Write([]byte(`{"projects":[{"id":1,"name":"source-project","uid":"01HZNQ7VFPK1XGD8R5MABXD4EX"},{"id":2,"name":"target-project","uid":"01HZNQ7VFPK1XGD8R5MABCD4EX"}]}`))
					case "/api/v1/projects/1", "/api/v1/projects/2":
						_, _ = w.Write([]byte(`{"aliases":[]}`))
					case "/api/v1/projects/1/issues/abc1":
						_, _ = w.Write([]byte(`{"issue":{"id":1,"project_id":1,"short_id":"abc1","revision":1}}`))
					case "/api/v1/projects/1/issues/abc1/actions/move":
						moves++
						var body map[string]any
						require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
						assert.NotContains(t, body, "single", "legacy daemons reject this property")
						_, _ = w.Write([]byte(`{"issue":{"id":1,"project_id":1,"short_id":"abc1"},"changed":true,"new_short_id":"abc1"}`))
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(server.Close)
				args := []string{"--project", "source-project", "move", "abc1", "target-project"}
				if single {
					args = append(args, "--single")
				}
				if dry {
					args = append(args, "--dry")
				}
				_, _, err := executeRootCapture(t, contextWithBaseURL(context.Background(), server.URL), args...)
				if single {
					require.NoError(t, err)
					assert.Equal(t, 1, moves)
				} else {
					require.Error(t, err)
					assert.Contains(t, err.Error(), "upgrade the daemon")
					assert.Zero(t, moves)
				}
			})
		}
	}
}

func TestMoveCLI_HistoricalReferenceUsesCurrentProject(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		for _, dry := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "_write", true: "_dry"}[dry], func(t *testing.T) {
				env, dir, source, current, _ := setupMoveCLIProjects(t)
				issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: source.ID, UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Title: "moving task", Author: "worker"})
				require.NoError(t, err)
				_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: current.ID, UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Title: "collision", Author: "worker"})
				require.NoError(t, err)
				_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: current.ID, IfMatchRev: issue.Revision, Actor: "worker"})
				require.NoError(t, err)
				moved, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				require.NotEqual(t, issue.ShortID, moved.ShortID)
				target, err := env.DB.CreateProject(t.Context(), "next-project")
				require.NoError(t, err)
				args := []string{"move", source.Name + "#" + issue.ShortID, target.Name}
				if dry {
					args = append(args, "--dry")
				}
				if mode == "agent" {
					args = append(args, "--agent")
				}
				if mode == "json" {
					args = append(args, "--json")
				}
				out := runCLI(t, env, dir, args...)
				if mode != "json" || dry {
					assert.Contains(t, out, current.Name+"#"+moved.ShortID)
					assert.NotContains(t, out, source.Name+"#"+moved.ShortID)
				} else {
					var response moveResponseWire
					require.NoError(t, json.Unmarshal([]byte(out), &response))
					assert.Equal(t, target.ID, response.Issue.ProjectID)
					assert.Equal(t, issue.UID, response.Issue.UID)
				}
			})
		}
	}
}
