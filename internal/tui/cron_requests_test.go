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

func (a *cronRuntimeTestAPI) RequestCron(_ context.Context, pid int64, id, kind, target, actor string) error {
	a.calls = append(a.calls, runtimeRequestCall{pid, id, kind, target, actor})
	if len(a.errs) > 0 {
		err := a.errs[0]
		a.errs = a.errs[1:]
		return err
	}
	return nil
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
func runtimeKey(cancel bool) rune {
	if cancel {
		return 'X'
	}
	return 'R'
}
func runtimeDialog(t *testing.T, m Model, key rune) Model {
	t.Helper()
	m, cmd := press(m, keyRune(key))
	require.Nil(t, cmd)
	require.Equal(t, modalCronAction, m.modal)
	require.NotNil(t, m.cron.action)
	require.Empty(t, m.cron.action.requestUID, "opening confirmation must not age the admission identity")
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
			for _, removed := range []bool{false, true} {
				if removed && !cancel {
					continue
				}
				m, api := runtimeFixture(t, cancel)
				if detail {
					m = sendKey(m, tea.KeyEnter)
				}
				key := runtimeKey(cancel)
				kind := "run"
				target := api.shown.UID
				if cancel {
					kind = "cancel"
					target = api.run.UID
				}
				m = runtimeDialog(t, m, key)
				m, cmd := m.routeModalKey(keyRune('n'))
				require.Nil(t, cmd)
				require.Nil(t, m.cron.action)
				require.Empty(t, api.calls)
				m = runtimeDialog(t, m, key)
				before := time.Now().UTC().Truncate(time.Millisecond)
				m, cmd = m.routeModalKey(keyRune('y'))
				require.NotNil(t, cmd)
				// Sending cannot be repeated or dismissed before its outcome arrives.
				next, again := m.routeModalKey(keyRune('y'))
				require.Nil(t, again)
				require.Equal(t, m.cron.action, next.cron.action)
				next, again = m.routeModalKey(keyRune('n'))
				require.Nil(t, again)
				require.Equal(t, modalCronAction, next.modal)
				api.errs = []error{errors.New("connection lost")}
				m = runtimeDone(t, m, cmd)
				require.Equal(t, modalCronAction, m.modal)
				require.Len(t, api.calls, 1)
				first := api.calls[0]
				require.True(t, uid.Valid(first.id))
				require.False(t, uid.MustTime(first.id).Before(before))
				require.Equal(t, runtimeRequestCall{7, first.id, kind, target, "operator"}, first)
				require.Equal(t, first.id, m.cron.action.requestUID)
				// An ambiguous retry keeps its body even after current eligibility changes.
				if cancel {
					if removed {
						next, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: m.scope.projectID})
						m = next.(Model)
						data := *m.cron.data
						data.runs = nil
						data.events = m.cron.events
						m = m.cronLoaded(cronLoadedMsg(data))
					} else {
						m.cron.data.runs[0].Status = "succeeded"
					}
				} else {
					m.cron.data.project.pause = &db.CronProjectPause{Invalid: true}
					m.cron.data.jobs[0].Definition.Enabled = false
				}
				m, cmd = m.routeModalKey(keyRune('y'))
				m = runtimeDone(t, m, cmd)
				require.Len(t, api.calls, 2)
				require.Equal(t, first, api.calls[1])
				require.Nil(t, m.cron.action)
				require.Equal(t, modalNone, m.modal)
			}
		}
	}
}
func TestCronRuntimeRequestFirstSubmitFences(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		for _, change := range []string{"connection", "scope", "selection", "revision", "read only", "capability"} {
			t.Run(change+map[bool]string{false: " run", true: " cancel"}[cancel], func(t *testing.T) {
				m, api := runtimeFixture(t, cancel)
				m = runtimeDialog(t, m, runtimeKey(cancel))
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
		if cancel {
			api.run.Revision++
		} else {
			api.shown.DefinitionEventUID = "changed"
		}
		m = runtimeDialog(t, m, runtimeKey(cancel))
		m, cmd := m.routeModalKey(keyRune('y'))
		m = runtimeDone(t, m, cmd)
		require.Empty(t, api.calls)
		require.Nil(t, m.cron.action)
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
				m.cron.action = &cronPending{kind: cronPause}
			case "read only":
				m.issueScoped = true
			}
			m, cmd := press(m, keyRune('R'))
			require.Nil(t, cmd)
			if reason != "pending" {
				require.Nil(t, m.cron.action)
			}
			require.Equal(t, modalNone, m.modal)
			require.Empty(t, api.calls)
		})
	}
	// Cancellation stays available while the project is paused, but only for a running run.
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
	require.Nil(t, m.cron.action)
}
func TestCronRuntimeRequestStaleCompletionAndDefinitiveError(t *testing.T) {
	m, _ := runtimeFixture(t, false)
	m = runtimeDialog(t, m, 'R')
	m, cmd := m.routeModalKey(keyRune('y'))
	msg := cmd()
	m.connGen++
	m, _ = m.cronSync()
	require.Nil(t, m.cron.action)
	next, _, _ := m.routeTopLevel(msg)
	require.Nil(t, next.(Model).cron.action)
	m, api := runtimeFixture(t, false)
	api.errs = []error{&APIError{Status: http.StatusGone, Code: "request_expired", Message: "Use a fresh UID"}}
	m = runtimeDialog(t, m, 'R')
	m, cmd = m.routeModalKey(keyRune('y'))
	m = runtimeDone(t, m, cmd)
	require.Nil(t, m.cron.action)
	require.Equal(t, modalNone, m.modal)
}

