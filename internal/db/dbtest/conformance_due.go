package dbtest

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/pagination"
)

type dueFixtureSpec struct {
	title     string
	scheduled string
	deadline  string
	timezone  string
	owner     string
	labels    []string
}

// checkListDueIssues pins the due-by filter shared by the project list, the
// global list and the browser's Today view: date-only, local date-time and UTC
// instant values; issue, daemon and UTC zone fallbacks; the legacy recurrence
// zone; day boundaries across display zones including DST; and the way limit,
// cursor and count apply to matching rows only.
func checkListDueIssues(t *testing.T, store db.Storage) error {
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "due-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "due-other-project")
	require.NoError(t, err)

	create := func(projectID int64, spec dueFixtureSpec) db.Issue {
		meta := map[string]jsontext.Value{}
		if spec.scheduled != "" {
			meta["scheduled_on"] = jsontext.Value(fmt.Sprintf("%q", spec.scheduled))
		}
		if spec.deadline != "" {
			meta["deadline_on"] = jsontext.Value(fmt.Sprintf("%q", spec.deadline))
		}
		if spec.timezone != "" {
			meta["timezone"] = jsontext.Value(fmt.Sprintf("%q", spec.timezone))
		}
		params := db.CreateIssueParams{
			ProjectID: projectID, Title: spec.title, Author: "due-author",
			Labels: spec.labels, Metadata: meta,
		}
		if spec.owner != "" {
			params.Owner = &spec.owner
		}
		issue, _, err := store.CreateIssue(ctx, params)
		require.NoError(t, err)
		return issue
	}

	create(project.ID, dueFixtureSpec{title: "date scheduled", scheduled: "2026-11-02"})
	create(project.ID, dueFixtureSpec{title: "date deadline", deadline: "2026-11-01"})
	create(project.ID, dueFixtureSpec{title: "date future", scheduled: "2026-11-03"})
	create(project.ID, dueFixtureSpec{title: "no dates"})
	create(project.ID, dueFixtureSpec{title: "local time utc", scheduled: "2026-11-02T09:00"})
	create(project.ID, dueFixtureSpec{title: "instant late", deadline: "2026-11-02T23:30:00Z"})
	create(project.ID, dueFixtureSpec{title: "issue zone", scheduled: "2026-11-03T01:00", timezone: "Pacific/Auckland"})
	create(project.ID, dueFixtureSpec{title: "daemon zone", scheduled: "2026-11-03T01:00"})
	create(project.ID, dueFixtureSpec{title: "dst fall back", deadline: "2026-11-01T23:30", timezone: "America/New_York"})
	create(project.ID, dueFixtureSpec{title: "dst spring gap", scheduled: "2026-03-08T02:30", timezone: "America/New_York"})
	closed := create(project.ID, dueFixtureSpec{title: "closed scheduled", scheduled: "2026-11-01"})
	_, _, _, err = store.CloseIssue(ctx, closed.ID, "done", "due-author", "", nil)
	require.NoError(t, err)
	create(project.ID, dueFixtureSpec{
		title: "tracked alice", scheduled: "2026-11-01", owner: "alice", labels: []string{"tracked"},
	})
	create(project.ID, dueFixtureSpec{title: "tracked bob", scheduled: "2026-11-01", owner: "bob", labels: []string{"tracked"}})
	create(other.ID, dueFixtureSpec{title: "other project", scheduled: "2026-11-01"})

	legacy := create(project.ID, dueFixtureSpec{title: "legacy recurrence zone"})
	_, err = store.CreateRecurrenceForIssue(ctx, db.CreateRecurrenceForIssueIn{
		IssueID: legacy.ID,
		Recurrence: db.CreateRecurrenceIn{
			ProjectID: project.ID, Actor: "due-author", Rule: "FREQ=DAILY;COUNT=2",
			DTStart: "2026-11-01", Timezone: "America/Los_Angeles",
			Template: db.RecurrenceTemplate{Title: "legacy recurrence zone"},
		},
	})
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: legacy.ID, Actor: "due-author",
		Patch: map[string]jsontext.Value{
			"scheduled_on": jsontext.Value(`"2026-11-02T20:00"`),
			"timezone":     jsontext.Value(`null`),
		},
	})
	require.NoError(t, err)
	// Newest issue never matches, so a limit must skip it rather than count it.
	create(project.ID, dueFixtureSpec{title: "newest unscheduled"})

	titles := func(params db.ListAllIssuesParams) []string {
		params.CreatedFirst = true
		rows, err := store.ListDueIssues(ctx, params)
		require.NoError(t, err)
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.Title)
		}
		slices.Sort(out)
		return out
	}
	scoped := func(dueBy, zone, daemonZone string) db.ListAllIssuesParams {
		return db.ListAllIssuesParams{
			ProjectID: project.ID, Status: "open",
			DueBy: dueBy, DueTimeZone: zone, DefaultTimezone: daemonZone,
		}
	}

	// Date-only, local date-time and UTC instant values in UTC.
	require.Equal(t, []string{
		"date deadline", "date scheduled", "dst fall back", "dst spring gap",
		"instant late", "issue zone", "local time utc", "tracked alice", "tracked bob",
	}, titles(scoped("2026-11-02", "UTC", "")))

	// Issue zone: 2026-11-03T01:00 in Auckland is 2026-11-02 in UTC but
	// 2026-11-03 for an Auckland reader; the issue zone wins over the daemon zone.
	require.Contains(t, titles(scoped("2026-11-02", "UTC", "Pacific/Honolulu")), "issue zone")
	require.NotContains(t, titles(scoped("2026-11-02", "Pacific/Auckland", "")), "issue zone")
	require.Contains(t, titles(scoped("2026-11-03", "Pacific/Auckland", "")), "issue zone")

	// Daemon zone and UTC fallback for a local date-time without an issue zone.
	require.Contains(t, titles(scoped("2026-11-02", "UTC", "Pacific/Auckland")), "daemon zone")
	require.NotContains(t, titles(scoped("2026-11-02", "UTC", "")), "daemon zone")
	require.Contains(t, titles(scoped("2026-11-03", "UTC", "")), "daemon zone")

	// Legacy recurrence zone is the fallback ahead of the daemon zone.
	require.Contains(t, titles(scoped("2026-11-02", "America/Los_Angeles", "")), "legacy recurrence zone")
	require.NotContains(t, titles(scoped("2026-11-02", "UTC", "")), "legacy recurrence zone")

	// Day boundary: a UTC instant late on the 2nd is the 3rd in Tokyo.
	require.Contains(t, titles(scoped("2026-11-02", "UTC", "")), "instant late")
	require.NotContains(t, titles(scoped("2026-11-02", "Asia/Tokyo", "")), "instant late")
	require.Contains(t, titles(scoped("2026-11-03", "Asia/Tokyo", "")), "instant late")

	// DST: 2026-11-01T23:30 New York (EST after the fall-back) is 04:30Z on the
	// 2nd, so it is due on the 1st only for a New York reader.
	require.Contains(t, titles(scoped("2026-11-01", "America/New_York", "")), "dst fall back")
	require.NotContains(t, titles(scoped("2026-11-01", "UTC", "")), "dst fall back")
	// A local time inside the spring-forward gap resolves to the first valid
	// instant after it (07:00Z on the 8th).
	require.NotContains(t, titles(scoped("2026-03-07", "UTC", "")), "dst spring gap")
	require.Contains(t, titles(scoped("2026-03-08", "UTC", "")), "dst spring gap")

	// Closed issues are left out under the default status and returned for any.
	require.NotContains(t, titles(scoped("2026-11-02", "UTC", "")), "closed scheduled")
	anyStatus := scoped("2026-11-02", "UTC", "")
	anyStatus.Status = ""
	require.Contains(t, titles(anyStatus), "closed scheduled")

	// Composition with owner, label and project scope.
	composed := scoped("2026-11-02", "UTC", "")
	composed.Owner, composed.Labels = "alice", []string{"tracked"}
	require.Equal(t, []string{"tracked alice"}, titles(composed))
	global := scoped("2026-11-02", "UTC", "")
	global.ProjectID = 0
	require.Contains(t, titles(global), "other project")
	require.NotContains(t, titles(scoped("2026-11-02", "UTC", "")), "other project")

	// Rows carry the dates projected into the display zone.
	rows, err := store.ListDueIssues(ctx, scoped("2026-11-03", "Pacific/Auckland", ""))
	require.NoError(t, err)
	var zoned *db.DueIssue
	for i := range rows {
		if rows[i].Title == "issue zone" {
			zoned = &rows[i]
		}
	}
	require.NotNil(t, zoned)
	require.Equal(t, "2026-11-03", zoned.ScheduledOnDate)
	require.Empty(t, zoned.DeadlineOnDate)
	rows, err = store.ListDueIssues(ctx, scoped("2026-11-02", "UTC", ""))
	require.NoError(t, err)
	seen := map[string]db.DueIssue{}
	for _, row := range rows {
		seen[row.Title] = row
	}
	require.Equal(t, "2026-11-02", seen["issue zone"].ScheduledOnDate)
	require.Equal(t, "2026-11-02", seen["instant late"].DeadlineOnDate)

	// Limit and count apply to matching rows, and a cursor continues past the
	// last returned matching row.
	all := scoped("2026-11-02", "UTC", "")
	total, err := store.CountDueIssues(ctx, all)
	require.NoError(t, err)
	require.Equal(t, int64(9), total)
	limited := all
	limited.Limit = 3
	limited.CreatedFirst = true
	page, err := store.ListDueIssues(ctx, limited)
	require.NoError(t, err)
	require.Len(t, page, 3, "the unmatched newest issue must not consume the limit")
	counted := limited
	counted.After = &pagination.Position{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
	count, err := store.CountDueIssues(ctx, counted)
	require.NoError(t, err)
	require.Equal(t, total, count, "count ignores limit and cursor")
	next := limited
	next.After = &pagination.Position{CreatedAt: page[2].CreatedAt, ID: page[2].ID}
	rest, err := store.ListDueIssues(ctx, next)
	require.NoError(t, err)
	require.Len(t, rest, 3)
	require.Less(t, rest[0].ID, page[2].ID)
	return nil
}
