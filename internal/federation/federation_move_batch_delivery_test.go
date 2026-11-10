package federation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestSyncFederationBatchMoveConverges(t *testing.T) {
	hub, projects := newMoveDeliveryHub(t, "batch-source", "batch-a", "batch-b")
	spoke := newMoveDeliverySpoke(t, hub, projects)
	// Unlike store-only delivery tests, the HTTP move path refreshes claims.
	for _, project := range projects {
		grant, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{SpokeInstanceUID: spoke.env.DB.InstanceUID(), ProjectID: &project.ID, Capabilities: "claim,pull,push", Actor: "worker"})
		require.NoError(t, err)
		credential := spoke.credentials[project.UID]
		credential.Token = grant.Token
		spoke.credentials[project.UID] = credential
	}
	server := daemon.NewServer(daemon.ServerConfig{DB: spoke.env.DB, FederationCredentials: spoke.credentials})
	defer func() { _ = server.Close() }()
	local := httptest.NewServer(server.Handler())
	defer local.Close()
	move := func(issues []db.Issue) {
		t.Helper()
		var items []map[string]any
		for n, issue := range issues {
			items = append(items, map[string]any{"ref": issue.UID, "to_project_uid": spoke.projects[1+n%2].UID})
		}
		start := 0
		for start < len(items) {
			body, err := json.Marshal(map[string]any{"actor": "worker", "comment": "routed", "items": items, "start_index": start})
			require.NoError(t, err)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, local.URL+"/api/v1/issues/actions/move", bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := local.Client().Do(req)
			require.NoError(t, err)
			raw, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
			var out struct {
				NextIndex int `json:"next_index"`
				Summary   struct {
					Refused int `json:"refused"`
				} `json:"summary"`
			}
			require.NoError(t, json.Unmarshal(raw, &out))
			require.Zero(t, out.Summary.Refused, string(raw[:min(len(raw), 1200)]))
			require.Greater(t, out.NextIndex, start)
			start = out.NextIndex
		}
	}
	var issues []db.Issue
	for n := range 210 {
		issue, _, err := spoke.env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: spoke.projects[0].ID, Title: fmt.Sprintf("route %d", n), Author: "worker"})
		require.NoError(t, err)
		issues = append(issues, issue)
	}
	for i := range projects {
		require.NoError(t, spoke.sync(t, i))
	}
	move(issues[:200])
	for _, i := range []int{1, 2} {
		require.NoError(t, spoke.sync(t, i))
	}
	move(issues[200:])
	for _, i := range []int{1, 2} {
		require.NoError(t, spoke.sync(t, i))
	}
	for n, issue := range issues {
		got, err := hub.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, projects[1+n%2].ID, got.ProjectID)
		local, err := spoke.env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, local.ShortID, got.ShortID)
		old, err := hub.DB.IssueByShortID(t.Context(), projects[0].ID, issue.ShortID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, issue.UID, old.UID)
		comments, err := hub.DB.CommentsByIssue(t.Context(), got.ID)
		require.NoError(t, err)
		require.Len(t, comments, 1)
		require.Equal(t, "routed", comments[0].Body)
		events, err := hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: got.ProjectID, Limit: 1000})
		require.NoError(t, err)
		var moved, commented int64
		for _, event := range events {
			if event.IssueUID != nil && *event.IssueUID == got.UID {
				if event.Type == "issue.moved" {
					moved = event.ID
				}
				if event.Type == "issue.commented" {
					commented = event.ID
				}
			}
		}
		require.NotZero(t, moved)
		require.Greater(t, commented, moved)
	}
}
