package daemon_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestPostRecurrenceInitialIssueIfMatchGuardsTheIssueRevision(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Weekly review", Author: "worker"})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/recurrences", project.ID)
	body := map[string]any{
		"actor": "worker", "initial_issue_ref": issue.ShortID, "rrule": "FREQ=WEEKLY", "dtstart": "2026-10-12", "timezone": "UTC",
		"template": map[string]string{"title": "Weekly review"},
	}

	stale := fmt.Sprintf(`"rev-%d"`, issue.Revision+1)
	response, raw := envDoRaw(t, env, http.MethodPost, path, body, map[string]string{"If-Match": stale})
	assertAPIError(t, response.StatusCode, raw, http.StatusPreconditionFailed, "revision_conflict")
	recurrences, err := env.DB.ListRecurrencesByProject(t.Context(), project.ID)
	require.NoError(t, err)
	require.Empty(t, recurrences, "a stale If-Match must not create the recurrence")
	stored, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.Equal(t, issue.Revision, stored.Revision)
	assert.Nil(t, stored.RecurrenceID)

	response, raw = envDoRaw(t, env, http.MethodPost, path, body, map[string]string{"If-Match": fmt.Sprintf(`"rev-%d"`, issue.Revision)})
	require.Less(t, response.StatusCode, 300, string(raw))
	stored, err = env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	assert.NotNil(t, stored.RecurrenceID)
}

func TestPostRecurrenceIfMatchNeedsAnInitialIssue(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	response, raw := envDoRaw(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/recurrences", project.ID), map[string]any{
		"actor": "worker", "rrule": "FREQ=WEEKLY", "dtstart": "2026-10-12", "timezone": "UTC",
		"template": map[string]string{"title": "Weekly review"},
	}, map[string]string{"If-Match": `"rev-1"`})
	assertAPIError(t, response.StatusCode, raw, http.StatusBadRequest, "validation")
}
