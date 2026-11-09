package tui

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func cronEnabledEvent(t *testing.T, m Model, kind, issue string, payload any) Model {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": kind, "project_id": m.scope.projectID, "issue_uid": issue, "payload": payload})
	require.NoError(t, err)
	next, _ := m.handleEventReceived(decodeEventReceived(frame{data: raw}))
	return next.(Model)
}

func cronDiff(key string, to any) map[string]any {
	return map[string]any{"diff": map[string]any{key: map[string]any{"to": to}}}
}

// Changes to the selected job, its parent's status or progress, or the cutoffs
// it counts with invalidate a resume, latched even after a refresh completes.
// Activity elsewhere still refreshes the tabs but keeps the confirmation.
func TestCronResumeInvalidation(t *testing.T) {
	job := pausedNative()
	uid, parent := job.UID, job.Definition.Issue.ParentUID
	cutoff := db.CronPassover{Version: 1, Through: time.Now().UTC().Add(time.Hour), By: "another-operator"}
	other := map[string]any{"uid": cronTestIssue, "job_uid": cronTestIssue}
	for _, tc := range []struct {
		name, event, issue string
		payload            any
		relevant           bool
	}{
		{name: "run", event: "cron.run.observed", payload: other},
		{name: "other-job", event: "cron.job.updated", payload: other},
		{name: "workflow", event: "cron.workflow.updated", payload: other},
		{name: "parent-comment", event: "issue.commented", issue: parent, payload: other},
		{name: "other-checkpoint", event: "issue.metadata_updated", issue: parent, payload: cronDiff("cron.progress."+cronTestIssue, nil)},
		{name: "parent-title", event: "issue.updated", issue: parent, payload: map[string]any{"title": "Updated title"}},
		{name: "other-cutoff", event: "project.metadata_updated", payload: cronDiff("cron.passover."+cronTestIssue, cutoff)},
		{name: "ordinary", event: "project.metadata_updated", payload: cronDiff("ordinary", cutoff)},
		{name: "request", event: "project.metadata_updated", payload: cronDiff("cron.request."+cronTestIssue, nil)},
		{name: "checkpoint", event: "issue.metadata_updated", issue: parent, payload: cronDiff("cron.progress."+uid, nil), relevant: true},
		{name: "status", event: "issue.updated", issue: parent, payload: map[string]any{"status": "closed"}, relevant: true},
		{name: "deleted", event: "issue.soft_deleted", issue: parent, relevant: true},
		{name: "reopened", event: "issue.reopened", issue: parent, relevant: true},
		{name: "selected-job", event: "cron.job.updated", payload: map[string]any{"uid": uid}, relevant: true},
		{name: "reset", relevant: true},
		{name: "project-cutoff", event: "project.metadata_updated", payload: cronDiff("cron.passover", cutoff), relevant: true},
		{name: "selected-cutoff", event: "project.metadata_updated", payload: cronDiff("cron.passover."+uid, cutoff), relevant: true},
		// Unclassifiable changes must be refused rather than accepted.
		{name: "malformed-metadata", event: "issue.metadata_updated", issue: parent, payload: map[string]any{"diff": nil}, relevant: true},
		{name: "unknown-job", event: "cron.job.updated", relevant: true},
	} {
		for _, confirming := range []bool{false, true} {
			for _, completed := range []bool{false, true} {
				t.Run(tc.name+map[bool]string{false: "/lookup", true: "/confirmation"}[confirming]+map[bool]string{false: "/loading", true: "/loaded"}[completed], func(t *testing.T) {
					m, api := enabledFixture(t, job)
					withEnabledBacklog(api)
					if tc.name == "reopened" && !confirming {
						api.parent.open = false // the closed parent suppressed the old count
					}
					m, lookup := press(m, keyRune('p'))
					reply := lookup().(cronActionMsg)
					if confirming {
						m, _ = m.cronActionAnswered(reply)
						require.Equal(t, modalCronAction, m.modal)
					}
					before := m.cron.events
					if tc.name == "reset" {
						next, _ := m.handleResetRequired(resetRequiredMsg{})
						m = next.(Model)
					} else {
						m = cronEnabledEvent(t, m, tc.event, tc.issue, tc.payload)
					}
					require.Greater(t, m.cron.events, before, "activity must still refresh the cron tabs")
					if completed {
						fresh := *m.cron.data
						fresh.events = m.cron.events
						if tc.relevant {
							// The loaded data now reflects the change; the latch must not depend on it.
							fresh.passovers = map[string]*db.CronPassoverState{uid: {Through: cutoff.Through, Scope: "project"}}
							fresh.parentOpen, fresh.progress = map[string]bool{parent: true}, api.parent.progress
						}
						m = m.cronLoaded(cronLoadedMsg(fresh))
					}
					if !confirming {
						m, _ = m.cronActionAnswered(reply)
					}
					if !tc.relevant {
						require.Equal(t, modalCronAction, m.modal, "unrelated activity must preserve a useful confirmation")
						require.Equal(t, 5, m.cron.action.count)
						m, write := m.routeModalKey(keyRune('y'))
						_, _ = deliverEnabled(t, m, write)
						require.Equal(t, []enabledCall{{uid: uid, expected: job.DefinitionEventUID, actor: "operator", enabled: true}}, api.enabledCalls)
						return
					}
					if confirming {
						m, _ = m.routeModalKey(keyRune('y'))
					}
					require.Nil(t, m.cron.action, "a relevant change must discard the old calculation before a write")
					require.Equal(t, modalNone, m.modal)
					require.Empty(t, api.enabledCalls)
					require.Equal(t, toastError, m.toast.level)
					require.Equal(t, "Job data changed; press p after refresh", m.toast.text)
				})
			}
		}
	}
}

// Admission is a separate gate: restoring the old project-wide freshness
// check here starves actions even though their own lookup and CAS are guarded.
func TestCronEnabledAdmissionDuringBackgroundRefresh(t *testing.T) {
	m, api := enabledFixture(t, pausedNative())
	withEnabledBacklog(api)
	m = cronEnabledEvent(t, m, "cron.run.observed", "", map[string]any{"uid": cronTestIssue})
	require.NotEqual(t, m.cron.data.events, m.cron.events, "the background refresh has not completed")
	m, cmd := press(m, keyRune('p'))
	require.NotNil(t, cmd, "unrelated background refresh must not prevent admission")
	require.NotNil(t, m.cron.action)
	reply, ok := cmd().(cronActionMsg)
	require.True(t, ok)
	require.NoError(t, reply.err)
}
