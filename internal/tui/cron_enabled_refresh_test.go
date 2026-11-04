package tui

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A completed refresh cannot make a backlog calculation from before a parent
// change current again, even when the selected job's revision is unchanged.
func TestCronResumeCompletedRefreshRequiresRecount(t *testing.T) {
	for _, stage := range []string{"lookup", "confirmation"} {
		t.Run(stage, func(t *testing.T) {
			m, api := enabledFixture(t, pausedNative())
			withEnabledBacklog(api)
			if stage == "lookup" {
				api.parent.open = false
			}
			m, lookup := press(m, keyRune('p'))
			reply := lookup().(cronEnabledReadyMsg)
			if stage == "lookup" {
				require.Zero(t, reply.request.count, "the old closed parent suppressed the backlog")
			} else {
				m, _ = m.cronEnabledReady(reply)
				require.Equal(t, modalCronResume, m.modal)
				require.Equal(t, 5, m.cron.enabled.count)
			}
			before := m.cron.events
			next, _ := m.handleEventReceived(eventReceivedMsg{projectID: m.scope.projectID, issueUID: api.shown.Definition.Issue.ParentUID, eventType: "issue.reopened"})
			m = next.(Model)
			require.Greater(t, m.cron.events, before)
			fresh := *m.cron.data
			fresh.events = m.cron.events
			fresh.parentOpen = map[string]bool{api.shown.Definition.Issue.ParentUID: true}
			fresh.progress = api.parent.progress
			m = m.cronLoaded(cronLoadedMsg(fresh))
			require.Equal(t, m.cron.events, m.cron.data.events, "the refresh has completed")
			require.Equal(t, api.shown.DefinitionEventUID, m.cron.data.jobs[0].DefinitionEventUID)
			if stage == "lookup" {
				m, _ = m.cronEnabledReady(reply)
			} else {
				m, _ = m.routeModalKey(keyRune('y'))
			}
			require.Nil(t, m.cron.enabled, "a completed refresh must discard the old backlog calculation before a write")
			require.Equal(t, modalNone, m.modal)
			require.Empty(t, api.enabledCalls)
			require.NotNil(t, m.toast)
			require.Contains(t, m.toast.text, "after refresh")
		})
	}
}
