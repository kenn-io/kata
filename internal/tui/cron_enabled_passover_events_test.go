package tui

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// A counted resume depends on its applicable project/job cutoff. Refreshing
// through completion must not make an older lookup or dialog actionable again.
func TestCronResumePassoverChangesRemainFenced(t *testing.T) {
	for _, stage := range []string{"lookup", "confirmation"} {
		for _, completed := range []bool{false, true} {
			for _, key := range []string{"cron.passover", "selected-job", "other-job", "ordinary", "request"} {
				t.Run(fmt.Sprintf("%s/completed=%t/%s", stage, completed, key), func(t *testing.T) {
					m, api := enabledFixture(t, pausedNative())
					withEnabledBacklog(api)
					m, lookup := press(m, keyRune('p'))
					reply := lookup().(cronEnabledReadyMsg)
					if stage == "confirmation" {
						m, _ = m.cronEnabledReady(reply)
					}
					changed := key
					switch key {
					case "selected-job":
						changed = "cron.passover." + api.shown.UID
					case "other-job":
						changed = "cron.passover." + cronTestIssue
					case "request":
						changed = "cron.request." + cronTestIssue
					}
					cutoff := time.Now().UTC().Add(time.Hour)
					var value any = db.CronPassover{Version: 1, Through: cutoff, By: "another-operator"}
					if key == "request" {
						value = nil
					}
					m = cronEnabledEvent(t, m, "project.metadata_updated", "", map[string]any{"diff": map[string]any{changed: map[string]any{"to": value}}})
					relevant := key == "cron.passover" || key == "selected-job"
					if completed {
						fresh := *m.cron.data
						fresh.events = m.cron.events
						if relevant {
							fresh.passovers = map[string]*db.CronPassoverState{api.shown.UID: {Through: cutoff, Scope: "project"}}
						}
						m = m.cronLoaded(cronLoadedMsg(fresh))
					}
					if stage == "lookup" {
						m, _ = m.cronEnabledReady(reply)
					} else {
						m, _ = m.routeModalKey(keyRune('y'))
					}
					if relevant {
						require.Nil(t, m.cron.enabled, "changed cutoff must cancel the stale action directly")
						require.Equal(t, modalNone, m.modal)
						require.NotNil(t, m.toast)
						require.Equal(t, "Job data changed; press p after refresh", m.toast.text)
					} else {
						require.NotNil(t, m.cron.enabled, "unrelated metadata must retain confirmation")
					}
					require.Empty(t, api.enabledCalls)
				})
			}
		}
	}
}
