package tui

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"
	_ "time/tzdata"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"
)

// withClock pins the TUI clock for one test. The instant carries its
// own location because the Today check reads the calendar day in the
// zone time.Now reports (the local zone in production).
func withClock(t *testing.T, now func() time.Time) {
	t.Helper()
	prior := renderNow
	renderNow = now
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
	instant := time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC)
	closed := dated("done", "2026-11-01", "2026-11-01")
	closed.Status = "closed"
	for _, tc := range []struct {
		name             string
		now              time.Time
		issues           []Issue
		today, scheduled []string
	}{
		{
			name: "scheduled and deadline dates", now: instant,
			issues: []Issue{
				dated("past", "2026-11-09", ""),
				dated("same", "2026-11-10", ""),
				dated("next", "2026-11-11", ""),
				dated("dlpast", "", "2026-11-01"),
				dated("dlnext", "", "2026-12-01"),
			},
			today: []string{"past", "same", "dlpast"}, scheduled: []string{"next", "dlnext"},
		},
		{
			name: "UTC before midnight", now: instant,
			issues: []Issue{dated("nov11", "2026-11-11", "")},
			today:  []string{}, scheduled: []string{"nov11"},
		},
		{
			name: "local zone after midnight", now: instant.In(time.FixedZone("UTC+10", 10*3600)),
			issues: []Issue{dated("nov11", "2026-11-11", "")},
			today:  []string{"nov11"}, scheduled: []string{},
		},
		{
			name: "missing dates", now: instant,
			issues: []Issue{dated("none", "", "")},
			today:  []string{}, scheduled: []string{},
		},
		{
			name: "closed row", now: instant,
			issues: []Issue{closed},
			today:  []string{}, scheduled: []string{},
		},
		{
			name: "single date", now: instant,
			issues: []Issue{dated("deadlineonly", "", "2026-11-10"), dated("scheduledonly", "2026-11-10", "")},
			today:  []string{"deadlineonly", "scheduledonly"}, scheduled: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withClock(t, func() time.Time { return tc.now })
			assert.Equal(t, tc.today, shortIDs(filteredIssues(tc.issues, ListFilter{Date: dateToday}.stampDay())))
			assert.Equal(t, tc.scheduled, shortIDs(filteredIssues(tc.issues, ListFilter{Date: dateScheduled}.stampDay())))
		})
	}
}

func TestDateToggle_ComposesWithOwnerLabelsAndSearch(t *testing.T) {
	withClock(t, func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) })
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

func TestDateToggle_FilterFormCommitsAndClearsDate(t *testing.T) {
	m := openFilterForm(t, filterFormFixture())
	m.input.active = len(m.input.fields) - 1

	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyRight})
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	assert.Equal(t, dateToday, m.list.filter.Date)

	// Reopening pre-positions the radio on the committed value.
	m = openFilterForm(t, m)
	assert.Equal(t, dateToday, m.input.field(fieldDate).radio.value())

	m.input.field(fieldDate).radio.set("any")
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	assert.Empty(t, m.list.filter.Date)
}

func TestDateToggle_RowsShowResolvedDates(t *testing.T) {
	withClock(t, func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) })
	t.Run("wide cells", func(t *testing.T) {
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
	})
	t.Run("narrow cells", func(t *testing.T) {
		iss := dated("both", "2026-11-12", "2026-11-15")
		iss.Title = "a reasonably long title"
		rows := buildRows([]queueRow{{issue: iss}}, 0, 24, false, viewChrome{})
		assert.Contains(t, rows[0][4], "a reasonably long")
		assert.NotContains(t, rows[0][4], "s:")
	})
}

func TestDateToggle_MidnightRolloverKeepsRowActionsOnTheSelectedIssue(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	withClock(t, func() time.Time { return now })

	m := filterFormFixture()
	m.list.issues = []Issue{
		dated("x", "2026-11-10", ""),
		dated("y", "2026-11-11", ""),
		dated("z", "2026-11-09", ""),
	}
	form := newFilterForm(ListFilter{}, m.scope)
	form.field(fieldDate).radio.set(dateToday)
	m, _ = m.commitFilterForm(form)
	m, _ = stepModel(m, runeKey('j')) // z, the second Today row
	got, ok := m.list.targetRow()
	require.True(t, ok)
	require.Equal(t, "z", got.ShortID)

	// Midnight passes without a refetch: the highlighted row must stay put.
	now = now.Add(2 * time.Minute)
	got, ok = m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, "z", got.ShortID)
}

