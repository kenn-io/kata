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
	}
	if err := def.Validate(); err != nil {
		return JobPreview{}, err
	}
	occurrences, supported, err := recurringOccurrences(def, progress, after, count)
	if err != nil {
		if IsRecurringIssueJob(def) {
			return JobPreview{Status: "unavailable"}, nil
		}
		return JobPreview{}, err
	}
	if !supported {
		return JobPreview{Status: "unsupported"}, nil
	}
	status := "ready"
	if len(occurrences) == 0 {
		status = "exhausted"
	}
	return JobPreview{Status: status, Times: occurrences}, nil
}
