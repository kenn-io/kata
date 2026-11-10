package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestIssuePlanningDatesHistoricalReference(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("tok"))
	source, target, issue := seedMovePair(t, env)
	moved, err := env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{
		IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester",
	})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/planning-dates", source.ID, issue.ShortID)
	status, raw := cronHTTP(t, env, http.MethodGet, path, nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	var response struct {
		IssueUID string `json:"issue_uid"`
		Revision int64  `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, issue.UID, response.IssueUID)
	require.Equal(t, moved.NewRevision, response.Revision)
}

func TestPlanningDatesKeepWriterZoneAcrossDaemons(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "UTC" })
	project := seedProject(t, env, "example-project")
	status, raw := cronHTTP(t, env, http.MethodPost, projectPath(project.ID)+"/issues", map[string]any{"actor": "worker", "title": "portable", "force_new": true, "metadata": map[string]any{"scheduled_on": "2026-11-12T09:00", "deadline_on": "2026-11-13T09:00"}})
	require.Equal(t, 200, status, string(raw))
	var created struct {
		Issue struct {
			UID string `json:"uid"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	require.NotEmpty(t, created.Issue.UID)
	server := httptest.NewServer(daemon.NewServer(daemon.ServerConfig{DB: env.DB, DefaultTimezone: "Europe/Paris", Broadcaster: daemon.NewEventBroadcaster()}).Handler())
	t.Cleanup(server.Close)
	reader := &testenv.Env{URL: server.URL, HTTP: server.Client()}
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s/planning-dates", project.ID, created.Issue.UID)
	status, raw = cronHTTP(t, reader, http.MethodGet, path, nil)
	require.Equal(t, 200, status, string(raw))
	var response struct {
		ScheduledOn struct {
			Timezone string `json:"timezone"`
			Instant  string `json:"instant"`
		} `json:"scheduled_on"`
		DeadlineOn struct {
			Timezone string `json:"timezone"`
			Instant  string `json:"instant"`
		} `json:"deadline_on"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, "UTC", response.ScheduledOn.Timezone)
	require.Equal(t, "2026-11-12T09:00:00Z", response.ScheduledOn.Instant)
	require.Equal(t, "UTC", response.DeadlineOn.Timezone)
	require.Equal(t, "2026-11-13T09:00:00Z", response.DeadlineOn.Instant)
	editPath := fmt.Sprintf("/api/v1/projects/%d/issues/%s/metadata", project.ID, created.Issue.UID)
	status, raw = cronHTTP(t, reader, http.MethodPost, editPath, map[string]any{"actor": "worker", "patch": map[string]any{"scheduled_on": "2026-11-14T09:00", "deadline_on": "2026-11-15T09:00"}})
	require.Equal(t, 200, status, string(raw))
	status, raw = cronHTTP(t, reader, http.MethodGet, path, nil)
	require.Equal(t, 200, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, "UTC", response.ScheduledOn.Timezone)
	require.Equal(t, "2026-11-14T09:00:00Z", response.ScheduledOn.Instant)
	require.Equal(t, "UTC", response.DeadlineOn.Timezone)
	require.Equal(t, "2026-11-15T09:00:00Z", response.DeadlineOn.Instant)
}

func TestSavedInstantDatesUseSelectedZoneAcrossViews(t *testing.T) {
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "UTC" })
	project := seedProject(t, env, "example-project")
	status, raw := cronHTTP(t, env, http.MethodPost, projectPath(project.ID)+"/issues", map[string]any{"actor": "worker", "title": "New York dates", "force_new": true, "metadata": map[string]any{"timezone": "America/New_York", "scheduled_on": "2026-01-01T02:00:00Z", "deadline_on": "2026-01-01T02:00:00Z"}})
	require.Equal(t, 200, status, string(raw))
	var created struct {
		Issue struct {
			UID string `json:"uid"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", project.ID, created.Issue.UID)
	check := func(row rowDates) {
		t.Helper()
		require.Equal(t, "2025-12-31", row.ScheduledOnDate)
		require.Equal(t, "2025-12-31", row.DeadlineOnDate)
	}
	status, raw = cronHTTP(t, env, http.MethodGet, path, nil)
	require.Equal(t, 200, status, string(raw))
	var show struct {
		Issue rowDates `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(raw, &show))
	check(show.Issue)
	status, raw = cronHTTP(t, env, http.MethodGet, projectPath(project.ID)+"/issues", nil)
	require.Equal(t, 200, status, string(raw))
	var list struct {
		Issues []rowDates `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Issues, 1)
	check(list.Issues[0])
	status, raw = cronHTTP(t, env, http.MethodGet, path+"/planning-dates", nil)
	require.Equal(t, 200, status, string(raw))
	var planning struct {
		ScheduledOn, DeadlineOn struct {
			Timezone string `json:"timezone"`
			Instant  string `json:"instant"`
		}
	}
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.NoError(t, json.Unmarshal(fields["scheduled_on"], &planning.ScheduledOn))
	require.NoError(t, json.Unmarshal(fields["deadline_on"], &planning.DeadlineOn))
	require.Equal(t, "America/New_York", planning.ScheduledOn.Timezone)
	require.Equal(t, "2026-01-01T02:00:00Z", planning.ScheduledOn.Instant)
	require.Equal(t, planning.ScheduledOn, planning.DeadlineOn)
	status, raw = cronHTTP(t, env, http.MethodGet, "/api/v1/ui/snapshot?view=all-open&time_zone=Asia%2FTokyo&selected_issue_uid="+created.Issue.UID, nil)
	require.Equal(t, 200, status, string(raw))
	var snapshot struct {
		Selected struct {
			Issue rowDates `json:"issue"`
		} `json:"selected"`
		Collection []rowDates `json:"collection"`
	}
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	check(snapshot.Selected.Issue)
	require.Len(t, snapshot.Collection, 1)
	check(snapshot.Collection[0])
}
