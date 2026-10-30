package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

type dueRow struct {
	Title           string `json:"title"`
	ScheduledOnDate string `json:"scheduled_on_date"`
	DeadlineOnDate  string `json:"deadline_on_date"`
}

type duePage struct {
	Issues     []dueRow `json:"issues"`
	NextCursor string   `json:"next_cursor"`
	Complete   bool     `json:"complete"`
	Total      *int64   `json:"total"`
}

func (p duePage) titles() []string {
	var out []string
	for _, row := range p.Issues {
		out = append(out, row.Title)
	}
	slices.Sort(out)
	return out
}

func getDuePage(t *testing.T, env *testenv.Env, path string) duePage {
	t.Helper()
	resp, body := envGetRaw(t, env, path)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var page duePage
	require.NoError(t, json.Unmarshal(body, &page))
	return page
}

func createDueIssue(t *testing.T, env *testenv.Env, projectID int64, title string, meta map[string]string, owner string, labels ...string) db.Issue {
	t.Helper()
	values := map[string]jsontext.Value{}
	for key, value := range meta {
		values[key] = jsontext.Value(fmt.Sprintf("%q", value))
	}
	params := db.CreateIssueParams{
		ProjectID: projectID, Title: title, Author: "example-agent", Labels: labels, Metadata: values,
	}
	if owner != "" {
		params.Owner = &owner
	}
	issue, _, err := env.DB.CreateIssue(t.Context(), params)
	require.NoError(t, err)
	return issue
}

// Both list routes share the Today rule and report the planning dates they
// matched on in the requested zone.
func TestListDueByFiltersByTodayRuleOnBothRoutes(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	createDueIssue(t, env, project.ID, "date scheduled", map[string]string{"scheduled_on": "2026-11-02"}, "")
	createDueIssue(t, env, project.ID, "date deadline", map[string]string{"deadline_on": "2026-11-01"}, "")
	createDueIssue(t, env, project.ID, "future", map[string]string{"scheduled_on": "2026-11-03"}, "")
	createDueIssue(t, env, project.ID, "undated", nil, "")
	createDueIssue(t, env, project.ID, "local time", map[string]string{"scheduled_on": "2026-11-02T09:00"}, "")
	createDueIssue(t, env, project.ID, "rfc3339", map[string]string{"deadline_on": "2026-11-02T23:30:00Z"}, "")
	closed := createDueIssue(t, env, project.ID, "closed", map[string]string{"scheduled_on": "2026-11-01"}, "")
	_, _, _, err = env.DB.CloseIssue(t.Context(), closed.ID, "done", "example-agent", "", nil)
	require.NoError(t, err)

	for name, base := range map[string]string{
		"project": fmt.Sprintf("/api/v1/projects/%d/issues", project.ID),
		"global":  "/api/v1/issues",
	} {
		t.Run(name, func(t *testing.T) {
			page := getDuePage(t, env, base+"?due_by=2026-11-02&status=open")
			assert.Equal(t, []string{"date deadline", "date scheduled", "local time", "rfc3339"}, page.titles())
			byTitle := map[string]dueRow{}
			for _, row := range page.Issues {
				byTitle[row.Title] = row
			}
			assert.Equal(t, "2026-11-02", byTitle["date scheduled"].ScheduledOnDate)
			assert.Equal(t, "2026-11-01", byTitle["date deadline"].DeadlineOnDate)
			assert.Equal(t, "2026-11-02", byTitle["rfc3339"].DeadlineOnDate)

			// An explicit zone moves the UTC instant across the day boundary.
			tokyo := getDuePage(t, env, base+"?due_by=2026-11-02&status=open&time_zone=Asia/Tokyo")
			assert.NotContains(t, tokyo.titles(), "rfc3339")
			assert.Equal(t, "2026-11-03", func() string {
				page := getDuePage(t, env, base+"?due_by=2026-11-03&status=open&time_zone=Asia/Tokyo")
				for _, row := range page.Issues {
					if row.Title == "rfc3339" {
						return row.DeadlineOnDate
					}
				}
				return ""
			}())

			// Closed issues stay out under status=open and return without a status.
			assert.NotContains(t, page.titles(), "closed")
			anyStatus := getDuePage(t, env, base+"?due_by=2026-11-02")
			assert.Contains(t, anyStatus.titles(), "closed")

			// Without due_by rows carry no calendar dates and nothing is filtered.
			plain := getDuePage(t, env, base+"?status=open")
			assert.Contains(t, plain.titles(), "undated")
			for _, row := range plain.Issues {
				assert.Empty(t, row.ScheduledOnDate)
				assert.Empty(t, row.DeadlineOnDate)
			}
		})
	}
}

