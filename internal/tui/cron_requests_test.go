package tui

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

type runtimeRequestCall struct {
	projectID               int64
	id, kind, target, actor string
}
type cronRuntimeTestAPI struct {
	*cronEnabledTestAPI
	calls  []runtimeRequestCall
	errs   []error
	run    db.CronRun
	runErr error
}

func (a *cronRuntimeTestAPI) RequestCron(_ context.Context, pid int64, id, kind, target, actor string) (db.RequestCronOut, error) {
	a.calls = append(a.calls, runtimeRequestCall{pid, id, kind, target, actor})
	if len(a.errs) > 0 {
		err := a.errs[0]
		a.errs = a.errs[1:]
		return db.RequestCronOut{}, err
	}
	return db.RequestCronOut{Request: db.CronRequest{UID: id, Kind: kind}}, nil
}
func (a *cronRuntimeTestAPI) ShowCronRun(context.Context, int64, string) (db.CronRun, error) {
	return a.run, a.runErr
}
func runtimeFixture(t *testing.T, cancel bool) (Model, *cronRuntimeTestAPI) {
	t.Helper()
	m, base := enabledFixture(t, executeRecord(`{}`))
	api := &cronRuntimeTestAPI{cronEnabledTestAPI: base}
	m.api = api
	m.cron.data.project.controls = []string{"job_pause", "project_pause", "run_request", "cancel_request"}
	if cancel {
		m.cron.tab = cronRuns
		m.cron.data.runs[0].Status = "running"
		raw, err := json.Marshal(m.cron.data.runs[0])
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &api.run))
	}
	return m, api
}
func runtimeDialog(t *testing.T, m Model, key rune) Model {
	t.Helper()
	m, cmd := press(m, keyRune(key))
	require.Nil(t, cmd)
	require.Equal(t, modalCronRequest, m.modal)
	require.NotNil(t, m.cron.request)
	require.Empty(t, m.cron.request.requestUID, "opening confirmation must not age the admission identity")
	return m
}
func runtimeDone(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	require.NotNil(t, cmd)
	next, _, handled := m.routeTopLevel(cmd())
	require.True(t, handled)
	return next.(Model)
}
func TestCronRuntimeRequestConfirmAndRetry(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		for _, detail := range []bool{false, true} {
			m, api := runtimeFixture(t, cancel)
			if detail {
				m = sendKey(m, tea.KeyEnter)
			}
			key := 'R'
			kind := "run"
			target := api.shown.UID
			if cancel {
				key = 'X'
				kind = "cancel"
				target = api.run.UID
			}
			m = runtimeDialog(t, m, key)
			m, cmd := m.routeModalKey(keyRune('n'))
			require.Nil(t, cmd)
			require.Nil(t, m.cron.request)
			require.Empty(t, api.calls)
			m = runtimeDialog(t, m, key)
			before := time.Now().UTC().Truncate(time.Millisecond)
			m, cmd = m.routeModalKey(keyRune('y'))
			require.NotNil(t, cmd)
			// Sending cannot be repeated or dismissed before its outcome arrives.
			next, again := m.routeModalKey(keyRune('y'))
			require.Nil(t, again)
			require.Equal(t, m.cron.request, next.cron.request)
			next, again = m.routeModalKey(keyRune('n'))
			require.Nil(t, again)
			require.Equal(t, modalCronRequest, next.modal)
			api.errs = []error{errors.New("connection lost")}
			m = runtimeDone(t, m, cmd)
			require.Equal(t, modalCronRequest, m.modal)
			require.Len(t, api.calls, 1)
			first := api.calls[0]
			require.True(t, uid.Valid(first.id))
			require.False(t, uid.MustTime(first.id).Before(before))
			require.Equal(t, runtimeRequestCall{7, first.id, kind, target, "operator"}, first)
			require.Equal(t, first.id, m.cron.request.requestUID)
			// An ambiguous retry keeps its body even after current eligibility changes.
			if cancel {
				m.cron.data.runs[0].Status = "succeeded"
			} else {
				m.cron.data.project.pause = &db.CronProjectPause{Invalid: true}
				m.cron.data.jobs[0].Definition.Enabled = false
			}
			m, cmd = m.routeModalKey(keyRune('y'))
			m = runtimeDone(t, m, cmd)
			require.Len(t, api.calls, 2)
			require.Equal(t, first, api.calls[1])
			require.Nil(t, m.cron.request)
			require.Equal(t, modalNone, m.modal)
		}
	}
}
func TestCronRuntimeRequestFirstSubmitFences(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		for _, change := range []string{"connection", "scope", "selection", "revision", "read only", "capability"} {
			t.Run(change+map[bool]string{false: " run", true: " cancel"}[cancel], func(t *testing.T) {
				m, api := runtimeFixture(t, cancel)
				key := 'R'
				if cancel {
					key = 'X'
				}
				m = runtimeDialog(t, m, key)
				switch change {
				case "connection":
					m.connGen++
				case "scope":
					m.scopeGen++
				case "selection":
					m.cron.stack = []cronFrame{{ref: cronRef{cronJobs, "other"}}}
				case "revision":
					if cancel {
						m.cron.data.runs[0].Revision++
					} else {
						m.cron.data.jobs[0].DefinitionEventUID = "changed"
					}
				case "read only":
					m.issueScoped = true
					m.scopedWritable = false
				case "capability":
					m.cron.data.project.controls = nil
				}
				_, cmd := m.routeModalKey(keyRune('y'))
				require.Nil(t, cmd)
				require.Empty(t, api.calls)
			})
		}
	}
}
func TestCronRuntimeRequestLiveRevisionFence(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		m, api := runtimeFixture(t, cancel)
		key := 'R'
		if cancel {
			key = 'X'
			api.run.Revision++
		} else {
			api.shown.DefinitionEventUID = "changed"
		}
		m = runtimeDialog(t, m, key)
		m, cmd := m.routeModalKey(keyRune('y'))
		m = runtimeDone(t, m, cmd)
		require.Empty(t, api.calls)
		require.Nil(t, m.cron.request)
		require.NotNil(t, m.toast)
		require.Contains(t, m.toast.text, "changed")
	}
}
func TestCronRuntimeRequestEligibility(t *testing.T) {
	for _, reason := range []string{"native", "notify", "disabled", "deleted", "paused", "project error", "unsupported", "jobs error", "pending", "read only"} {
		t.Run(reason, func(t *testing.T) {
			m, api := runtimeFixture(t, false)
			switch reason {
			case "native":
				m.cron.data.jobs[0].Definition.Action.Kind = "notify"
				m.cron.data.jobs[0].Definition.Issue.ParentUID = new(cronTestParent)
			case "notify":
				m.cron.data.jobs[0].Definition.Action.Kind = "notify"
			case "disabled":
				m.cron.data.jobs[0].Definition.Enabled = false
			case "deleted":
				m.cron.data.jobs[0].DeletedAt = new(time.Now())
			case "paused":
				m.cron.data.project.pause = &db.CronProjectPause{Invalid: true}
			case "project error":
				m.cron.data.projectErr = errors.New("unavailable")
			case "unsupported":
				m.cron.data.project.controls = nil
			case "jobs error":
				m.cron.data.jobsErr = errors.New("unavailable")
			case "pending":
				m.cron.enabled = &cronEnabledRequest{}
			case "read only":
				m.issueScoped = true
			}
			m, cmd := press(m, keyRune('R'))
			require.Nil(t, cmd)
			require.Nil(t, m.cron.request)
			require.Empty(t, api.calls)
		})
	}
	m, api := runtimeFixture(t, true)
	m.cron.data.project.pause = &db.CronProjectPause{Invalid: true}
	m = runtimeDialog(t, m, 'X')
	m, cmd := m.routeModalKey(keyRune('y'))
	m = runtimeDone(t, m, cmd)
	require.Len(t, api.calls, 1)
	m, _ = runtimeFixture(t, true)
	m.cron.data.runs[0].Status = "succeeded"
	m, cmd = press(m, keyRune('X'))
	require.Nil(t, cmd)
	require.Nil(t, m.cron.request)
}
func TestCronRuntimeRequestStaleCompletionAndDefinitiveError(t *testing.T) {
	m, _ := runtimeFixture(t, false)
	m = runtimeDialog(t, m, 'R')
	m, cmd := m.routeModalKey(keyRune('y'))
	msg := cmd()
	m.connGen++
	m, _ = m.cronSync()
	require.Nil(t, m.cron.request)
	next, _, _ := m.routeTopLevel(msg)
	require.Nil(t, next.(Model).cron.request)
	m, api := runtimeFixture(t, false)
	api.errs = []error{&APIError{Status: http.StatusGone, Code: "request_expired", Message: "Use a fresh UID"}}
	m = runtimeDialog(t, m, 'R')
	m, cmd = m.routeModalKey(keyRune('y'))
	m = runtimeDone(t, m, cmd)
	require.Nil(t, m.cron.request)
	require.Equal(t, modalNone, m.modal)
}
func TestCronRuntimeRequestDetails(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		m, _ := runtimeFixture(t, cancel)
		ref := cronRef{cronJobs, m.cron.data.jobs[0].UID}
		request := db.CronRequest{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB9", Kind: "run", JobUID: ref.uid, RequestedBy: "operator", RequestedAt: snapshotFixedNow, ExpiresAt: snapshotFixedNow.Add(time.Minute), State: "pending"}
		if cancel {
			ref = cronRef{cronRuns, m.cron.data.runs[0].UID}
			request.Kind = "cancel"
			request.RunUID = new(ref.uid)
		}
		m.cron.data.requests = []db.CronRequest{request}
		_, _, lines := cronDetail(m, ref)
		var text strings.Builder
		for _, line := range lines {
			text.WriteString(line.text)
			text.WriteByte('\n')
		}
		require.Contains(t, text.String(), request.UID)
		require.Contains(t, text.String(), "pending")
		require.Contains(t, text.String(), "operator")
		m.cron.data.requestsErr = errors.New("request read failed")
		_, _, lines = cronDetail(m, ref)
		text.Reset()
		for _, line := range lines {
			text.WriteString(line.text)
		}
		require.Contains(t, text.String(), "Requests · unavailable")
	}
}

