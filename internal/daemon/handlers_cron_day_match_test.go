package daemon_test

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Keep these API regressions runnable by file, without compiling the other
// daemon test fixtures during focused iteration.
func dayMatchHTTP(t *testing.T, env *testenv.Env, method, path string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, env.URL+path, bytes.NewReader(raw))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer tok")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	raw, err = io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, raw
}

func TestCronNativeBothDayFieldsValidation(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Service review", Author: "worker"})
	require.NoError(t, err)
	definition := cron.JobDefinition{Version: 1, Kind: "job", Enabled: true,
		Trigger: cron.Trigger{Kind: "cron", Cron: "0 9 1-7 * 2", Timezone: "UTC"},
		Action:  cron.Action{Kind: "notify", Recipient: "current-owner-or-author"},
		Issue:   &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "all"}
	status, raw := dayMatchHTTP(t, env, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs", project.ID), map[string]any{"actor": "worker", "name": "First Tuesday", "definition": definition})
	require.Equalf(t, http.StatusBadRequest, status, "%s", raw)
	require.Contains(t, string(raw), "validation")
	require.Contains(t, string(raw), "trigger.day_match")
}

func TestCronDayMatchCapabilities(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	status, raw := dayMatchHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", project.ID), nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	require.Contains(t, string(raw), "cron_day_match_v1")
}
