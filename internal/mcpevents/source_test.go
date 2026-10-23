package mcpevents_test

import (
	"context"
	"testing"
	"time"

	"go.kenn.io/kata/internal/mcpevents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveProjects(t *testing.T) {
	both := []mcpevents.Project{{UID: testProjectA, ID: 1}, {UID: testProjectB, ID: 2}}
	t.Run("a dynamic selection follows the catalog, even when empty", func(t *testing.T) {
		f := &fakeAuthorized{projects: both}
		got, err := mcpevents.ResolveProjects(t.Context(), f, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, err)
		assert.Len(t, got, 2)
		f.setProjects()
		got, err = mcpevents.ResolveProjects(t.Context(), f, mcpevents.Arguments{For: "example-user"})
		require.NoError(t, err)
		assert.Empty(t, got)
	})
	t.Run("an explicit selection with nothing visible is scope removed", func(t *testing.T) {
		f := &fakeAuthorized{projects: both[:1]}
		_, err := mcpevents.ResolveProjects(t.Context(), f, mcpevents.Arguments{For: "example-user", ProjectUIDs: []string{testProjectB}})
		assert.ErrorIs(t, err, mcpevents.ErrScopeRemoved)
	})
}

func TestScanTranslatesAndSettlesNonMatches(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeAuthorized{projects: []mcpevents.Project{{UID: testProjectA, ID: 1}}, head: 9}
	f.events = []mcpevents.Occurrence{
		{EventID: 3, EventUID: "e3", Type: "issue.commented", ProjectUID: testProjectA, IssueUID: testIssue, CreatedAt: at},
		notifyOccurrence(5, testProjectA, "example-user", at),
		notifyOccurrence(6, testProjectB, "example-user", at), // another project
		notifyOccurrence(7, testProjectA, "someone-else", at),
	}
	projects, err := f.VisibleProjects(t.Context(), nil)
	require.NoError(t, err)
	result, err := mcpevents.Scan(t.Context(), f, testDaemon, mcpevents.Arguments{For: "example-user"}, projects, 0, 10)
	require.NoError(t, err)
	require.Len(t, result.Matches, 1)
	m := result.Matches[0]
	assert.Equal(t, int64(5), m.SourceEventID)
	assert.Equal(t, "notification", m.Reason)
	assert.Equal(t, testProjectA, m.ProjectUID)
	assert.Equal(t, testIssue, m.IssueUID)
	assert.Equal(t, mcpevents.InboxSourceEventID(testDaemon, m.SourceEventUID, "example-user"), m.EventKey)
	assert.Equal(t, int64(7), result.Next, "non-matching rows settle without delivery")

	empty, err := mcpevents.Scan(t.Context(), f, testDaemon, mcpevents.Arguments{For: "nobody"}, projects, 0, 10)
	require.NoError(t, err)
	assert.Empty(t, empty.Matches)
	assert.Equal(t, int64(7), empty.Next)

	none, err := mcpevents.Scan(t.Context(), f, testDaemon, mcpevents.Arguments{For: "example-user"}, nil, 4, 10)
	require.NoError(t, err)
	assert.Empty(t, none.Matches)
	assert.Equal(t, int64(4), none.Next, "an empty catalog reads nothing")
}

func TestScanReportsResets(t *testing.T) {
	f := &fakeAuthorized{projects: []mcpevents.Project{{UID: testProjectA, ID: 1}}, head: 50, resetAbove: 20}
	result, err := mcpevents.Scan(t.Context(), f, testDaemon, mcpevents.Arguments{For: "example-user"}, f.projects, 10, 10)
	require.NoError(t, err)
	assert.True(t, result.Reset)
	assert.Equal(t, int64(50), result.ResetHead)
	assert.Empty(t, result.Matches)
}

func TestScanHidesTheIssueOfAMoveIntoAnUnselectedProject(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	move := mcpevents.Occurrence{
		EventID: 4, EventUID: "e4", Type: "issue.moved", ProjectUID: testProjectB, IssueUID: testIssue, CreatedAt: at,
		Payload: []byte(`{"from_project_uid":"` + testProjectA + `","to_project_uid":"` + testProjectB + `"}`),
	}
	f := &fakeAuthorized{projects: []mcpevents.Project{{UID: testProjectA, ID: 1}}, head: 4, events: []mcpevents.Occurrence{move}}
	// The authority passes a departure-project move through; Scan hides what it
	// must not disclose.
	page := mcpevents.Page{Occurrences: []mcpevents.Occurrence{move}, Next: 4}
	result, err := mcpevents.Scan(t.Context(), pagedAuthorized{fakeAuthorized: f, page: page}, testDaemon, mcpevents.Arguments{For: "example-user"}, f.projects, 0, 10)
	require.NoError(t, err)
	require.Len(t, result.Matches, 1)
	assert.Equal(t, "visibility", result.Matches[0].Reason)
	assert.Equal(t, testProjectA, result.Matches[0].ProjectUID, "reported in the departure project")
	assert.Empty(t, result.Matches[0].IssueUID)

}

// pagedAuthorized returns a fixed page from Read.
type pagedAuthorized struct {
	*fakeAuthorized
	page mcpevents.Page
}

func (p pagedAuthorized) Read(_ context.Context, _ []mcpevents.Project, _ int64, _ int) (mcpevents.Page, error) {
	return p.page, nil
}
