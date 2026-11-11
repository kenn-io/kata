package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/testenv"
)

// An already open browser must still read the snapshot after its daemon upgrades.
func TestUISnapshotSavedDatesRemainReadableAfterUpgrade(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	scheduled, err := metadata.NewScheduleValue("2026-01-01T02:00:00.125Z", "America/New_York")
	require.NoError(t, err)
	deadline, err := metadata.NewScheduleValue("2026-01-02", "Asia/Tokyo")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Scheduled", Author: "user-a",
		Metadata: map[string]jsontext.Value{"scheduled_on": scheduled, "deadline_on": deadline},
	})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/ui/snapshot?view=all-open&selected_issue_uid=%s&include_graph=true&time_zone=UTC", issue.UID), nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	type snapshotIssue struct {
		Metadata           map[string]jsontext.Value `json:"metadata"`
		ScheduledOnDate    string                    `json:"scheduled_on_date"`
		ScheduledOnDisplay string                    `json:"scheduled_on_display"`
		DeadlineOnDate     string                    `json:"deadline_on_date"`
	}
	var snapshot struct {
		Collection []snapshotIssue `json:"collection"`
		Selected   struct {
			Issue snapshotIssue `json:"issue"`
		} `json:"selected"`
		Graph struct {
			Issues []snapshotIssue `json:"issues"`
		} `json:"graph"`
	}
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	require.Len(t, snapshot.Collection, 1)
	require.Len(t, snapshot.Graph.Issues, 1)
	require.Equal(t, "2025-12-31", snapshot.Selected.Issue.ScheduledOnDate)
	require.Equal(t, "2025-12-31T21:00:00.125", snapshot.Selected.Issue.ScheduledOnDisplay)
	require.Equal(t, "2026-01-02", snapshot.Selected.Issue.DeadlineOnDate)
	for name, row := range map[string]snapshotIssue{"collection": snapshot.Collection[0], "selected": snapshot.Selected.Issue, "graph": snapshot.Graph.Issues[0]} {
		t.Run(name, func(t *testing.T) {
			assert.JSONEq(t, `"2025-12-31T21:00:00.125"`, string(row.Metadata["scheduled_on"]))
			assert.JSONEq(t, `"2026-01-02"`, string(row.Metadata["deadline_on"]))
		})
	}
	status, raw = cronHTTP(t, env, http.MethodGet, fmt.Sprintf("/api/v1/ui/snapshot?view=all-open&selected_issue_uid=%s&include_graph=true&time_zone=UTC", issue.UID), nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var cached struct {
		Selected struct {
			Issue snapshotIssue `json:"issue"`
		} `json:"selected"`
	}
	require.NoError(t, json.Unmarshal(raw, &cached))
	require.JSONEq(t, `"2025-12-31T21:00:00.125"`, string(cached.Selected.Issue.Metadata["scheduled_on"]))
	stored, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	var fields map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(stored.Metadata), &fields))
	require.JSONEq(t, string(scheduled), string(fields["scheduled_on"]))
	require.JSONEq(t, string(deadline), string(fields["deadline_on"]))
	status, raw = cronHTTP(t, env, http.MethodPost, fmt.Sprintf("%s/issues/%s/metadata", projectPath(project.ID), issue.ShortID), map[string]any{
		"actor": "user-a", "patch": map[string]any{"scheduled_on": "2025-12-31T21:00:00.125"},
	})
	require.Equal(t, http.StatusOK, status, string(raw))
	stored, err = env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	_, zone, instant, present, err := metadata.ScheduleFieldInstant(string(stored.Metadata), "scheduled_on", "UTC")
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "America/New_York", zone)
	require.Equal(t, "2026-01-01T02:00:00.125Z", instant.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	status, raw = cronHTTP(t, env, http.MethodPost, fmt.Sprintf("%s/issues/%s/metadata", projectPath(project.ID), issue.ShortID), map[string]any{
		"actor": "user-a", "patch": map[string]any{"scheduled_on": "2026-01-02T09:30"},
	})
	require.Equal(t, http.StatusOK, status, string(raw))
	stored, err = env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(stored.Metadata), &fields))
	saved, err := metadata.DecodeSavedSchedule(fields["scheduled_on"])
	require.NoError(t, err)
	require.Equal(t, "America/New_York", saved.Timezone)
	require.Equal(t, metadata.ScheduleTZDataVersion, saved.TZDataVersion)
	require.Equal(t, "2026-01-02T09:30", saved.Value)
}

func TestUISnapshotUpgradeKeepsRepeatedInstantOnEcho(t *testing.T) {
	env := testenv.New(t)
	project := seedProject(t, env, "example-project")
	scheduled, err := metadata.NewScheduleValue("2026-11-01T06:30:00Z", "America/New_York")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Repeated hour", Author: "user-a",
		Metadata: map[string]jsontext.Value{"scheduled_on": scheduled},
	})
	require.NoError(t, err)
	status, raw := cronHTTP(t, env, http.MethodPost, fmt.Sprintf("%s/issues/%s/metadata", projectPath(project.ID), issue.ShortID), map[string]any{
		"actor": "user-a", "patch": map[string]any{"scheduled_on": "2026-11-01T01:30:00"},
	})
	require.Equal(t, http.StatusOK, status, string(raw))
	stored, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	_, zone, instant, present, err := metadata.ScheduleFieldInstant(string(stored.Metadata), "scheduled_on", "UTC")
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, "America/New_York", zone)
	require.Equal(t, "2026-11-01T06:30:00Z", instant.UTC().Format("2006-01-02T15:04:05Z"))
}