func TestSnapshot_CronRuntimeRequests(t *testing.T) {
	defer snapshotInit(t)()
	for _, width := range []int{60, 80, 120} {
		for _, state := range []string{"run", "cancel", "retry", "detail"} {
			// The standard terminal-size screen replaces details below 80 columns.
			if width < 80 && state == "detail" {
				continue
			}
			t.Run(fmt.Sprintf("%s-%d", state, width), func(t *testing.T) {
				m, _ := runtimeFixture(t, state == "cancel")
				m.width = width
				m.height = 24
				if state == "detail" {
					m.cron.data.requests = []db.CronRequest{{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB9", Kind: "run", JobUID: m.cron.data.jobs[0].UID, RequestedBy: "operator", RequestedAt: snapshotFixedNow, ExpiresAt: snapshotFixedNow.Add(10 * time.Minute), State: "pending"}}
					m = sendKey(m, tea.KeyEnter)
					_, _, lines := cronScreen(m)
					*m.cronCursor() = len(lines) - 1
				} else {
					key := 'R'
					if state == "cancel" {
						key = 'X'
					}
					m = runtimeDialog(t, m, key)
					if state == "retry" {
						m.cron.request.requestUID = "01ARZ3NDEKTSV4RRFFQ69G5FB9"
						m.cron.request.err = errors.New("connection interrupted")
					}
				}
				assertGolden(t, fmt.Sprintf("cron-request-%s-%d", state, width), m.View().Content)
			})
		}
	}
}

// Full cron help teaches both runtime keys; ordinary footers remain scoped to
// the current tab. The family plan requires p, R and X together in help.
func TestCronHelpListsAllControlKeys(t *testing.T) {
	m, _ := runtimeFixture(t, false)
	m.cron.help = true
	view := stripANSI(m.viewBody())
	require.Contains(t, view, "pause/resume")
	require.Contains(t, view, "request run")
	require.Contains(t, view, "request cancel")
}
