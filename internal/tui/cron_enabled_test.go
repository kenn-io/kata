package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	kataapi "go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

type enabledCall struct {
	uid, expected, actor string
	enabled              bool
}
type cronEnabledTestAPI struct {
	*cronWriteTestAPI
	parent                cronParent
	parentErr, errorWrite error
	enabledCalls          []enabledCall
	mutationDeadline      time.Time
	mutationHasDeadline   bool
}

func (a *cronEnabledTestAPI) readCronParent(context.Context, int64, string) (cronParent, error) {
	return a.parent, a.parentErr
}
func (a *cronEnabledTestAPI) CronJobEnabled(ctx context.Context, _ int64, id, expected, actor string, enabled bool) (db.CronJob, error) {
	a.mutationDeadline, a.mutationHasDeadline = ctx.Deadline()
	a.enabledCalls = append(a.enabledCalls, enabledCall{id, expected, actor, enabled})
	return a.shown, a.errorWrite
}
func enabledFixture(t *testing.T, record db.CronJob) (Model, *cronEnabledTestAPI) {
	t.Helper()
	m, base := cronWriteFixture(t)
	base.shown = record
	raw, err := json.Marshal(kataapi.CronJobFrom(record))
	require.NoError(t, err)
	var wire generated.CronJob
	require.NoError(t, json.Unmarshal(raw, &wire))
	m.cron.data.jobs = []generated.CronJob{wire}
	api := &cronEnabledTestAPI{cronWriteTestAPI: base}
	m.api = api
	m.list.actor = "operator"
	return m, api
}
func deliverEnabled(t *testing.T, m Model, cmd tea.Cmd) (Model, tea.Cmd) {
	t.Helper()
	require.NotNil(t, cmd)
	next, nextCmd, handled := m.routeTopLevel(cmd())
	require.True(t, handled)
	return next.(Model), nextCmd
}
func pausedNative() db.CronJob {
	job := nativeRecord()
	job.Definition.Enabled = false
	job.Definition.Trigger = cron.Trigger{Kind: "interval", IntervalSeconds: 60}
	job.Definition.Catchup = "all"
	return job
}
func withEnabledBacklog(api *cronEnabledTestAPI) {
	anchor := time.Now().UTC().Add(-5 * time.Minute)
	api.parent = cronParent{open: true, progress: map[string]cron.RecurringProgress{api.shown.UID: {Version: 1, Anchor: anchor, Through: anchor}}}
}

func TestCronEnabledKeyListAndDetail(t *testing.T) {
	for _, detail := range []bool{false, true} {
		t.Run(map[bool]string{false: "list", true: "detail"}[detail], func(t *testing.T) {
			m, api := enabledFixture(t, executeRecord(`{"large":9007199254740993}`))
			if detail {
				m = sendKey(m, tea.KeyEnter)
			}
			api.showErr = errors.New("unexpected job lookup")
			m, cmd := press(m, keyRune('p'))
			require.NotNil(t, cmd)
			reply, ok := cmd().(cronEnabledReadyMsg)
			require.True(t, ok)
			require.NoError(t, reply.err, "pause uses the selected revision without a lookup")
			m, write := m.cronEnabledReady(reply)
			_, _ = deliverEnabled(t, m, write)
			require.Equal(t, []enabledCall{{uid: api.shown.UID, expected: api.shown.DefinitionEventUID, actor: "operator", enabled: false}}, api.enabledCalls)
		})
	}
}

func TestCronEnabledWriteAllowsFederationForwarding(t *testing.T) {
	m, api := enabledFixture(t, executeRecord(`{}`))
	m, lookup := press(m, keyRune('p'))
	m, write := deliverEnabled(t, m, lookup)
	_, _ = deliverEnabled(t, m, write)

	require.True(t, api.mutationHasDeadline)
	require.Greater(t, time.Until(api.mutationDeadline), 15*time.Second)
}

