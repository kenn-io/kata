package tui

import (
	"fmt"
	"time"

	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

type cronHealthState struct{ state, reason string }

// cronHealth describes the local evidence, not a daemon skip decision. Native
// jobs deliver issues: executor observations cannot describe their health.
func cronHealth(p cronData, j generated.CronJob, now time.Time) cronHealthState {
	state := func(label, reason string) cronHealthState { return cronHealthState{label, reason} }
	if !j.Definition.Enabled {
		return state("paused", "")
	}
	if p.jobsErr != nil {
		return state("unavailable", "job definitions could not be refreshed")
	}
	def, err := cronJobDefinition(j)
	if err != nil {
		return state("unavailable", "invalid definition: "+err.Error())
	}

	progress, base := cron.RecurringProgress{}, j.CreatedAt
	last, hasRun := p.lastRuns[j.UID]
	native := cron.IsRecurringIssueJob(def)
	if native {
		open, known := p.parentOpen[def.Issue.ParentUID]
		if !known {
			return state("unavailable", "parent issue could not be read")
		}
		if !open {
			return state("parent closed", "delivery stops while the parent is closed or deleted")
		}
		var knownProgress bool
		progress, knownProgress = p.progress[j.UID]
		if !knownProgress {
			return state("unavailable", "delivery progress is missing or unreadable")
		}
		base = time.Time{}
	} else {
		if p.lastErr == nil {
			return state("unavailable", "last run has not been read")
		}
		if err := p.lastErr[j.UID]; err != nil {
			return state("unavailable", "last run: "+err.Error())
		}
		if hasRun {
			base = last.CreatedAt
			switch last.Status {
			case "running":
				wait := cronRunHealth(p, last)
				started := last.CreatedAt
				if last.StartedAt != nil {
					started = *last.StartedAt
				}
				if def.TimeoutSeconds > 0 && started.Add(time.Duration(def.TimeoutSeconds)*time.Second).Before(now) {
					reason := fmt.Sprintf("running past timeout_seconds %d (started %s)", def.TimeoutSeconds, started.Format(time.RFC3339))
					if wait.state == "blocked" {
						reason += "; " + wait.reason
					}
					return state("overdue", reason)
				}
				return wait
			case "failed", "unknown":
				return state("failing", "last run status is "+last.Status)
			}
		} else if def.Trigger.Kind == "once" {
			base = time.Time{}
		}
	}
	instants, err := cron.RecurringOccurrences(def, progress, base, 1)
	if err != nil {
		return state("unavailable", "schedule or progress: "+err.Error())
	}
	grace := 15 * time.Minute
	if def.GraceSeconds > 0 {
		grace = time.Duration(def.GraceSeconds) * time.Second
	}
	if len(instants) > 0 && instants[0].Add(grace).Before(now) {
		return state("missed", "Expected "+instants[0].Format(time.RFC3339)+"; over "+grace.String()+" late (estimate)")
	}
	if !native && !hasRun {
		return state("never ran", "No scheduled run is past its grace period")
	}
	if native {
		return state("ok", "No delivery is past its grace period")
	}
	if def.Trigger.Kind == "once" && len(instants) == 0 {
		return state("ok", "One-time job has run")
	}
	return state("ok", "last run status is "+last.Status)
}
