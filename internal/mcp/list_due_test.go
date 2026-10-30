package mcpserver

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"slices"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestMCPListDueBy(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	create := func(projectID int64, title string, meta map[string]string) {
		values := map[string]jsontext.Value{}
		for key, value := range meta {
			values[key] = jsontext.Value(fmt.Sprintf("%q", value))
		}
		_, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: projectID, Title: title, Author: "example-agent", Metadata: values,
		})
		require.NoError(t, err)
	}
	create(project.ID, "scheduled", map[string]string{"scheduled_on": "2026-11-02"})
	create(project.ID, "deadline instant", map[string]string{"deadline_on": "2026-11-02T23:30:00Z"})
	create(project.ID, "future", map[string]string{"scheduled_on": "2026-11-03"})
	create(project.ID, "undated", nil)
	create(other.ID, "other project", map[string]string{"scheduled_on": "2026-11-01"})

	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP)
	require.NoError(t, err)
	bound, err := NewBoundScope(ProjectIdentity{ID: project.ID, UID: project.UID, Name: project.Name})
	require.NoError(t, err)

	type listed struct {
		Total  *int64 `json:"total"`
		Issues []struct {
			Title           string `json:"title"`
			ScheduledOnDate string `json:"scheduled_on_date"`
			DeadlineOnDate  string `json:"deadline_on_date"`
		} `json:"issues"`
	}
	call := func(t *testing.T, scope *Scope, args map[string]any) (listed, *sdkmcp.CallToolResult) {
		t.Helper()
		session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "example-agent", Version: "test"})
		result, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.list", Arguments: args})
		require.NoError(t, err)
		var page listed
		if !result.IsError {
			require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &page))
		}
		return page, result
	}
	titles := func(page listed) []string {
		var out []string
		for _, row := range page.Issues {
			out = append(out, row.Title)
		}
		slices.Sort(out)
		return out
	}

	t.Run("bound project", func(t *testing.T) {
		page, result := call(t, bound, map[string]any{"due_by": "2026-11-02", "time_zone": "UTC", "status": "open", "include_total": true})
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		assert.Equal(t, []string{"deadline instant", "scheduled"}, titles(page))
		require.NotNil(t, page.Total)
		assert.Equal(t, int64(2), *page.Total)
		for _, row := range page.Issues {
			if row.Title == "scheduled" {
				assert.Equal(t, "2026-11-02", row.ScheduledOnDate)
			} else {
				assert.Equal(t, "2026-11-02", row.DeadlineOnDate)
			}
		}
	})
	t.Run("time zone moves the day boundary", func(t *testing.T) {
		page, result := call(t, bound, map[string]any{"due_by": "2026-11-02", "time_zone": "Asia/Tokyo"})
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		assert.Equal(t, []string{"scheduled"}, titles(page))
	})
	t.Run("every project in scope", func(t *testing.T) {
		page, result := call(t, NewAllScope(), map[string]any{"due_by": "2026-11-02", "time_zone": "UTC"})
		require.False(t, result.IsError, "%s", mustJSON(t, result))
		assert.Equal(t, []string{"deadline instant", "other project", "scheduled"}, titles(page))
	})
	t.Run("invalid input", func(t *testing.T) {
		for _, args := range []map[string]any{
			{"due_by": "tomorrow"},
			{"due_by": "2026-11-02", "time_zone": "Mars/Base"},
			{"time_zone": "UTC"},
		} {
			_, result := call(t, bound, args)
			assert.True(t, result.IsError, "%v", args)
		}
	})
}

func TestMCPListDueByRejectsOldDaemon(t *testing.T) {
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/health", r.URL.Path)
		writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.34.0"})
	})
	handlers := toolHandlers{options: Options{Client: client, Scope: NewAllScope()}}
	_, _, err := handlers.list(t.Context(), nil, ListInput{DueBy: "2026-11-02"})
	require.ErrorContains(t, err, "due_by requires daemon API 0.35.0")
}
