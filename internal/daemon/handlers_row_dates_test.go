package daemon_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// rowDatesFixture seeds issues whose planning dates land on different
// calendar dates depending on which zone resolves them. The daemon zone is
// America/New_York.
type rowDatesFixture struct {
	env                                     *testenv.Env
	project                                 db.Project
	dateOnly, issueZone, recurrence, nodate db.Issue
}

func newRowDatesFixture(t *testing.T) rowDatesFixture {
	t.Helper()
	env := testenv.New(t, func(cfg *daemon.ServerConfig) { cfg.DefaultTimezone = "America/New_York" })
	project := seedProject(t, env, "spoke-project")
	create := func(title string, meta map[string]string) db.Issue {
		values := map[string]jsontext.Value{}
		for key, value := range meta {
			values[key] = jsontext.Value(fmt.Sprintf("%q", value))
		}
		issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{
			ProjectID: project.ID, Title: title, Author: "tester", Metadata: values,
		})
		require.NoError(t, err)
		return issue
	}
	f := rowDatesFixture{env: env, project: project}
	f.dateOnly = create("date only", map[string]string{"scheduled_on": "2026-10-29", "deadline_on": "2026-11-02"})
	// 23:30Z on the 29th is already the 30th in Tokyo, still the 29th in New York.
	f.issueZone = create("issue zone", map[string]string{
		"scheduled_on": "2026-10-29T23:30:00Z", "deadline_on": "2026-10-29T23:30:00Z", "timezone": "Asia/Tokyo",
	})
	f.recurrence = create("recurrence zone", map[string]string{
		"scheduled_on": "2026-10-29T23:30:00Z", "deadline_on": "2026-10-29T23:30:00Z",
	})
	f.nodate = create("no dates", map[string]string{"topic": "x"})

	// A legacy recurrence timezone governs only the scheduled date of an
	// occurrence whose metadata names no zone; the deadline keeps the daemon zone.
	res, err := env.DB.ExecContext(t.Context(), `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"REC00000000000000000000777", project.ID, "FREQ=WEEKLY", "2026-10-29", "Pacific/Auckland",
		"tpl", "", `[]`, `{}`, "tester")
	require.NoError(t, err)
	rid, err := res.LastInsertId()
	require.NoError(t, err)
	_, err = env.DB.ExecContext(t.Context(),
		`UPDATE issues SET recurrence_id = ?, occurrence_key = '2026-10-29' WHERE id = ?`, rid, f.recurrence.ID)
	require.NoError(t, err)
	return f
}

type rowDates struct {
	Title           string `json:"title"`
	ScheduledOnDate string `json:"scheduled_on_date"`
	DeadlineOnDate  string `json:"deadline_on_date"`
}

func (f rowDatesFixture) want() map[string]rowDates {
	return map[string]rowDates{
		"date only":       {ScheduledOnDate: "2026-10-29", DeadlineOnDate: "2026-11-02"},
		"issue zone":      {ScheduledOnDate: "2026-10-30", DeadlineOnDate: "2026-10-30"},
		"recurrence zone": {ScheduledOnDate: "2026-10-30", DeadlineOnDate: "2026-10-29"},
		"no dates":        {},
	}
}

func TestListRowsCarryResolvedPlanningDates(t *testing.T) {
	f := newRowDatesFixture(t)
	for _, tc := range []struct{ name, path string }{
		{"project list", fmt.Sprintf("/api/v1/projects/%d/issues", f.project.ID)},
		{"cross-project list", "/api/v1/issues"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := cronHTTP(t, f.env, http.MethodGet, tc.path, nil)
			require.Equalf(t, http.StatusOK, status, "%s", raw)
			var body struct {
				Issues []rowDates `json:"issues"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			got := map[string]rowDates{}
			for _, row := range body.Issues {
				title := row.Title
				row.Title = ""
				got[title] = row
			}
			require.Equal(t, f.want(), got)

			var generic struct {
				Issues []map[string]any `json:"issues"`
			}
			require.NoError(t, json.Unmarshal(raw, &generic))
			for _, row := range generic.Issues {
				if row["title"] == "no dates" {
					require.NotContains(t, row, "scheduled_on_date")
					require.NotContains(t, row, "deadline_on_date")
				}
			}
		})
	}
}

func TestShowIssueCarriesResolvedPlanningDates(t *testing.T) {
	f := newRowDatesFixture(t)
	for title, issue := range map[string]db.Issue{
		"date only": f.dateOnly, "issue zone": f.issueZone, "recurrence zone": f.recurrence, "no dates": f.nodate,
	} {
		t.Run(title, func(t *testing.T) {
			status, raw := cronHTTP(t, f.env, http.MethodGet,
				fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.project.ID, issue.ShortID), nil)
			require.Equalf(t, http.StatusOK, status, "%s", raw)
			var body struct {
				Issue rowDates `json:"issue"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			want := f.want()[title]
			want.Title = title
			require.Equal(t, want, body.Issue)
			if want == (rowDates{Title: title}) {
				var generic struct {
					Issue map[string]any `json:"issue"`
				}
				require.NoError(t, json.Unmarshal(raw, &generic))
				require.NotContains(t, generic.Issue, "scheduled_on_date")
				require.NotContains(t, generic.Issue, "deadline_on_date")
			}
		})
	}
}

// One unreadable stored value must not take a whole list or show response
// down; only the field that cannot be resolved is omitted.
func TestRowsOmitUnreadablePlanningDateAndKeepTheRest(t *testing.T) {
	f := newRowDatesFixture(t)
	_, err := f.env.DB.ExecContext(t.Context(),
		`UPDATE issues SET metadata = ? WHERE id = ?`,
		`{"scheduled_on":"next tuesday","deadline_on":"2026-11-02"}`, f.nodate.ID)
	require.NoError(t, err)

	status, raw := cronHTTP(t, f.env, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%d/issues", f.project.ID), nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	var list struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal(raw, &list))
	got := map[any]map[string]any{}
	for _, row := range list.Issues {
		got[row["title"]] = row
	}
	require.Len(t, got, 4)
	require.NotContains(t, got["no dates"], "scheduled_on_date")
	require.Equal(t, "2026-11-02", got["no dates"]["deadline_on_date"])
	require.Equal(t, "2026-10-29", got["date only"]["scheduled_on_date"])

	status, raw = cronHTTP(t, f.env, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%d/issues/%s", f.project.ID, f.nodate.ShortID), nil)
	require.Equalf(t, http.StatusOK, status, "%s", raw)
	var shown struct {
		Issue map[string]any `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(raw, &shown))
	require.NotContains(t, shown.Issue, "scheduled_on_date")
	require.Equal(t, "2026-11-02", shown.Issue["deadline_on_date"])
}
