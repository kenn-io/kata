package daemon_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestExternalRootOwnershipAndClaimConflictsReturn409(t *testing.T) {
	const token = "test-token"
	headers := map[string]string{"Authorization": "Bearer " + token}

	t.Run("edit externally owned issue content", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken(token))
		project, issue := createExternalRootConflictIssue(t, env, "edit-conflict")
		createExternalRootConflictBinding(t, env, project, issue, false)

		resp, raw := envDoRaw(t, env, http.MethodPatch,
			issuePathRef(project.ID, issue.ShortID, ""),
			map[string]any{"actor": "tester", "title": "Local replacement"}, headers)
		assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "external_root_content_owned")
	})

	t.Run("reopen issue during reconciliation", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken(token))
		project, issue := createExternalRootConflictIssue(t, env, "reopen-conflict")
		issue, _, _, err := env.DB.CloseIssue(
			t.Context(), issue.ID, "done", "tester", "Verified complete.", nil,
		)
		require.NoError(t, err)
		createExternalRootConflictBinding(t, env, project, issue, true)

		resp, raw := envDoRaw(t, env, http.MethodPost,
			issuePathRef(project.ID, issue.ShortID, "actions/reopen"),
			map[string]any{"actor": "tester"}, headers)
		assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "external_root_claim_active")
	})

	t.Run("archive project during reconciliation", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken(token))
		project, issue := createExternalRootConflictIssue(t, env, "archive-conflict")
		createExternalRootConflictBinding(t, env, project, issue, true)

		resp, raw := envDoRaw(t, env, http.MethodDelete,
			projectPath(project.ID)+"?actor=tester&force=true", nil, headers)
		assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "external_root_claim_active")
	})

	t.Run("merge project during reconciliation", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken(token))
		target, err := env.DB.CreateProject(t.Context(), "merge-target")
		require.NoError(t, err)
		source, issue := createExternalRootConflictIssue(t, env, "merge-source")
		createExternalRootConflictBinding(t, env, source, issue, true)

		resp, raw := envDoRaw(t, env, http.MethodPost,
			projectPath(target.ID)+"/merge",
			map[string]any{"actor": "tester", "source_project_id": source.ID}, headers)
		assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "external_root_claim_active")
	})
}

func createExternalRootConflictIssue(
	t *testing.T,
	env *testenv.Env,
	projectName string,
) (db.Project, db.Issue) {
	t.Helper()
	project, err := env.DB.CreateProject(t.Context(), projectName)
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "Example issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	return project, issue
}

func createExternalRootConflictBinding(
	t *testing.T,
	env *testenv.Env,
	project db.Project,
	issue db.Issue,
	claim bool,
) db.ExternalRootBinding {
	t.Helper()
	now := time.Now().UTC()
	binding, _, err := env.DB.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID:            project.ID,
		IssueID:              issue.ID,
		ConnectorInstance:    "notes",
		ExternalRootKey:      "root-one",
		ExternalAccountKey:   "example-account",
		Actor:                "tester",
		ReceiveCommentsAfter: now,
	})
	require.NoError(t, err)
	if claim {
		_, acquired, claimErr := env.DB.ClaimExternalRootBinding(
			t.Context(), binding.ID, "worker-claim", now, now.Add(-time.Minute),
		)
		require.NoError(t, claimErr)
		require.True(t, acquired)
	}
	return binding
}
