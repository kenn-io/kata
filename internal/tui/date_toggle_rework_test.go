package tui

import (
	"net/http"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewWalkSearchCancelAfterScheduledEmpties(t *testing.T) {
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

func TestReviewWalkMidnightFetchPreservesFreshlyFilteredSelection(t *testing.T) {
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
	after, ok := m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, "keep", after.ShortID)
	require.NotNil(t, m.detail.issue)
	assert.Equal(t, "keep", m.detail.issue.ShortID)
}

func TestReviewWalkDateCheckIgnoresSupersededFailure(t *testing.T) {
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
