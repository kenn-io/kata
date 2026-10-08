package linearsync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testIssue() Issue {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return Issue{ID: issueID, TeamID: teamID, StateID: stateID, Identifier: "EX-1", Title: "Example task", Description: "Markdown **body**", URL: "https://linear.app/example-workspace/issue/EX-1/example-task", CreatedAt: at, UpdatedAt: at.Add(time.Hour), Priority: 2}
}
func TestMappingOwnershipAndWorkflowTypes(t *testing.T) {
	c := testConfig()
	scope := Scope{WorkspaceID: workspaceID, TeamID: teamID, Name: "Example team"}
	for _, typ := range []string{"triage", "backlog", "unstarted", "started", "completed", "canceled", "duplicate"} {
		batch, err := BuildImportBatch(c.SourceKey(), c, scope, []State{{ID: stateID, Type: typ}}, []Issue{testIssue()})
		require.NoError(t, err)
		require.Len(t, batch.Items, 1)
		i := batch.Items[0]
		require.Equal(t, "issue:"+issueID, i.ExternalID)
		require.Equal(t, "[Linear EX-1] Example task", i.Title)
		require.Contains(t, i.Body, "Markdown **body**")
		require.Equal(t, new(int64(1)), i.Priority)
		closed := typ == "completed" || typ == "canceled" || typ == "duplicate"
		require.Equal(t, closed, i.Status == "closed")
		if closed {
			require.NotNil(t, i.ClosedAt)
			reason := "wontfix"
			if typ == "completed" {
				reason = "done"
			}
			require.Equal(t, reason, *i.ClosedReason)
		}
	}
	c.TitlePrefix = new(false)
	i := testIssue()
	i.Priority = 0
	batch, err := BuildImportBatch(c.SourceKey(), c, scope, []State{{ID: stateID, Type: "started"}}, []Issue{i})
	require.NoError(t, err)
	require.Equal(t, "Example task", batch.Items[0].Title)
	require.Equal(t, []string{"linear"}, batch.Items[0].Labels)
	require.Nil(t, batch.Items[0].Priority)
}
func TestMappingRejectsWrongScopeAndInvalidObservations(t *testing.T) {
	c := testConfig()
	scope := Scope{WorkspaceID: workspaceID, TeamID: teamID}
	for _, edit := range []func(*Issue){func(i *Issue) { i.TeamID = projectID }, func(i *Issue) { i.StateID = projectID }, func(i *Issue) { i.Priority = 5 }, func(i *Issue) { i.URL = "https://foreign.example/issue" }, func(i *Issue) { i.CreatedAt = time.Time{} }, func(i *Issue) { i.Title = "bad\x00title" }} {
		i := testIssue()
		edit(&i)
		_, err := BuildImportBatch(c.SourceKey(), c, scope, []State{{ID: stateID, Type: "started"}}, []Issue{i})
		require.Error(t, err)
	}
	_, err := BuildImportBatch(c.SourceKey(), c, scope, []State{{ID: stateID, Type: "started"}}, []Issue{testIssue(), testIssue()})
	require.Error(t, err)
	c.ProjectID = projectID
	_, err = BuildImportBatch(c.SourceKey(), c, scope, []State{{ID: stateID, Type: "started"}}, []Issue{testIssue()})
	require.Error(t, err)
}

func TestMappingOmitsTrashedIssue(t *testing.T) {
	c := testConfig()
	scope := Scope{WorkspaceID: workspaceID, TeamID: teamID}
	i := testIssue()
	i.Trashed = true
	batch, err := BuildImportBatch(c.SourceKey(), c, scope, []State{{ID: stateID, Type: "started"}}, []Issue{i})
	require.NoError(t, err)
	require.Empty(t, batch.Items)
}
