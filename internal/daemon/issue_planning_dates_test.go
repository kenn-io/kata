package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssuePlanningDatesNativeResolution(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"), func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "America/New_York" })
	project := seedProject(t, env, "example-project")
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Review", Author: "worker", Metadata: map[string]jsontext.Value{"scheduled_on": []byte(`"2026-10-06T09:00"`)}})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/planning-dates", project.ID, issue.ShortID)
	before, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodGet, path, nil)
	require.Equalf(t, 200, status, "%s", raw)
	var response map[string]any
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, issue.UID, response["issue_uid"])
	require.Equal(t, float64(issue.Revision), response["revision"])
	require.Contains(t, response, "deadline_on")
	require.Nil(t, response["deadline_on"])
	require.Equal(t, map[string]any{"field": "scheduled_on", "value": "2026-10-06T09:00", "timezone": "America/New_York", "instant": "2026-10-06T13:00:00Z"}, response["scheduled_on"])
	after, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	other := seedProject(t, env, "other-project")
	status, _ = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/issues/%s/planning-dates", other.ID, issue.UID), nil)
	require.Equal(t, 404, status)
}

func TestIssuePlanningDatesScopedReadBoundary(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project := seedProject(t, env, "example-project")
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	expiry := time.Now().Add(time.Hour)
	_, _, err := env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "planning-reader", Actor: "worker", AdminActor: db.BootstrapActor, Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID}, ExpiresAt: &expiry})
	require.NoError(t, err)
	for _, tc := range []struct {
		issue  db.Issue
		status int
	}{{child, 200}, {outside, 404}} {
		path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/planning-dates", project.ID, tc.issue.UID)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, env.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer planning-reader")
		res, err := env.HTTP.Do(req)
		require.NoError(t, err)
		raw, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.Equalf(t, tc.status, res.StatusCode, "%s", raw)
	}
}
