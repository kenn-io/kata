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
	if !j.Definition.Enabled {
		return cronHealthState{"paused", ""}
	}
	if p.jobsErr != nil {
		return cronHealthState{"unavailable", ""} // the jobs error row says why
	}
	def, err := cronJobDefinition(j)
	if err != nil {
		return cronHealthState{"unavailable", "invalid definition: " + err.Error()}
	}

	progress, base := cron.RecurringProgress{}, j.CreatedAt
	last, hasRun := p.lastRuns[j.UID]
	native := cron.IsRecurringIssueJob(def)
	if native {
		open, known := p.parentOpen[def.Issue.ParentUID]
		if !known {
			return cronHealthState{"unavailable", "parent issue could not be read"}
		}
		if !open {
			return cronHealthState{"parent closed", "delivery stops while the parent is closed or deleted"}
		}
		var knownProgress bool
		progress, knownProgress = p.progress[j.UID]
		if !knownProgress {
			return cronHealthState{"unavailable", "delivery progress is missing or unreadable"}
		}
		base = time.Time{}
	} else {
		if p.lastErr == nil {
			return cronHealthState{"unavailable", "last run has not been read"}
		}
		if err := p.lastErr[j.UID]; err != nil {
			return cronHealthState{"unavailable", "last run: " + err.Error()}
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
					reason := fmt.Sprintf("Timeout exceeded: %ds; started %s", def.TimeoutSeconds, started.Format(time.RFC3339))
					if wait.state == "blocked" {
						reason += "; " + wait.reason
					}
					return cronHealthState{"overdue", reason}
				}
				return wait
			case "failed", "unknown":
				return cronHealthState{"failing", "last run status is " + last.Status}
			}
			if def.Trigger.Kind == "once" && (last.Status == "succeeded" || last.Status == "cancelled") && j.DefinitionEventUID != "" && last.DefinitionEventUID != nil && *last.DefinitionEventUID == j.DefinitionEventUID {
				return cronHealthState{"ok", "One-time job has run under its current definition"}
			}
		} else if def.Trigger.Kind == "once" {
			base = time.Time{}
		}
	}
	instants, err := cron.RecurringOccurrences(def, progress, base, 1)
	if err != nil {
		return cronHealthState{"unavailable", "schedule or progress: " + err.Error()}
	}
	grace := 15 * time.Minute
	if def.GraceSeconds > 0 {
		grace = time.Duration(def.GraceSeconds) * time.Second
	}
	if len(instants) > 0 && instants[0].Add(grace).Before(now) {
		return cronHealthState{"missed", "Expected " + instants[0].Format(time.RFC3339) + "; over " + grace.String() + " late (estimate)"}
	}
	if !native && !hasRun {
		return cronHealthState{"never ran", "No scheduled run is past its grace period"}
	}
	if native {
		return cronHealthState{"ok", "No delivery is past its grace period"}
	}
	if def.Trigger.Kind == "once" && len(instants) == 0 {
		return cronHealthState{"ok", "One-time job has run"}
	}
	return cronHealthState{"ok", "last run status is " + last.Status}
}
