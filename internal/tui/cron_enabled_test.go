package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
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
	enabled, discard     bool
}
type cronEnabledTestAPI struct {
	*cronWriteTestAPI
	parent                cronParent
	parentErr, errorWrite error
	passover              *db.CronPassoverState
	enabledCalls          []enabledCall
	mutationDeadline      time.Time
}

func (a *cronEnabledTestAPI) ShowCronJob(context.Context, int64, string) (CronJobRead, error) {
	return CronJobRead{Job: a.shown, Passover: a.passover}, a.showErr
}
func (a *cronEnabledTestAPI) readCronParent(context.Context, int64, string) (cronParent, error) {
	return a.parent, a.parentErr
}
func (a *cronEnabledTestAPI) CronJobEnabled(ctx context.Context, _ int64, id, expected, actor string, enabled, discard bool) (db.CronJob, error) {
	a.mutationDeadline, _ = ctx.Deadline()
	a.enabledCalls = append(a.enabledCalls, enabledCall{id, expected, actor, enabled, discard})
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

func TestCronPauseWritesSelectedRevision(t *testing.T) {
	for _, detail := range []bool{false, true} {
		t.Run(map[bool]string{false: "list", true: "detail"}[detail], func(t *testing.T) {
			m, api := enabledFixture(t, executeRecord(`{"large":9007199254740993}`))
			if detail {
				m = sendKey(m, tea.KeyEnter)
			}
			api.showErr = errors.New("unexpected job lookup")
			m, cmd := press(m, keyRune('p'))
			m, write := deliverEnabled(t, m, cmd)
			_, _ = deliverEnabled(t, m, write)
			require.Equal(t, []enabledCall{{uid: api.shown.UID, expected: api.shown.DefinitionEventUID, actor: "operator"}}, api.enabledCalls, "pause uses the selected revision without a lookup")
			require.Greater(t, time.Until(api.mutationDeadline), 15*time.Second)
		})
	}
}

// y resumes and d, where the project advertises it, resumes discarding the
// backlog; both recheck the same fences before writing.
func TestCronResumeConfirmation(t *testing.T) {
	for _, key := range []rune{'y', 'd'} {
		for _, fence := range []string{"none", "revision", "selection", "read-only", "scope", "connection"} {
			t.Run(string(key)+"/"+fence, func(t *testing.T) {
				m, api := enabledFixture(t, pausedNative())
				withEnabledBacklog(api)
				m.cron.data.project.controls = []string{"resume_discard_missed"}
				m, lookup := press(m, keyRune('p'))
				m, cmd := deliverEnabled(t, m, lookup)
				require.Nil(t, cmd)
				require.Equal(t, modalCronAction, m.modal)
				require.Equal(t, 5, m.cron.action.count, "confirm only the selected job's missed children")
				switch fence {
				case "revision":
					m.cron.data.jobs[0].DefinitionEventUID = cronTestIssue
				case "selection":
					m.cron.data.jobs = nil
				case "read-only":
					m.issueScoped = true
					m.scopedWritable = false
				case "scope":
					m.scopeGen++
				case "connection":
					m.connGen++
				}
				m, write := m.routeModalKey(keyRune(key))
				if fence != "none" {
					require.Nil(t, write)
					require.Empty(t, api.enabledCalls)
					return
				}
				_, _ = deliverEnabled(t, m, write)
				require.Equal(t, []enabledCall{{api.shown.UID, api.shown.DefinitionEventUID, "operator", true, key == 'd'}}, api.enabledCalls)
			})
		}
	}
}

// Without the capability the dialog offers no discard and d does nothing; n
// cancels.
func TestCronResumeDiscardNeedsCapability(t *testing.T) {
	m, api := enabledFixture(t, pausedNative())
	withEnabledBacklog(api)
	m, lookup := press(m, keyRune('p'))
	m, _ = deliverEnabled(t, m, lookup)
	require.NotContains(t, stripANSI(m.overlayInputAndModal(m.viewBody())), "discard")
	m, write := m.routeModalKey(keyRune('d'))
	require.Nil(t, write)
	require.Equal(t, modalCronAction, m.modal)
	m, _ = m.routeModalKey(keyRune('n'))
	require.Equal(t, modalNone, m.modal)
	require.Nil(t, m.cron.action)
	require.Empty(t, api.enabledCalls)
}

func TestCronResumeLookupFences(t *testing.T) {
	for _, kind := range []string{"selection", "scope", "connection", "new-definition", "filter", "help", "count-error", "missing-progress", "closed-parent", "read-only", "invalid-cutoff"} {
		t.Run(kind, func(t *testing.T) {
			m, api := enabledFixture(t, pausedNative())
			withEnabledBacklog(api)
			if kind == "invalid-cutoff" {
				api.passover = &db.CronPassoverState{Through: time.Now().UTC().Add(time.Hour), Invalid: true}
			}
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
			switch kind {
			case "count-error":
				require.Contains(t, m.toast.text, "count unavailable")
			case "missing-progress":
				require.Contains(t, m.toast.text, "cannot count missed runs")
			case "invalid-cutoff":
				require.Nil(t, m.cron.action)
				require.Equal(t, "Could not change the job: invalid cutoff; repair with --discard-missed", m.toast.text)
			case "closed-parent":
				// Resume does not imply delivery while the parent gate is closed.
				_, _ = deliverEnabled(t, m, cmd)
				require.Len(t, api.enabledCalls, 1)
			}
		})
	}
}

// A valid cutoff later than the stored progress leaves nothing to confirm.
func TestCronResumeAppliesPassover(t *testing.T) {
	m, api := enabledFixture(t, pausedNative())
	withEnabledBacklog(api)
	api.passover = &db.CronPassoverState{Through: time.Now().UTC().Add(time.Hour)}
	m, lookup := press(m, keyRune('p'))
	m, write := deliverEnabled(t, m, lookup)
	require.Equal(t, modalNone, m.modal)
	_, _ = deliverEnabled(t, m, write)
	require.Len(t, api.enabledCalls, 1)
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
				require.Nil(t, m.cron.action, "permission discovery must not admit a pending enabled action")
				require.Contains(t, m.toast.text, "Checking daemon permissions")
				require.Empty(t, api.enabledCalls)
				return
			}
			require.Nil(t, cmd)
			require.Empty(t, api.enabledCalls)
		})
	}
}

