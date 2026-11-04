package tui

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Contract: activity on another job or run does not cancel R/X confirmation.
func TestCronRuntimeRequestSurvivesUnrelatedRefresh(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		for _, stage := range []string{"admission", "confirmation"} {
			for _, completed := range []bool{false, true} {
				t.Run(fmt.Sprintf("cancel=%t/%s/completed=%t", cancel, stage, completed), func(t *testing.T) {
					m, api := runtimeFixture(t, cancel)
					key := 'R'
					if cancel {
						key = 'X'
					}
					if stage == "confirmation" {
						m = runtimeDialog(t, m, key)
					}
					next, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: m.scope.projectID})
					m = next.(Model)
					require.NotEqual(t, m.cron.data.events, m.cron.events)
					if completed {
						data := *m.cron.data
						data.events = m.cron.events
						m.cron.data = &data
					}
					if stage == "admission" {
						m = runtimeDialog(t, m, key)
					}
					m, cmd := m.routeModalKey(keyRune('y'))
					require.NotNil(t, cmd, "unrelated activity must not discard confirmation")
					m = runtimeDone(t, m, cmd)
					require.Len(t, api.calls, 1)
					require.Nil(t, m.cron.request)
				})
			}
		}
	}
}

// A lost cancellation response must remain replayable after the run disappears.
func TestCronRuntimeRetryAfterRefreshRemovesRun(t *testing.T) {
	m, api := runtimeFixture(t, true)
	m = runtimeDialog(t, m, 'X')
	api.errs = []error{errors.New("response lost")}
	m, cmd := m.routeModalKey(keyRune('y'))
	m = runtimeDone(t, m, cmd)
	require.Len(t, api.calls, 1)
	first := api.calls[0]
	next, _ := m.handleEventReceived(eventReceivedMsg{eventType: "cron.run.observed", projectID: m.scope.projectID})
	m = next.(Model)
	data := *m.cron.data
	data.runs = nil
	data.events = m.cron.events
	m.cron.data = &data
	m, cmd = m.routeModalKey(keyRune('y'))
	require.NotNil(t, cmd, "retry must retain identity when refresh removes the run")
	m = runtimeDone(t, m, cmd)
	require.Len(t, api.calls, 2)
	require.Equal(t, first, api.calls[1])
	require.Nil(t, m.cron.request)
}
