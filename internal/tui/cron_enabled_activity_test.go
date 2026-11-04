package tui

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func cronEnabledEvent(t *testing.T, m Model, kind, issue string, payload any) Model {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": kind, "project_id": m.scope.projectID, "issue_uid": issue, "payload": payload})
	require.NoError(t, err)
	next, _ := m.handleEventReceived(decodeEventReceived(frame{data: raw}))
	return next.(Model)
}

func TestCronResumeSurvivesUnrelatedActivity(t *testing.T) {
	for _, stage := range []string{"lookup", "confirmation"} {
		for _, completed := range []bool{false, true} {
			phase := "loading"
			if completed {
				phase = "loaded"
			}
			for _, kind := range []string{"run", "other-job", "workflow", "parent-comment", "other-checkpoint", "parent-title"} {
				t.Run(stage+"/"+phase+"/"+kind, func(t *testing.T) {
					m, api := enabledFixture(t, pausedNative())
					withEnabledBacklog(api)
					m, cmd := press(m, keyRune('p'))
					require.NotNil(t, cmd)
					reply := cmd().(cronEnabledReadyMsg)
					if stage == "confirmation" {
						m, _ = m.cronEnabledReady(reply)
					}
					before := m.cron.events
					for range 3 {
						event, issue, payload := "cron.run.observed", "", map[string]any{"uid": cronTestIssue, "job_uid": cronTestIssue}
						switch kind {
						case "other-job":
							event = "cron.job.updated"
						case "workflow":
							event = "cron.workflow.updated"
						case "parent-comment":
							event = "issue.commented"
							issue = api.shown.Definition.Issue.ParentUID
						case "other-checkpoint":
							event = "issue.metadata_updated"
							issue = api.shown.Definition.Issue.ParentUID
							payload = map[string]any{"diff": map[string]any{"cron.progress." + cronTestIssue: map[string]any{"to": nil}}}
						case "parent-title":
							event = "issue.updated"
							issue = api.shown.Definition.Issue.ParentUID
							payload = map[string]any{"title": "Updated title"}
						}
						m = cronEnabledEvent(t, m, event, issue, payload)
					}
					require.Greater(t, m.cron.events, before, "unrelated activity must still refresh the cron tabs")
					if completed {
						fresh := *m.cron.data
						fresh.events = m.cron.events
						m = m.cronLoaded(cronLoadedMsg(fresh))
					}
					if stage != "confirmation" {
						m, _ = m.cronEnabledReady(reply)
					}
					require.Equal(t, modalCronResume, m.modal, "unrelated activity must preserve a useful confirmation")
					require.NotNil(t, m.cron.enabled)
					require.Equal(t, 5, m.cron.enabled.count)
					m, write := m.routeModalKey(keyRune('y'))
					require.NotNil(t, m.cron.enabled, "unrelated activity must not discard the action when confirming")
					require.NotNil(t, write)
					_, _ = deliverEnabled(t, m, write)
					require.Equal(t, []enabledCall{{uid: api.shown.UID, expected: api.shown.DefinitionEventUID, actor: "operator", enabled: true}}, api.enabledCalls)
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
	for range 3 {
		m = cronEnabledEvent(t, m, "cron.run.observed", "", map[string]any{"uid": cronTestIssue})
	}
	require.NotEqual(t, m.cron.data.events, m.cron.events, "the background refresh has not completed")
	m, cmd := press(m, keyRune('p'))
	require.NotNil(t, cmd, "unrelated background refresh must not prevent admission")
	require.NotNil(t, m.cron.enabled)
	reply, ok := cmd().(cronEnabledReadyMsg)
	require.True(t, ok)
	require.NoError(t, reply.err)
}

func TestCronResumeRelevantChangesRemainFenced(t *testing.T) {
	for _, stage := range []string{"lookup", "confirmation"} {
		for _, completed := range []bool{false, true} {
			phase := "loading"
			if completed {
				phase = "loaded"
			}
			for _, kind := range []string{"checkpoint", "status", "deleted", "selected-job", "reset", "reopened", "malformed-metadata", "unknown-job"} {
				t.Run(stage+"/"+phase+"/"+kind, func(t *testing.T) {
					m, api := enabledFixture(t, pausedNative())
					withEnabledBacklog(api)
					if kind == "reopened" && stage == "lookup" {
						api.parent.open = false
					}
					m, lookup := press(m, keyRune('p'))
					reply := lookup().(cronEnabledReadyMsg)
					if kind == "reopened" && stage == "lookup" {
						require.Zero(t, reply.request.count, "a closed parent suppressed the old count")
					}
					if stage == "confirmation" {
						m, _ = m.cronEnabledReady(reply)
						require.Equal(t, modalCronResume, m.modal)
						require.Equal(t, 5, m.cron.enabled.count)
					}
					before := m.cron.events
					event, issue, payload := "issue.metadata_updated", api.shown.Definition.Issue.ParentUID, map[string]any{"diff": map[string]any{"cron.progress." + api.shown.UID: map[string]any{"to": nil}}}
					switch kind {
					case "status":
						event = "issue.updated"
						payload = map[string]any{"status": "closed"}
					case "deleted":
						event = "issue.soft_deleted"
					case "selected-job":
						event = "cron.job.updated"
						issue = ""
						payload = map[string]any{"uid": api.shown.UID}
					case "reopened":
						event = "issue.reopened"
					// These deliberately exercise the input guards: dropping either nil-diff
					// or absent-UID refusal would otherwise accept an unclassifiable change.
					case "malformed-metadata":
						payload = map[string]any{"diff": nil}
					case "unknown-job":
						event = "cron.job.updated"
						issue = ""
						payload = nil
					}
					if kind == "reset" {
						next, _ := m.handleResetRequired(resetRequiredMsg{})
						m = next.(Model)
					} else {
						m = cronEnabledEvent(t, m, event, issue, payload)
					}
					require.Greater(t, m.cron.events, before)
					if completed {
						fresh := *m.cron.data
						fresh.events = m.cron.events
						if kind == "reopened" {
							fresh.parentOpen = map[string]bool{api.shown.Definition.Issue.ParentUID: true}
							fresh.progress = api.parent.progress
						}
						m = m.cronLoaded(cronLoadedMsg(fresh))
						require.Equal(t, m.cron.events, m.cron.data.events, "refresh completed")
					} else {
						require.NotEqual(t, m.cron.events, m.cron.data.events, "refresh is pending")
					}
					require.Equal(t, api.shown.DefinitionEventUID, m.cron.data.jobs[0].DefinitionEventUID)
					if stage == "lookup" {
						m, _ = m.cronEnabledReady(reply)
					} else {
						m, _ = m.routeModalKey(keyRune('y'))
					}
					require.Nil(t, m.cron.enabled, "a relevant change must discard the old calculation before a write")
					require.Equal(t, modalNone, m.modal)
					require.Empty(t, api.enabledCalls)
					require.NotNil(t, m.toast)
					require.Equal(t, toastError, m.toast.level)
					require.Equal(t, "Job data changed; press p after refresh", m.toast.text)
				})
			}
		}
	}
}
