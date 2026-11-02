package tui

import (
	"testing"
	"testing/synctest"
	"time"
	_ "time/tzdata"

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
	iss := testIssue(shortID)
	iss.Status = "open"
	iss.ScheduledOnDate, iss.DeadlineOnDate = scheduled, deadline
	return iss
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
		shortIDs(filteredIssues(issues, ListFilter{Date: dateToday}.stampDay())))
	assert.Equal(t, []string{"next", "dlnext"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateScheduled}.stampDay())))
}

func TestDateToggle_ZoneBoundaryMovesTheDay(t *testing.T) {
	// 23:30 UTC on Nov 10 is already Nov 11 at UTC+10.
	instant := time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)
	issues := []Issue{dated("nov11", "2026-11-11", "")}

	withClock(t, instant)
	assert.Empty(t, filteredIssues(issues, ListFilter{Date: dateToday}.stampDay()))
	assert.Equal(t, []string{"nov11"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateScheduled}.stampDay())))

	withClock(t, instant.In(time.FixedZone("UTC+10", 10*3600)))
	assert.Equal(t, []string{"nov11"},
		shortIDs(filteredIssues(issues, ListFilter{Date: dateToday}.stampDay())))
	assert.Empty(t, filteredIssues(issues, ListFilter{Date: dateScheduled}.stampDay()))
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
		shortIDs(filteredIssues(issues, ListFilter{Date: dateToday}.stampDay())))
	assert.Empty(t, filteredIssues(issues, ListFilter{Date: dateScheduled}.stampDay()))
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
	}.stampDay())
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
	m.list.selectedUID = m.list.issues[2].UID
	m.list = m.list.applyFetched(refetchedMsg{issues: m.list.issues})
	got, ok = m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, "z", got.ShortID)
	assertQueueShortIDs(t, m.list.visibleRows(), []string{"x", "y", "z"})
}

func TestDateToggle_CancelingFormAfterMidnightRefreshKeepsDayAndSelection(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	prior := renderNow
	renderNow = func() time.Time { return now }
	t.Cleanup(func() { renderNow = prior })

	m, cleanup := splitTestSetup(t)
	defer cleanup()
	issues := []Issue{dated("x", "2026-11-10", ""), dated("y", "2026-11-11", ""), dated("z", "2026-11-09", "")}
	m.list.issues = issues
	form := newFilterForm(ListFilter{}, m.scope)
	form.field(fieldDate).radio.set(dateToday)
	m, _ = m.commitFilterForm(form)
	m.list = m.list.applyFetched(refetchedMsg{issues: issues})
	m.list.cursor, m.list.selectedUID, m.list.selectedProjectID = 1, "01TEST-z", 7
	zIssue := issues[2]
	m.detail.issue, m.detail.scopePID = &zIssue, 7

	// Open the form before midnight, refresh after it, then cancel.
	m.input = newFilterForm(m.list.filter, m.scope)
	now = now.Add(2 * time.Minute)
	m, _ = stepModel(m, refetchedMsg{dispatchKey: m.currentCacheKey(), issues: issues})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	assert.Equal(t, "2026-11-11", m.list.filter.Day)
	assert.Equal(t, "01TEST-z", m.list.selectedUID)
	got, ok := m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, "z", got.ShortID)
	require.NotNil(t, m.detail.issue)
	assert.Equal(t, "01TEST-z", m.detail.issue.UID)
}

// An idle TUI must advance relative date filters at local midnight without
// relying on a daemon event or a key press.
func TestDateToggle_IdleMidnightRefetchKeepsSelection(t *testing.T) {
	santiago, err := time.LoadLocation("America/Santiago")
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		start  time.Time
		jump   time.Duration
		day    string
		checks int
	}{
		{"midnight", time.Date(2026, 11, 10, 23, 59, 0, 0, time.FixedZone("local", -5*3600)), 0, "2026-11-11", 2},
		{"suspend", time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC), 15 * time.Hour, "2026-11-11", 2},
		{"DST gap", time.Date(2027, 9, 4, 23, 30, 0, 0, santiago), 0, "2027-09-05", 31},
	} {
		for _, mode := range []string{dateToday, dateScheduled, ""} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					base := time.Now()
					wallJump := time.Duration(0)
					prior := renderNow
					renderNow = func() time.Time { return tc.start.Add(time.Since(base) + wallJump) }
					t.Cleanup(func() { renderNow = prior })
					priorColor := activeColorMode
					activeColorMode = colorNone
					t.Cleanup(func() { activeColorMode = priorColor })

					m := filterFormFixture()
					issues := []Issue{
						dated("new", tc.day, ""),
						dated("keep", "2026-11-09", "2027-11-20"),
					}
					m.list.filter = ListFilter{Date: mode}.stampDay()
					m.list = m.list.applyFetched(initialFetchMsg{issues: issues})
					m.list.cursor = len(m.list.visibleRows()) - 1
					// Filter commits clear the recorded identity until navigation.
					m.list.selectedUID = ""
					m.cache.put(m.currentCacheKey(), issues)
					tick := m.Init()
					require.NotNil(t, tick)
					// A pending SSE reader must stay the sole reader. No API is
					// available: changing the day uses only the cached rows.
					m.sseCh = make(chan tea.Msg)
					wallJump = tc.jump
					for range tc.checks {
						before := time.Now()
						msg := tick()
						require.Greater(t, time.Since(before), time.Duration(0), "each wakeup must be in the future")
						require.LessOrEqual(t, time.Since(before), time.Minute, "resume must update within a minute")
						var handled bool
						var next tea.Model
						next, tick, handled = m.routeSSE(msg)
						require.True(t, handled)
						m = next.(Model)
						require.NotNil(t, tick)
						if mode != "" {
							assert.Equal(t, renderNow().Format(time.DateOnly), m.list.filter.Day)
						}
					}
					assert.Equal(t, tc.day, renderNow().Format(time.DateOnly))
					if mode == dateScheduled {
						assertQueueShortIDs(t, m.list.visibleRows(), []string{"keep"})
					} else {
						assertQueueShortIDs(t, m.list.visibleRows(), []string{"new", "keep"})
					}
					selected, ok := m.list.targetRow()
					require.True(t, ok)
					assert.Equal(t, "keep", selected.ShortID)
				})
			})
		}
	}
}

func TestDateToggle_ClosedStatusControls(t *testing.T) {
	for _, mode := range []string{dateToday, dateScheduled} {
		t.Run(mode, func(t *testing.T) {
			m := openFilterForm(t, filterFormFixture())
			m.input.field(fieldStatus).radio.set("closed")
			m.input.field(fieldDate).radio.set(mode)
			m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			require.Equal(t, inputFilterForm, m.input.kind)
			assert.Equal(t, "Today and Scheduled show open issues only", m.input.err)
			m.input.field(fieldStatus).radio.set("open")
			m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
			require.Equal(t, inputNone, m.input.kind)
			assert.Equal(t, mode, m.list.filter.Date)
			m, _ = stepModel(m, tea.KeyPressMsg{Code: 's'})
			assert.Equal(t, "open", m.list.filter.Status)
			assert.Equal(t, "Today and Scheduled show open issues only", m.list.status)
		})
	}
}
