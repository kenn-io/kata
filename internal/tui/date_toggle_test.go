package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withClock pins the TUI clock for one test. The instant carries its
// own location because the Today check reads the calendar day in the
// zone time.Now reports (the local zone in production).
func withClock(t *testing.T, now time.Time) {
	t.Helper()
	prior := renderNow
	renderNow = func() time.Time { return now }
	t.Cleanup(func() { renderNow = prior })
}

func dated(shortID, scheduled, deadline string) Issue {
	return Issue{
		ShortID: shortID, Status: "open",
		ScheduledOnDate: scheduled, DeadlineOnDate: deadline,
	}
}

func shortIDs(issues []Issue) []string {
	out := make([]string, 0, len(issues))
	for _, iss := range issues {
		out = append(out, iss.ShortID)
	}
	return out
}

func TestDateToggle_TodayAndScheduledSplitOnLocalDay(t *testing.T) {
	withClock(t, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	issues := []Issue{
		dated("past", "2026-11-09", ""),
		dated("same", "2026-11-10", ""),
		dated("next", "2026-11-11", ""),
		dated("dlpast", "", "2026-11-01"),
		dated("dlnext", "", "2026-12-01"),
	}

	assert.Equal(t, []string{"past", "same", "dlpast"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateToday})))
	assert.Equal(t, []string{"next", "dlnext"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateScheduled})))
}

func TestDateToggle_ZoneBoundaryMovesTheDay(t *testing.T) {
	// 23:30 UTC on Nov 10 is already Nov 11 at UTC+10.
	instant := time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)
	issues := []Issue{dated("nov11", "2026-11-11", "")}

	withClock(t, instant)
	assert.Empty(t, filteredIssues(issues, ListFilter{Date: dateToday}))
	assert.Equal(t, []string{"nov11"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateScheduled})))

	withClock(t, instant.In(time.FixedZone("UTC+10", 10*3600)))
	assert.Equal(t, []string{"nov11"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateToday})))
	assert.Empty(t, filteredIssues(issues, ListFilter{Date: dateScheduled}))
}

func TestDateToggle_RowsMissingDatesDoNotMatch(t *testing.T) {
	withClock(t, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	closed := dated("done", "2026-11-01", "2026-11-01")
	closed.Status = "closed"
	issues := []Issue{
		dated("none", "", ""),
		dated("deadlineonly", "", "2026-11-10"),
		dated("scheduledonly", "2026-11-10", ""),
		closed,
	}

	assert.Equal(t, []string{"deadlineonly", "scheduledonly"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateToday})))
	assert.Empty(t, filteredIssues(issues, ListFilter{Date: dateScheduled}))
}

func TestDateToggle_ComposesWithOwnerLabelsAndSearch(t *testing.T) {
	withClock(t, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	alice, bob := "alice", "bob"
	a := dated("a", "2026-11-10", "")
	a.Owner, a.Labels, a.Title = &alice, []string{"bug"}, "fix login"
	b := dated("b", "2026-11-10", "")
	b.Owner, b.Labels, b.Title = &bob, []string{"bug"}, "fix login"
	c := dated("c", "2026-11-10", "")
	c.Owner, c.Labels, c.Title = &alice, []string{"feature"}, "fix login"
	d := dated("d", "2026-11-10", "")
	d.Owner, d.Labels, d.Title = &alice, []string{"bug"}, "other"
	e := dated("e", "2026-11-20", "")
	e.Owner, e.Labels, e.Title = &alice, []string{"bug"}, "fix login"
	issues := []Issue{a, b, c, d, e}

	got := filteredIssues(issues, ListFilter{
		Date: dateToday, Owner: "alice", Labels: []string{"bug"}, Search: "login",
	})
	assert.Equal(t, []string{"a"}, shortIDs(got))
}

func TestDateToggle_FilterFormCommitsAndResetsDate(t *testing.T) {
	m := openFilterForm(t, filterFormFixture())
	// Date is the last axis: status, owner, search, labels, date.
	m = pressTabN(m, 4)
	require.Equal(t, fieldDate, m.input.fields[m.input.active].id)
	assert.Equal(t, "any", m.input.fields[m.input.active].radio.value())

	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	assert.Equal(t, dateToday, m.list.filter.Date)

	// Reopening pre-positions the radio on the committed value.
	m = openFilterForm(t, m)
	assert.Equal(t, dateToday, m.input.field(fieldDate).radio.value())

	m.input = m.input.resetFilterFields()
	assert.Equal(t, "any", m.input.field(fieldDate).radio.value())
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	assert.Empty(t, m.list.filter.Date)
}

func TestDateToggle_RowsShowResolvedDates(t *testing.T) {
	withClock(t, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	rows := buildRows([]queueRow{
		{issue: dated("both", "2026-11-12", "2027-01-05")},
		{issue: dated("sched", "2026-11-12", "")},
		{issue: dated("dl", "", "2026-11-15")},
		{issue: dated("none", "", "")},
	}, 0, 60, false, viewChrome{})

	assert.Contains(t, rows[0][4], "s:11-12")
	assert.Contains(t, rows[0][4], "d:2027-01-05")
	assert.Contains(t, rows[1][4], "s:11-12")
	assert.NotContains(t, rows[1][4], "d:")
	assert.Contains(t, rows[2][4], "d:11-15")
	assert.NotContains(t, rows[2][4], "s:")
	assert.NotContains(t, rows[3][4], ":")
}

func TestDateToggle_RowDatesYieldToTheTitleOnNarrowCells(t *testing.T) {
	withClock(t, time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	iss := dated("both", "2026-11-12", "2026-11-15")
	iss.Title = "a reasonably long title"
	rows := buildRows([]queueRow{{issue: iss}}, 0, 24, false, viewChrome{})
	assert.Contains(t, rows[0][4], "a reasonably long")
	assert.NotContains(t, rows[0][4], "s:")
}

func TestDateToggle_MidnightRolloverKeepsRowActionsOnTheSelectedIssue(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	prior := renderNow
	renderNow = func() time.Time { return now }
	t.Cleanup(func() { renderNow = prior })

	m := filterFormFixture()
	m.list.issues = []Issue{
		dated("x", "2026-11-10", ""),
		dated("y", "2026-11-11", ""),
		dated("z", "2026-11-09", ""),
	}
	form := newFilterForm(ListFilter{}, m.scope)
	form.field(fieldDate).radio.set(dateToday)
	m, _ = m.commitFilterForm(form)
	m.list.cursor = 1 // z, the second Today row
	got, ok := m.list.targetRow()
	require.True(t, ok)
	require.Equal(t, "z", got.ShortID)

	// Midnight passes without a refetch: the highlighted row must stay put.
	now = now.Add(2 * time.Minute)
	got, ok = m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, "z", got.ShortID)

	// The next fetch moves the day on and the selection follows the issue.
	m.list.selectedUID = "uid-z"
	m.list.issues[2].UID = "uid-z"
	m.list = m.list.applyFetched(refetchedMsg{issues: m.list.issues})
	got, ok = m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, "z", got.ShortID)
	assert.Equal(t, []string{"x", "y", "z"}, shortIDs(rowIssues(m.list.visibleRows())))
}

func rowIssues(rows []queueRow) []Issue {
	out := make([]Issue, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.issue)
	}
	return out
}