// Contract: activity on another job or run does not cancel R/X confirmation.
func TestCronRuntimeRequestSurvivesUnrelatedRefresh(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		for _, stage := range []string{"admission", "confirmation"} {
			for _, completed := range []bool{false, true} {
				t.Run(fmt.Sprintf("cancel=%t/%s/completed=%t", cancel, stage, completed), func(t *testing.T) {
					m, api := runtimeFixture(t, cancel)
					if stage == "confirmation" {
						m = runtimeDialog(t, m, runtimeKey(cancel))
					}
					next, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: m.scope.projectID})
					m = next.(Model)
					require.NotEqual(t, m.cron.data.events, m.cron.events)
					if completed {
						data := *m.cron.data
						data.events = m.cron.events
						m = m.cronLoaded(cronLoadedMsg(data))
					}
					if stage == "admission" {
						m = runtimeDialog(t, m, runtimeKey(cancel))
					}
					m, cmd := m.routeModalKey(keyRune('y'))
					require.NotNil(t, cmd, "unrelated activity must not discard confirmation")
					m = runtimeDone(t, m, cmd)
					require.Len(t, api.calls, 1)
					require.Nil(t, m.cron.action)
				})
			}
		}
	}
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
		detailText := func() string {
			_, _, lines := cronDetail(m, ref)
			var text strings.Builder
			for _, line := range lines {
				text.WriteString(line.text + "\n")
			}
			return text.String()
		}
		require.Contains(t, detailText(), request.Kind+" · pending · "+request.UID)
		require.Contains(t, detailText(), "operator")
		m.cron.data.requestsErr = errors.New("request read failed")
		require.Contains(t, detailText(), "Requests · unavailable")
	}
}

// One dialog per state at the standard width, plus the narrow-terminal
// overlay that keeps the dialog and its keys visible below 80 columns.
func TestSnapshot_CronRuntimeRequests(t *testing.T) {
	defer snapshotInit(t)()
	for _, tc := range []struct {
		state string
		width int
	}{{"run", 80}, {"cancel", 80}, {"retry", 80}, {"detail", 80}, {"retry", 60}} {
		t.Run(fmt.Sprintf("%s-%d", tc.state, tc.width), func(t *testing.T) {
			m, _ := runtimeFixture(t, tc.state == "cancel")
			m.width = tc.width
			m.height = 24
			if tc.state == "detail" {
				m.cron.data.requests = []db.CronRequest{{UID: "01ARZ3NDEKTSV4RRFFQ69G5FB9", Kind: "run", JobUID: m.cron.data.jobs[0].UID, RequestedBy: "operator", RequestedAt: snapshotFixedNow, ExpiresAt: snapshotFixedNow.Add(10 * time.Minute), State: "pending"}}
				m = sendKey(m, tea.KeyEnter)
				_, _, lines := cronScreen(m)
				*m.cronCursor() = len(lines) - 1
			} else {
				m = runtimeDialog(t, m, runtimeKey(tc.state == "cancel"))
				if tc.state == "retry" {
					m.cron.action.requestUID = "01ARZ3NDEKTSV4RRFFQ69G5FB9"
					m.cron.action.err = errors.New("connection interrupted")
				}
			}
			assertGolden(t, fmt.Sprintf("cron-request-%s-%d", tc.state, tc.width), m.View().Content)
		})
	}
}
