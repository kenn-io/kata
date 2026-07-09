package tui

import (
	"net/http"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmoke_ProjectsViewLoop covers the end-to-end happy path of
// spec §8.7: an unbound cwd lands on viewProjects, the table is
// ordered by last_event_at desc, Enter on the sentinel transitions to
// viewList in all-projects scope, and P from viewList returns to
// viewProjects.
func TestSmoke_ProjectsViewLoop(t *testing.T) {
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/projects/resolve": projectNotInitializedHandler,
		"/api/v1/projects": func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "stats", r.URL.Query().Get("include"))
			_, _ = w.Write([]byte(`{"projects":[
                {"id":1,"identity":"github.com/wesm/proj-a","name":"proj-a",
                 "stats":{"open":2,"closed":1,"last_event_at":"2026-05-04T10:00:00.000Z"}},
                {"id":2,"identity":"github.com/wesm/proj-b","name":"proj-b",
                 "stats":{"open":5,"closed":2,"last_event_at":"2026-05-04T12:00:00.000Z"}},
                {"id":3,"identity":"github.com/wesm/proj-c","name":"proj-c",
                 "stats":{"open":10,"closed":3,"last_event_at":"2026-05-04T11:00:00.000Z"}}
            ]}`))
		},
		// Cross-project list — the all-projects drill-in fetches from here.
		"/api/v1/issues": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"issues":[]}`))
		},
	})
	c := NewClient(srv.URL, srv.Client())

	// 1. Boot resolves to viewProjects.
	bi, err := bootResolveScope(t.Context(), c, "/tmp/unbound")
	require.NoError(t, err)
	require.Equal(t, viewProjects, bi.view)

	// 2. Build the model from boot — same wiring Run uses in production.
	m := buildRunModel(Options{}, c, bi)

	// 3. Rows ordered by last_event_at desc: proj-b (12:00), proj-c (11:00), proj-a (10:00).
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	require.Len(t, rows, 4)
	assert.True(t, rows[0].sentinel)
	assert.Equal(t, "proj-b", rows[1].name)
	assert.Equal(t, "proj-c", rows[2].name)
	assert.Equal(t, "proj-a", rows[3].name)

	// 4. Enter on the sentinel transitions to viewList in all-projects scope.
	m.projectsCursor = 0
	nextModel, cmd := m.routeProjectsViewKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Equal(t, viewList, nextModel.view)
	assert.True(t, nextModel.scope.allProjects)
	require.NotNil(t, cmd, "drill-in must dispatch fetchInitial")

	// 5. P from the resulting viewList returns to viewProjects with scope preserved.
	nm2, cmd2 := updateModel(nextModel, tea.KeyPressMsg{Code: 'P', Text: "P"})
	assert.Equal(t, viewProjects, nm2.view)
	assert.True(t, nm2.scope.allProjects, "scope preserved on P-back")
	require.NotNil(t, cmd2, "P must dispatch fetchProjectsWithStats")
}