// An idle TUI must advance relative date filters at local midnight without
// relying on a daemon event or a key press.
func TestDateToggle_IdleDayTickKeepsSelection(t *testing.T) {
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
					withClock(t, func() time.Time { return tc.start.Add(time.Since(base) + wallJump) })
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
					m, _ = stepModel(m, runeKey('G'))
					tick := dayTick()
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
	m := openFilterForm(t, filterFormFixture())
	m.input.field(fieldStatus).radio.set("closed")
	m.input.field(fieldDate).radio.set(dateToday)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.Equal(t, inputFilterForm, m.input.kind)
	assert.Equal(t, "Today and Scheduled show open issues only", m.input.err)
	m.input.field(fieldStatus).radio.set("open")
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.Equal(t, inputNone, m.input.kind)
	assert.Equal(t, dateToday, m.list.filter.Date)

	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's'})
	assert.Empty(t, m.list.filter.Status)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's'})
	assert.Equal(t, "open", m.list.filter.Status)
}

func TestDateToggle_DayRolloverReconcilesSplitDetail(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "empty"}[empty], func(t *testing.T) {
			withClock(t, func() time.Time { return time.Date(2026, 11, 11, 0, 1, 0, 0, time.UTC) })
			m, cleanup := splitTestSetup(t)
			defer cleanup()
			m.list.issues = []Issue{dated("tomorrow", "2026-11-11", "")}
			if !empty {
				m.list.issues = append(m.list.issues, dated("later", "2026-11-12", ""))
			}
			m.list.filter = ListFilter{Date: dateScheduled, Day: "2026-11-10"}
			old := m.list.issues[0]
			m.detail.issue, m.detail.scopePID = &old, 7
			m, _ = stepModel(m, dayTickMsg{})
			if empty {
				require.Nil(t, m.detail.issue)
				return
			}
			require.NotNil(t, m.detail.issue)
			assert.Equal(t, "later", m.detail.issue.ShortID)
		})
	}
}

func TestDateToggle_FilterCommitReconcilesSplitDetail(t *testing.T) {
	withClock(t, func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) })
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m.list.issues = []Issue{dated("future", "2026-11-12", ""), dated("due", "2026-11-10", "")}
	old := m.list.issues[0]
	m.detail.issue, m.detail.scopePID = &old, 7
	m = openFilterForm(t, m)
	m.input.field(fieldDate).radio.set(dateToday)
	m, _ = stepModel(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	require.NotNil(t, m.detail.issue)
	assert.Equal(t, "due", m.detail.issue.ShortID)
}

func TestDateToggle_DaemonSupport(t *testing.T) {
	for _, version := range []string{"0.35.0", "0.36.0"} {
		t.Run(version, func(t *testing.T) {
			calls := 0
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "/api/v1/health", r.URL.Path)
				calls++
				respondJSON(t, w, map[string]any{"api_schema_version": version})
			})
			m := filterFormFixture()
			m.api = newUndoClient(client)
			form := newFilterForm(m.list.filter, m.scope)
			form.field(fieldDate).radio.set(dateToday)
			m, cmd := m.commitFilterForm(form)
			m = drainInboxCmds(m, cmd)
			assert.Equal(t, 1, calls)
			assert.Equal(t, dateToday, m.list.filter.Date)
			if version == "0.36.0" {
				assert.Empty(t, m.list.status)
			} else {
				assert.Contains(t, m.list.status, "upgrade the daemon")
			}
		})
	}
}

