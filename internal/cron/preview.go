package cron

import "time"

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
	if err := def.ValidateVersion(); err != nil {
		return JobPreview{}, err
	}
	if !def.Enabled {
		return JobPreview{Status: "paused"}, nil
	}
	var progress RecurringProgress
	if IsRecurringIssueJob(def) {
		if parent == nil {
			return JobPreview{Status: "unavailable"}, nil
		}
		if !parent.Open {
			return JobPreview{Status: "parent_closed"}, nil
		}
		if parent.Progress == nil {
			return JobPreview{Status: "unavailable"}, nil
		}
		progress = *parent.Progress
	} else if def.Trigger.Kind != "once" {
		if def.Trigger.Kind != "cron" {
			return JobPreview{Status: "unsupported"}, nil
		}
		schedule, err := adapterCronSchedule(def.Trigger)
		if err != nil {
			return JobPreview{}, err
		}
		if schedule == nil {
			return JobPreview{Status: "unsupported"}, nil
		}
	}
	occurrences, err := RecurringOccurrences(def, progress, after, count)
	if err != nil {
		return JobPreview{}, err
	}
	status := "ready"
	if len(occurrences) == 0 {
		status = "exhausted"
	}
	return JobPreview{Status: status, Times: occurrences}, nil
}