func TestCronResumeBacklogConfirmation(t *testing.T) {
	m, api := enabledFixture(t, pausedNative())
	withEnabledBacklog(api)
	m, lookup := press(m, keyRune('p'))
	m, cmd := deliverEnabled(t, m, lookup)
	require.Nil(t, cmd)
	require.Equal(t, modalCronResume, m.modal)
	require.Empty(t, api.enabledCalls)
	require.Equal(t, 5, m.cron.enabled.count, "confirm only the selected job's missed children")
	m, write := m.routeModalKey(keyRune('y'))
	require.NotNil(t, write)
	_, _ = deliverEnabled(t, m, write)
	require.Len(t, api.enabledCalls, 1)
	require.Equal(t, api.shown.DefinitionEventUID, api.enabledCalls[0].expected)
	require.True(t, api.enabledCalls[0].enabled)
}

func TestCronResumeLookupFences(t *testing.T) {
	for _, kind := range []string{"selection", "scope", "connection", "new-definition", "filter", "help", "count-error", "missing-progress", "closed-parent", "read-only"} {
		t.Run(kind, func(t *testing.T) {
			m, api := enabledFixture(t, pausedNative())
			withEnabledBacklog(api)
			m, lookup := press(m, keyRune('p'))
			require.NotNil(t, lookup)
			switch kind {
			case "selection":
				m.cron.data.jobs[0].UID = cronTestIssue
			case "scope":
				m.scopeGen++
			case "connection":
				m.connGen++
			case "new-definition":
				m.cron.data.jobs[0].DefinitionEventUID = cronTestIssue
			case "filter":
				m.cron.filtering = true
			case "help":
				m.cron.help = true
			case "count-error":
				api.parentErr = errors.New("count unavailable")
			case "missing-progress":
				api.parent.progress = nil
			case "closed-parent":
				api.parent.open = false
			case "read-only":
				m.issueScoped = true
				m.scopedWritable = false
			}
			m, cmd := deliverEnabled(t, m, lookup)
			require.Equal(t, modalNone, m.modal)
			require.Empty(t, api.enabledCalls)
			if kind == "count-error" {
				require.NotNil(t, m.toast)
				require.Contains(t, m.toast.text, "count unavailable")
			}
			if kind == "missing-progress" {
				require.NotNil(t, m.toast)
				require.Contains(t, m.toast.text, "cannot count missed runs")
			}
			if kind == "closed-parent" {
				// Resume does not imply delivery while the parent gate is closed.
				require.NotNil(t, cmd)
				_, _ = deliverEnabled(t, m, cmd)
				require.Len(t, api.enabledCalls, 1)
			}
		})
	}
}

func TestCronEnabledAdmissionAndPending(t *testing.T) {
	for _, kind := range []string{"read-only", "stale-permissions", "stale-jobs", "pending", "other-tab"} {
		t.Run(kind, func(t *testing.T) {
			m, api := enabledFixture(t, executeRecord(`{}`))
			switch kind {
			case "read-only":
				m.issueScoped = true
				m.scopedWritable = false
			case "stale-permissions":
				m.authCapabilitiesRequired = true
				m.authCapabilitiesReady = false
			case "stale-jobs":
				m.cron.data.jobsErr = errors.New("unavailable")
			case "pending":
				m, _ = press(m, keyRune('p'))
			case "other-tab":
				m = sendRune(m, '3')
			}
			m, cmd := press(m, keyRune('p'))
			if kind == "stale-permissions" {
				require.NotNil(t, cmd, "stale permissions must schedule capability discovery")
				require.Nil(t, m.cron.enabled, "permission discovery must not admit a pending enabled action")
				require.NotNil(t, m.toast)
				require.Contains(t, m.toast.text, "Checking daemon permissions")
				require.Empty(t, api.enabledCalls)
				return
			}
			require.Nil(t, cmd)
			require.Empty(t, api.enabledCalls)
		})
	}
}

func TestCronResumeConflictRefreshes(t *testing.T) {
	m, api := enabledFixture(t, pausedNative())
	withEnabledBacklog(api)
	api.errorWrite = &APIError{Status: http.StatusConflict, Code: "cron_conflict", Message: "changed"}
	m, lookup := press(m, keyRune('p'))
	m, _ = deliverEnabled(t, m, lookup)
	m, write := m.routeModalKey(keyRune('y'))
	before := m.cron.events
	m, _ = deliverEnabled(t, m, write)
	require.Greater(t, m.cron.events, before)
	require.Equal(t, modalNone, m.modal)
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "changed")
}