func TestDateToggle_SearchCancelKeepsExpiredScheduledDetailEmpty(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	withClock(t, func() time.Time { return now })
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m.api = nil
	m.list.issues = []Issue{dated("soon", "2026-11-11", "")}
	form := newFilterForm(m.list.filter, m.scope)
	form.field(fieldDate).radio.set(dateScheduled)
	m, _ = m.commitFilterForm(form)
	seed := m.list.issues[0]
	m.detail.issue, m.detail.scopePID = &seed, 7
	m = openBarFromCmd(t, m, '/')
	now = now.Add(2 * time.Minute)
	m, _ = stepModel(m, dayTickMsg{})
	require.Empty(t, m.list.visibleRows())
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	require.Equal(t, inputNone, m.input.kind)
	assert.Nil(t, m.detail.issue, "cancel must keep the expired Scheduled row out of detail")
	m, _ = stepModel(m, tea.KeyPressMsg{Code: tea.KeyTab})
	m, _ = stepModel(m, runeKey('d'))
	assert.NotEqual(t, inputDatesForm, m.input.kind, "an empty Scheduled result has no row to edit")
}

func TestDateToggle_SearchCancelKeepsRemotelyRescheduledIssueOutOfToday(t *testing.T) {
	withClock(t, func() time.Time { return datesNow })
	h := newDatesHarness(t, map[string]string{"scheduled_on": "2026-11-02"}, viewList)
	h.m.sseCh = nil
	h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 160, Height: 40})
	require.Equal(t, splitlayout.Split, h.m.layout)
	h.m.list.filter = ListFilter{Date: dateToday}.stampDay()
	h.m = openBarFromCmd(t, h.m, '/')
	h.m = typeString(h.m, "missing")
	remote := NewClient(h.env.URL, h.env.HTTP)
	_, err := remote.SetDates(t.Context(), h.issue.ProjectID, h.issue.UID, DatesInput{
		Actor: "bob", IfMatchRevision: h.revision(), Set: map[string]*string{"scheduled_on": new("2026-12-01")},
	})
	require.NoError(t, err)
	h.m = drainInboxCmds(h.m, h.m.list.refetchCmd(h.m.api, h.m.scope))
	require.Empty(t, h.m.list.visibleRows())
	cancel := h.key(tea.KeyPressMsg{Code: tea.KeyEsc})
	h.m = drainInboxCmds(h.m, cancel)
	require.Empty(t, h.m.list.visibleRows())
	assert.Nil(t, h.m.detail.issue, "cancel leaves Today detail empty after the sole issue moves to December")
}

func TestDateToggle_MidnightFetchPreservesFreshlyFilteredSelection(t *testing.T) {
	now := time.Date(2026, 11, 10, 23, 59, 0, 0, time.UTC)
	withClock(t, func() time.Time { return now })
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m.api = nil
	issues := []Issue{dated("new", "2026-11-11", ""), dated("keep", "2026-11-10", "")}
	m.list.issues = issues
	form := newFilterForm(m.list.filter, m.scope)
	form.field(fieldDate).radio.set(dateToday)
	m, _ = m.commitFilterForm(form)
	before, ok := m.list.targetRow()
	require.True(t, ok)
	require.Equal(t, "keep", before.ShortID)
	now = now.Add(2 * time.Minute)
	m, _ = stepModel(m, refetchedMsg{dispatchKey: m.currentCacheKey(), issues: issues})
	assert.Equal(t, "2026-11-11", m.list.filter.Day)
	after, ok := m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, "keep", after.ShortID)
	require.NotNil(t, m.detail.issue)
	assert.Equal(t, "keep", m.detail.issue.ShortID)
}

func TestDateToggle_DaemonCheckIgnoresSupersededFailure(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/health", r.URL.Path)
		calls++
		if calls == 1 {
			http.Error(w, `{"title":"temporarily unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		respondJSON(t, w, map[string]any{"api_schema_version": "0.36.0"})
	})
	m := filterFormFixture()
	m.api = newUndoClient(client)
	form := newFilterForm(m.list.filter, m.scope)
	form.field(fieldDate).radio.set(dateToday)
	m, oldCmd := m.commitFilterForm(form)
	oldResult := oldCmd()
	form = newFilterForm(m.list.filter, m.scope)
	form.field(fieldDate).radio.set("any")
	m, _ = m.commitFilterForm(form)
	form = newFilterForm(m.list.filter, m.scope)
	form.field(fieldDate).radio.set(dateToday)
	m, newCmd := m.commitFilterForm(form)
	m = drainInboxCmds(m, newCmd)
	require.Empty(t, m.list.status)
	m, _ = stepModel(m, oldResult)
	assert.Empty(t, m.list.status, "the current filter must ignore the superseded HTTP 503")
}
