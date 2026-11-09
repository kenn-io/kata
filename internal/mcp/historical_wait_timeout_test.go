package mcpserver

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitHistoricalProjectLookupDeadlineRetainsLastStates(t *testing.T) {
	var shows atomic.Int64
	var historicalCatalog atomic.Bool
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" {
			if r.URL.Query().Get("include") == "archived" {
				historicalCatalog.Store(true)
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"), projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project")}})
			return
		}
		issue := issueJSON(1, "spoke-project", "abc1")
		if shows.Add(1) > 1 {
			issue = issueJSON(2, "hub-project", "def1")
			issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
		}
		writeJSON(w, map[string]any{"issue": issue, "labels": []any{}, "links": []any{}, "comments": []any{}})
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"}, {ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"}})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
	_, output, err := handlers.wait(t.Context(), nil, WaitInput{Refs: []string{"spoke-project#abc1"}, Status: "closed", TimeoutSeconds: 1})
	require.True(t, historicalCatalog.Load(), "deadline must occur in the added historical project lookup")
	require.NoError(t, err, "catalog lookup expiry is the documented timeout outcome")
	require.Equal(t, "timeout", output.Reason)
	require.Equal(t, []WaitState{{Ref: "spoke-project#abc1", Status: "open", Matched: false}}, output.States, "retain the last complete round rather than partial moved state")
}