func TestCronResumeConfirmationFences(t *testing.T) {
	for _, kind := range []string{"cancel", "revision", "permissions", "scope", "connection"} {
		t.Run(kind, func(t *testing.T) {
			m, api := enabledFixture(t, pausedNative())
			withEnabledBacklog(api)
			m, lookup := press(m, keyRune('p'))
			m, _ = deliverEnabled(t, m, lookup)
			require.NotEqual(t, modalNone, m.modal)
			switch kind {
			case "cancel":
				m, cmd := m.routeModalKey(keyRune('n'))
				require.Nil(t, cmd)
				require.Equal(t, modalNone, m.modal)
				return
			case "revision":
				m.cron.data.jobs[0].DefinitionEventUID = cronTestIssue
			case "permissions":
				m.issueScoped = true
				m.scopedWritable = false
			case "scope":
				m.scopeGen++
			case "connection":
				m.connGen++
			}
			_, cmd := m.routeModalKey(keyRune('y'))
			require.Nil(t, cmd)
			require.Empty(t, api.enabledCalls)
		})
	}
}

func TestSnapshot_CronResumeBacklog(t *testing.T) {
	defer snapshotInit(t)()
	for _, width := range []int{60, 80, 120} {
		m, _ := enabledFixture(t, pausedNative())
		m.width = width
		m.modal = modalCronResume
		m.cron.enabled = &cronEnabledRequest{name: "Weekly report", count: 5}
		assertGolden(t, fmt.Sprintf("cron-resume-backlog-%d", width), m.View().Content)
	}
}

// An edit response already in flight must not open a competing form during a pause.
func TestCronEditLookupCannotBypassEnabledPending(t *testing.T) {
	m, api := enabledFixture(t, executeRecord(`{}`))
	m, editLookup := press(m, keyRune('e'))
	require.NotNil(t, editLookup)
	editReply := editLookup()
	m, pauseLookup := press(m, keyRune('p'))
	m, pauseWrite := deliverEnabled(t, m, pauseLookup)
	require.NotNil(t, pauseWrite)
	next, _, handled := m.routeTopLevel(editReply)
	require.True(t, handled)
	m = next.(Model)
	require.Nil(t, m.cron.form)
	require.Empty(t, api.puts)
	_, _ = deliverEnabled(t, m, pauseWrite)
	require.Len(t, api.enabledCalls, 1)
}

// Refreshing data invalidates a count, but must tell the operator to retry.
func TestCronResumeRefreshRequiresRecount(t *testing.T) {
	for _, stage := range []string{"lookup", "confirmation"} {
		t.Run(stage, func(t *testing.T) {
			m, api := enabledFixture(t, pausedNative())
			withEnabledBacklog(api)
			m, lookup := press(m, keyRune('p'))
			if stage == "confirmation" {
				m, _ = deliverEnabled(t, m, lookup)
			}
			m.cron.events++
			if stage == "lookup" {
				m, _ = deliverEnabled(t, m, lookup)
			} else {
				m, _ = m.routeModalKey(keyRune('y'))
			}
			require.Nil(t, m.cron.enabled)
			require.Equal(t, modalNone, m.modal)
			require.Empty(t, api.enabledCalls)
			require.NotNil(t, m.toast)
			require.Equal(t, toastError, m.toast.level)
			require.Equal(t, "Job data changed; press p after refresh", m.toast.text)
		})
	}
}

func TestCronResumeModalCountGrammar(t *testing.T) {
	for _, count := range []int{1, 2} {
		view := stripANSI(renderCronResumeModal(&cronEnabledRequest{name: "Weekly report", count: count}))
		word := "issues"
		if count == 1 {
			word = "issue"
		}
		require.Contains(t, view, word+" that came due")
	}
}

// Pause and resume share a selection fence, but a pause-specific bypass must
// still be rejected without relying on the resume tests.
func TestCronPauseRejectsChangedSelection(t *testing.T) {
	m, api := enabledFixture(t, executeRecord(`{}`))
	m, ready := press(m, keyRune('p'))
	m.cron.data.jobs[0].UID = cronTestIssue
	m, write := deliverEnabled(t, m, ready)
	require.Nil(t, write)
	require.Nil(t, m.cron.enabled)
	require.Empty(t, api.enabledCalls)
}
