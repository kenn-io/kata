package cron

import (
	"time"

	cronparser "github.com/netresearch/go-cron"
)

// PreviewParent contains the observed parent state and its decoded checkpoint.
type PreviewParent struct {
	Open     bool
	Progress *RecurringProgress
}

// JobPreview is a saved job's next instants and their availability.
type JobPreview struct {
	Status string
	Times  []time.Time
}

// PreviewJob applies the same availability rules to every saved-job preview.
func PreviewJob(def JobDefinition, parent *PreviewParent, after time.Time, count int) (JobPreview, error) {
	status := ""
	var progress RecurringProgress
	if !def.Enabled {
		status = "paused"
	} else if IsRecurringIssueJob(def) {
		if parent == nil || parent.Progress == nil {
			status = "unavailable"
		} else if !parent.Open {
			status = "parent_closed"
		} else {
			progress = *parent.Progress
		}
	} else if def.Trigger.Kind != "once" {
		if def.Trigger.Kind != "cron" {
			status = "unsupported"
		} else if schedule, err := adapterCronSchedule(def.Trigger); err == nil {
			_, _, hasZone, _ := splitCronDirective(def.Trigger.Cron)
			if schedule == nil {
				status = "unsupported"
			} else if parsed, ok := schedule.(*cronparser.SpecSchedule); ok && parsed.Location == time.Local && (hasZone || def.Trigger.Timezone != "") {
				status = "unavailable"
			}
		}
	}
	if status != "" {
		if err := def.ValidateVersion(); err != nil {
			return JobPreview{}, err
		}
		return JobPreview{Status: status}, nil
	}
	occurrences, err := RecurringOccurrences(def, progress, after, count)
	if err != nil {
		return JobPreview{}, err
	}
	status = "ready"
	if len(occurrences) == 0 {
		status = "exhausted"
	}
	return JobPreview{Status: status, Times: occurrences}, nil
}
