package daemon_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Closed comment history must not consume the lexical candidate budget.
func TestSearchStatusFiltersCommentsBeforeCandidateLimit(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	project, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	for i := range 205 {
		issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: fmt.Sprintf("historical task %d", i), Author: "tester"})
		require.NoError(t, err)
		_, _, err = env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: strings.Repeat("runnerrefused ", 10)})
		require.NoError(t, err)
		_, _, _, err = env.DB.CloseIssue(ctx, issue.ID, "done", "tester", "", nil)
		require.NoError(t, err)
	}
	current, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "current task", Author: "tester"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(ctx, db.CreateCommentParams{IssueID: current.ID, Author: "tester", Body: "runnerrefused " + strings.Repeat("context ", 100)})
	require.NoError(t, err)
	base := projectPath(project.ID) + "/search?q=runnerrefused&mode=lexical&limit=1"
	var unfiltered struct {
		Results []api.SearchHit `json:"results"`
	}
	resp, body := envGetRaw(t, env, base)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.NoError(t, json.Unmarshal(body, &unfiltered))
	require.Len(t, unfiltered.Results, 1)
	require.Equal(t, "closed", unfiltered.Results[0].Issue.Status, "fixture must rank closed history ahead of current work")
	for _, status := range []string{"open", "closed"} {
		var got struct {
			Results []api.SearchHit `json:"results"`
		}
		resp, body := envGetRaw(t, env, base+"&status="+status)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		require.NoError(t, json.Unmarshal(body, &got))
		require.Len(t, got.Results, 1)
		assert.Equal(t, status, got.Results[0].Issue.Status)
		assert.Contains(t, got.Results[0].MatchedIn, "comments")
		if status == "open" {
			assert.Equal(t, current.UID, got.Results[0].Issue.UID)
		}
	}
}

func TestSearchStatusRejectsInvalidExplicitValues(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	for _, status := range []string{"", " ", "OPEN", "all", "pending"} {
		t.Run(fmt.Sprintf("status=%q", status), func(t *testing.T) {
			resp, body := envGetRaw(t, env, projectPath(project.ID)+"/search?q=work&status="+url.QueryEscape(status))
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		})
	}
}