// With no time_zone the daemon's configured zone is the reader zone, then UTC.
func TestListDueByZoneFallsBackToDaemonZoneThenUTC(t *testing.T) {
	for _, tt := range []struct {
		name       string
		daemonZone string
		query      string
		want       []string
	}{
		{name: "utc fallback", daemonZone: "", query: "due_by=2026-11-02", want: []string{"instant"}},
		{name: "daemon zone", daemonZone: "Asia/Tokyo", query: "due_by=2026-11-02", want: nil},
		{name: "daemon zone next day", daemonZone: "Asia/Tokyo", query: "due_by=2026-11-03", want: []string{"instant"}},
		{name: "request zone overrides daemon zone", daemonZone: "Asia/Tokyo", query: "due_by=2026-11-02&time_zone=UTC", want: []string{"instant"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var opts []testenv.Option
			if tt.daemonZone != "" {
				opts = append(opts, testenv.WithDefaultTimezone(tt.daemonZone))
			}
			env := testenv.New(t, opts...)
			project, err := env.DB.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			createDueIssue(t, env, project.ID, "instant", map[string]string{"deadline_on": "2026-11-02T23:30:00Z"}, "")
			page := getDuePage(t, env, fmt.Sprintf("/api/v1/projects/%d/issues?%s", project.ID, tt.query))
			assert.Equal(t, tt.want, page.titles())
		})
	}
}

// An issue's own zone resolves its civil value ahead of the daemon zone.
func TestListDueByIssueZoneBeatsDaemonZone(t *testing.T) {
	env := testenv.New(t, testenv.WithDefaultTimezone("Pacific/Honolulu"))
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	createDueIssue(t, env, project.ID, "issue zone", map[string]string{
		"scheduled_on": "2026-11-03T01:00", "timezone": "Pacific/Auckland",
	}, "")
	path := fmt.Sprintf("/api/v1/projects/%d/issues?due_by=2026-11-02&time_zone=UTC", project.ID)
	assert.Equal(t, []string{"issue zone"}, getDuePage(t, env, path).titles())
}

func TestListDueByComposesWithLabelsOwnerAndOtherFilters(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	due := map[string]string{"scheduled_on": "2026-11-01"}
	createDueIssue(t, env, project.ID, "wanted", due, "alice", "tracked")
	createDueIssue(t, env, project.ID, "wrong owner", due, "bob", "tracked")
	createDueIssue(t, env, project.ID, "wrong label", due, "alice", "other")
	createDueIssue(t, env, project.ID, "not due", map[string]string{"scheduled_on": "2026-12-01"}, "alice", "tracked")
	for _, base := range []string{fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), "/api/v1/issues"} {
		page := getDuePage(t, env, base+"?due_by=2026-11-02&owner=alice&label=tracked")
		assert.Equal(t, []string{"wanted"}, page.titles(), base)
	}
}

func TestListDueByValidation(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	for _, base := range []string{fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), "/api/v1/issues"} {
		for _, query := range []string{
			"due_by=tomorrow",
			"due_by=2026-13-01",
			"due_by=2026-11-02T09:00",
			"due_by=2026-11-02&time_zone=Mars/Base",
			"time_zone=UTC",
		} {
			resp, body := envGetRaw(t, env, base+"?"+query)
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		}
	}
}

// due_by and the effective zone are part of the cursor identity.
func TestListDueByCursorPagingAndChangedDueBy(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	for i := range 4 {
		createDueIssue(t, env, project.ID, fmt.Sprintf("due %d", i), map[string]string{"scheduled_on": "2026-11-01"}, "")
		createDueIssue(t, env, project.ID, fmt.Sprintf("later %d", i), map[string]string{"scheduled_on": "2026-12-01"}, "")
	}
	for _, base := range []string{fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), "/api/v1/issues"} {
		query := "?due_by=2026-11-02&sort=oldest&limit=2&include_total=true"
		first := getDuePage(t, env, base+query)
		require.Equal(t, []string{"due 0", "due 1"}, first.titles(), base)
		require.NotEmpty(t, first.NextCursor)
		require.NotNil(t, first.Total)
		assert.Equal(t, int64(4), *first.Total, "total counts matches, not the limit or the later issues")
		second := getDuePage(t, env, base+query+"&cursor="+url.QueryEscape(first.NextCursor))
		require.Equal(t, []string{"due 2", "due 3"}, second.titles(), base)
		assert.True(t, second.Complete)

		for _, changed := range []string{
			"?due_by=2026-11-03&sort=oldest&limit=2",
			"?sort=oldest&limit=2",
			"?due_by=2026-11-02&time_zone=Asia/Tokyo&sort=oldest&limit=2",
		} {
			resp, body := envGetRaw(t, env, base+changed+"&cursor="+url.QueryEscape(first.NextCursor))
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, "validation")
		}
	}
}

// A limit applies to matching rows: unmatched newer rows never use it up.
func TestListDueByLimitAndTotalCountMatchingRows(t *testing.T) {
	env := testenv.New(t)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	createDueIssue(t, env, project.ID, "due", map[string]string{"scheduled_on": "2026-11-01"}, "")
	for i := range 3 {
		createDueIssue(t, env, project.ID, fmt.Sprintf("newer %d", i), nil, "")
	}
	for _, base := range []string{fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), "/api/v1/issues"} {
		page := getDuePage(t, env, base+"?due_by=2026-11-02&limit=1&include_total=true")
		assert.Equal(t, []string{"due"}, page.titles(), base)
		assert.True(t, page.Complete, base)
		require.NotNil(t, page.Total)
		assert.Equal(t, int64(1), *page.Total, base)
	}
}