func TestCronEnabledWriteErrorRefreshes(t *testing.T) {
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
	require.Contains(t, m.toast.text, "changed")
}

func TestSnapshot_CronResumeDialogs(t *testing.T) {
	defer snapshotInit(t)()
	for name, allowDiscard := range map[string]bool{"cron-resume-backlog-80": false, "cron-resume-discard-80": true} {
		m, _ := enabledFixture(t, pausedNative())
		m.width = 80
		m.modal = modalCronAction
		m.cron.action = &cronPending{kind: cronResume, name: "Weekly report", count: 5, allowDiscard: allowDiscard}
		assertGolden(t, name, m.View().Content)
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

// Count 2 protects the singular/plural boundary; the count-5 snapshot cannot
// detect an accidental <= 2 singular condition.
func TestCronResumeModalCountGrammar(t *testing.T) {
	require.Contains(t, stripANSI(renderCronActionModal(&cronPending{name: "Weekly report", count: 1})), "issue that came due")
	require.Contains(t, stripANSI(renderCronActionModal(&cronPending{name: "Weekly report", count: 2})), "issues that came due")
}

// Pause and resume share a selection fence, but a pause-specific bypass must
// still be rejected without relying on the resume tests.
func TestCronPauseRejectsChangedSelection(t *testing.T) {
	m, api := enabledFixture(t, executeRecord(`{}`))
	m, ready := press(m, keyRune('p'))
	m.cron.data.jobs[0].UID = cronTestIssue
	m, write := deliverEnabled(t, m, ready)
	require.Nil(t, write)
	require.Nil(t, m.cron.action)
	require.Empty(t, api.enabledCalls)
}
