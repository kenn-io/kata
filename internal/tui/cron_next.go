package tui

import (
	"encoding/json/v2"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

const cronNextCount = 3

// cronNativeParent returns the parent issue UID of a native recurring job and
// "" for every other job.
func cronNativeParent(j generated.CronJob) string {
	if j.Definition.Action.Kind != "notify" || j.Definition.Issue == nil || j.Definition.Issue.Kind != "per-run" {
		return ""
	}
	return deref(j.Definition.Issue.ParentUID)
}

// cronNextRuns previews when a job runs next using the daemon's own schedule
// step. Native jobs deliver on those instants, so they read "Next". Kata does
// not run an adapter's job: its cron and once instants are the adapter's own
// trigger times and its interval has no instant at all.
func cronNextRuns(p cronData, j generated.CronJob, now time.Time) (label string, lines []string) {
	native := cronNativeParent(j) != ""
	label = "Triggers"
	if native {
		label = "Next"
	}
	if kind := j.Definition.Trigger.Kind; !native && kind != "cron" && kind != "once" || native && kind != "cron" && kind != "interval" {
		return "", nil
	}
	if !j.Definition.Enabled {
		return label, []string{"none while paused"}
	}
	var def cron.JobDefinition
	raw, err := json.Marshal(j.Definition)
	if err != nil || json.Unmarshal(raw, &def) != nil {
		return "", nil
	}
	progress := cron.RecurringProgress{}
	if native {
		// The daemon delivers only while the parent issue is open.
		open, known := p.parentOpen[cronNativeParent(j)]
		var ok bool
		if progress, ok = p.progress[j.UID]; !known || !ok {
			return label, []string{"unavailable"}
		}
		if !open {
			return label, []string{"none while the parent is closed"}
		}
	}
	times, err := cron.RecurringOccurrences(def, progress, now, cronNextCount)
	if err != nil {
		return label, []string{"unavailable"}
	}
	zone := cron.TriggerLocation(def.Trigger)
	for _, t := range times {
		lines = append(lines, t.In(zone).Format("2006-01-02 15:04 MST"))
	}
	if len(lines) == 0 {
		return "", nil
	}
	return label, lines
}
